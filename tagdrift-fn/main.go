package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	ce "github.com/cloudevents/sdk-go/v2"
	"github.com/cloudevents/sdk-go/v2/protocol/http"
	vsphere "github.com/embano1/vsphere/client"
	"github.com/embano1/vsphere/logger"
	"github.com/kelseyhightower/envconfig"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var buildCommit = "unknown"

type config struct {
	// cloudevents listener
	Port  int  `envconfig:"PORT" default:"8080"` // knative injected
	Debug bool `envconfig:"DEBUG" default:"false"`

	// vCenter config
	vsphere.Config
	Category string `envconfig:"CATEGORY" default:"k8s-zone"`

	// 	secrets
	SlackToken string `envconfig:"SLACK_TOKEN" required:"true"`
}

func main() {
	var cfg config
	if err := envconfig.Process("", &cfg); err != nil {
		panic("process environment variables: " + err.Error())
	}

	log, err := getLogger(cfg.Debug)
	if err != nil {
		panic("create logger: " + err.Error())
	}
	log = log.Named("tagdrift")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx = logger.Set(ctx, log)

	vc, err := vsphere.New(ctx)
	if err != nil {
		log.Fatal("create vsphere client", zap.Error(err))
	}

	log.Info("starting tagdrift function",
		zap.Int("listenPort", cfg.Port),
		zap.Bool("debug", cfg.Debug),
	)

	ceClient, err := ce.NewClientHTTP(http.WithPort(cfg.Port))
	if err != nil {
		log.Fatal("create cloudevents client", zap.Error(err))
	}

	if err = ceClient.StartReceiver(ctx, eventhandler(vc)); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal("could not run tagdrift function", zap.Error(err))
	}
	log.Info("shutdown complete")
}

func getLogger(debug bool) (*zap.Logger, error) {
	fields := []zap.Field{
		zap.String("commit", buildCommit),
	}

	var cfg zap.Config
	if debug {
		cfg = zap.NewDevelopmentConfig()
	} else {
		cfg = zap.NewProductionConfig()
		cfg.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	}

	log, err := cfg.Build(zap.Fields(fields...))
	if err != nil {
		return nil, err
	}

	return log, nil
}
