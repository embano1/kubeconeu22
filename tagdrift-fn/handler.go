package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"strings"
	"time"

	ce "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/protocol/http"
	"github.com/embano1/vsphere/client"
	"github.com/embano1/vsphere/logger"
	"github.com/kelseyhightower/envconfig"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"
)

func eventhandler(vc *client.Client) func(ctx context.Context, event ce.Event) error {
	var cfg config
	if err := envconfig.Process("", &cfg); err != nil {
		panic("process environment variables: " + err.Error())
	}

	category := cfg.Category
	slack := nethttp.Client{
		Timeout: time.Second * 5,
	}

	return func(ctx context.Context, event ce.Event) error {
		log := logger.Get(ctx).With(zap.String("eventID", event.ID()))
		log.Debug("received event", zap.Any("event", event))

		var vevent types.VmMigratedEvent // also works for DrsVmMigratedEvent
		if err := event.DataAs(&vevent); err != nil {
			log.Error("could not marshal event to VmMigratedEvent", zap.Error(err))
			return http.NewResult(nethttp.StatusBadRequest,
				"could not marshal cloudevent event data to vsphere event (eventID: %d)",
				event.ID(),
			)
		}

		src := vevent.SourceHost
		if src.Name == "" {
			log.Error("invalid event", zap.Error(errors.New("empty source host name")))
			return http.NewResult(nethttp.StatusBadRequest,
				"vsphere event did not contain source host name (eventID: %d)",
				event.ID(),
			)
		}

		host := vevent.Host
		if host == nil || host.Name == "" {
			log.Error("invalid event", zap.Error(errors.New("empty current host name")))
			return http.NewResult(nethttp.StatusBadRequest,
				"vsphere event did not contain current host name (eventID: %d)",
				event.ID(),
			)
		}

		vm := vevent.Vm
		if vm == nil || vm.Name == "" {
			log.Error("invalid event", zap.Error(errors.New("empty vm name")))
			return http.NewResult(nethttp.StatusBadRequest,
				"vsphere event did not contain vm name (eventID: %d)",
				event.ID(),
			)
		}

		zoneTags, err := vc.Tags.GetTagsForCategory(ctx, category)
		if err != nil {
			log.Error(
				"could not retrieve tags for category",
				zap.String("category", category),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not get tags for category %q (eventID: %d)",
				category,
				event.ID(),
			)
		}

		vmTags, err := vc.Tags.ListAttachedTags(ctx, vm.Vm)
		if err != nil {
			log.Error(
				"could not retrieve tags for vm",
				zap.String("vm", vm.Name),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not get tags for vm %q (eventID: %d)",
				vm.Name,
				event.ID(),
			)
		}

		var zone tags.Tag
		match := false
	LOOP:
		// does vm have any zone tag?
		for _, vmTag := range vmTags {
			for _, v := range zoneTags {
				if v.ID == vmTag {
					match = true
					zone = v
					break LOOP
				}
			}
		}

		if !match {
			log.Debug(
				"ignoring vm: did not contain matching zonal tag",
				zap.String("vm", vm.Name),
				zap.Any("zoneTags", zoneTags),
				zap.Any("vmTags", vmTags),
			)
			return nil
		}

		log.Debug("found zone assigment for vm",
			zap.String("vm", vm.Name),
			zap.String("zone", zone.Name),
		)

		// retrieve tags for current vm host
		hostTags, err := vc.Tags.ListAttachedTags(ctx, host.Host)
		if err != nil {
			log.Error(
				"could not retrieve tags for host",
				zap.String("host", host.Name),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not retrieve tags for host %q (eventID: %d)",
				host.Name,
				event.ID(),
			)
		}

		hostZone := "(n/a)"
		inSync := false
		for _, t := range hostTags {
			if v, ok := contains(zoneTags, t); ok {
				hostZone = v
			}

			if t == zone.ID {
				inSync = true
				break
			}
		}

		if inSync {
			log.Debug("vm and host running in same zone",
				zap.String("vm", vm.Name),
				zap.String("host", host.Name),
				zap.String("zone", zone.Name),
			)
			return nil
		}

		log.Info("vm not running on a host matching desired zone",
			zap.String("vm", vm.Name),
			zap.String("host", host.Name),
			zap.String("vmZone", zone.Name),
			zap.String("hostZone", hostZone),
		)

		data := fmt.Sprintf(slackMessage, vm.Name, src.Name, host.Name, event.Subject(), zone.Name, hostZone)
		req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, cfg.SlackToken, strings.NewReader(data))
		if err != nil {
			log.Error("could not create slack request", zap.Error(err))
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not create slack request (eventID: %d)",
				event.ID(),
			)
		}
		req.Header.Add("Content-Type", "application/json; charset=utf-8")

		// TODO: add retry logic
		resp, err := slack.Do(req)
		if err != nil {
			log.Error(
				"could not send slack message",
				zap.Int("statusCode", resp.StatusCode),
				zap.String("payload", data),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not send slack message (eventID: %d)",
				event.ID(),
			)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Error(
				"could not read slack response body",
				zap.Int("statusCode", resp.StatusCode),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not read slack response body (eventID: %d)",
				event.ID(),
			)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			log.Error(
				"could not send slack message",
				zap.Int("statusCode", resp.StatusCode),
				zap.String("errorMessage", string(body)),
				zap.String("payload", data),
				zap.Error(err),
			)
			return http.NewResult(nethttp.StatusInternalServerError,
				"could not send slack message (eventID: %d)",
				event.ID(),
			)
		}

		return nil
	}
}

// contains checks if the given tag id is in tags and returns the tag name and
// true if found
func contains(tags []tags.Tag, id string) (string, bool) {
	for _, tag := range tags {
		if tag.ID == id {
			return tag.Name, true
		}
	}

	return "", false
}

const slackMessage = `{
	"blocks": [
		{
			"type": "section",
			"text": {
				"type": "mrkdwn",
				"text": ":warning: A virtual machine is *not running* in its preferred zone."
			}
		},
		{
			"type": "divider"
		},
		{
			"type": "section",
			"text": {
				"type": "mrkdwn",
				"text": "Virtual machine %s migrated from host %s to host %s.\n\n *Event reason:* %s\n*VM zone:* %s\n*Host zone:* %s"
			}
		}
	]
}`
