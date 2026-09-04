package observability

import (
	"fmt"
	"os"

	"github.com/getsentry/sentry-go"
	"go.uber.org/zap/zapcore"
)

type Environment string

func (e Environment) String() string {
	return string(e)
}

const (
	EnvironmentProduction  Environment = "production"
	EnvironmentStaging     Environment = "staging"
	EnvironmentDevelopment Environment = "development"
)

// ZapSentryAdapter forwards error-level logs to Sentry. A Core, not a hook: hooks
// receive a bare zapcore.Entry, which carries no fields, so they drop them all.
func ZapSentryAdapter(environment Environment) func(core zapcore.Core) zapcore.Core {
	return func(core zapcore.Core) zapcore.Core {
		return &sentryCore{Core: core, environment: environment}
	}
}

type sentryCore struct {
	zapcore.Core
	environment Environment
	fields      []zapcore.Field
}

func (c *sentryCore) With(fields []zapcore.Field) zapcore.Core {
	combined := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	combined = append(combined, c.fields...)
	combined = append(combined, fields...)

	return &sentryCore{
		Core:        c.Core.With(fields),
		environment: c.environment,
		fields:      combined,
	}
}

// Must add this core, not the wrapped one, or zap bypasses Write.
func (c *sentryCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return ce.AddCore(entry, c)
	}

	return ce
}

func (c *sentryCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	if entry.Level == zapcore.ErrorLevel {
		c.capture(entry, fields)
	}

	return c.Core.Write(entry, fields)
}

func (c *sentryCore) capture(entry zapcore.Entry, fields []zapcore.Field) {
	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range c.fields {
		field.AddTo(encoder)
	}
	for _, field := range fields {
		field.AddTo(encoder)
	}

	extra := make(map[string]any, len(encoder.Fields)+2)
	for key, value := range encoder.Fields {
		extra[key] = value
	}

	extra["caller"] = entry.Caller.String()
	if entry.Stack != "" {
		extra["stack"] = entry.Stack
	}

	exceptionType := entry.LoggerName
	if exceptionType == "" {
		exceptionType = "error"
	}

	// The error identifies the fault; the message is often generic.
	value := entry.Message
	if err, ok := encoder.Fields["error"]; ok {
		value = fmt.Sprintf("%s: %v", entry.Message, err)
	}

	hostname, _ := os.Hostname()

	sentry.CaptureEvent(&sentry.Event{
		Environment: c.environment.String(),
		Extra:       extra,
		Level:       sentry.LevelError,
		Message:     entry.Message,
		ServerName:  hostname,
		Timestamp:   entry.Time,
		Logger:      entry.LoggerName,
		Exception: []sentry.Exception{
			{
				Type:  exceptionType,
				Value: value,
			},
		},
	})
}
