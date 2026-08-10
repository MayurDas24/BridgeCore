// Package logger provides a single, application-wide structured logger
// built on top of Uber's zap. Every other package receives a *zap.Logger
// (or SugaredLogger) rather than constructing its own, so log output stays
// consistent and centrally configurable.
package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New builds a zap.Logger appropriate for the given environment.
// "production" gets JSON output tuned for log aggregators (Datadog, ELK,
// Loki); anything else gets a human-readable console encoder.
func New(env string) (*zap.Logger, error) {
	var cfg zap.Config

	if env == "production" {
		cfg = zap.NewProductionConfig()
		cfg.EncoderConfig.TimeKey = "timestamp"
		cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	} else {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stderr"}

	logger, err := cfg.Build(zap.AddCallerSkip(0))
	if err != nil {
		return nil, err
	}

	return logger.With(zap.String("service", "bridgecore-api")), nil
}
