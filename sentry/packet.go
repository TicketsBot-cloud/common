package sentry

import (
	"os"
	"time"

	"github.com/TicketsBot-cloud/gdl/rest/request"
	"github.com/getsentry/sentry-go"
)

func constructErrorPacket(e error, tags map[string]string) *sentry.Event {
	return constructPacket(e, sentry.LevelError, tags)
}

func constructPacket(e error, level sentry.Level, tags map[string]string) *sentry.Event {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "null"
	}

	extra := map[string]interface{}{}

	if restError, ok := e.(request.RestError); ok {
		extra["status_code"] = restError.StatusCode
		extra["message"] = restError.Error()
		extra["url"] = restError.Url
		extra["raw"] = string(restError.Raw)
	}

	// Skip 4 frames: runtime.Callers, NewStacktrace, constructPacket, constructErrorPacket/Error/ErrorWithContext
	stacktrace := sentry.NewStacktrace()
	if stacktrace != nil && len(stacktrace.Frames) > 4 {
		stacktrace.Frames = stacktrace.Frames[:len(stacktrace.Frames)-4]
	}

	return &sentry.Event{
		Message:   e.Error(),
		Extra:     extra,
		Timestamp: time.Now(),
		Level:     level,
		ServerName: hostname,
		Tags:      tags,
		Exception: []sentry.Exception{
			{
				Type:       e.Error(),
				Value:      e.Error(),
				Stacktrace: stacktrace,
			},
		},
	}
}

func constructLogPacket(msg string, extra map[string]interface{}, tags map[string]string) *sentry.Event {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "null"
	}

	return &sentry.Event{
		Message:    msg,
		Extra:      extra,
		Timestamp:  time.Now(),
		Level:      sentry.LevelInfo,
		ServerName: hostname,
		Tags:       tags,
	}
}
