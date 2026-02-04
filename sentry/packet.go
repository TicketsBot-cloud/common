package sentry

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/TicketsBot-cloud/gdl/rest/request"
	"github.com/getsentry/sentry-go"
)

func constructErrorPacket(e error, tags map[string]string) *sentry.Event {
	return constructPacket(e, sentry.LevelError, tags)
}

// getErrorTypeName returns the type name of an error for better Sentry grouping
func getErrorTypeName(e error) string {
	t := reflect.TypeOf(e)
	if t == nil {
		return "error"
	}

	// Get the full type name including package
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
		return "*" + t.PkgPath() + "." + t.Name()
	}

	if t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}

	return t.String()
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

	// Extract user context from tags if present
	var user sentry.User
	if guildId, ok := tags["guild"]; ok {
		user.ID = fmt.Sprintf("guild:%s", guildId)
		user.Data = map[string]string{"guild_id": guildId}
		delete(tags, "guild")
	}
	if userId, ok := tags["user"]; ok {
		user.ID = fmt.Sprintf("user:%s", userId)
		if user.Data == nil {
			user.Data = make(map[string]string)
		}
		user.Data["user_id"] = userId
		delete(tags, "user")
	}

	return &sentry.Event{
		Message:    e.Error(),
		Extra:      extra,
		Timestamp:  time.Now(),
		Level:      level,
		ServerName: hostname,
		Tags:       tags,
		User:       user,
		Exception: []sentry.Exception{
			{
				Type:       getErrorTypeName(e),
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
