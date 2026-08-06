package featureflags

import (
	"context"
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The GrowthBook SDK logs through log/slog, while every service here logs through
// zap. Bridging the two keeps SDK diagnostics in the same structured stream
// Promtail already scrapes, which matters because the failure we most need to see
// is a silently dead data source connection.
type slogAdapter struct {
	logger *zap.Logger
	fields []zap.Field
	group  string
}

func newSlogAdapter(logger *zap.Logger) *slog.Logger {
	return slog.New(&slogAdapter{logger: logger.Named("growthbook")})
}

func (s *slogAdapter) Enabled(_ context.Context, level slog.Level) bool {
	return s.logger.Core().Enabled(zapLevel(level))
}

func (s *slogAdapter) Handle(_ context.Context, record slog.Record) error {
	fields := make([]zap.Field, 0, len(s.fields)+record.NumAttrs())
	fields = append(fields, s.fields...)

	record.Attrs(func(attr slog.Attr) bool {
		fields = appendAttr(fields, s.group, attr)
		return true
	})

	if entry := s.logger.Check(zapLevel(record.Level), record.Message); entry != nil {
		entry.Write(fields...)
	}

	return nil
}

func (s *slogAdapter) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := &slogAdapter{
		logger: s.logger,
		group:  s.group,
		fields: make([]zap.Field, len(s.fields), len(s.fields)+len(attrs)),
	}
	copy(next.fields, s.fields)

	for _, attr := range attrs {
		next.fields = appendAttr(next.fields, s.group, attr)
	}

	return next
}

func (s *slogAdapter) WithGroup(name string) slog.Handler {
	if name == "" {
		return s
	}

	group := name
	if s.group != "" {
		group = s.group + "." + name
	}

	next := &slogAdapter{
		logger: s.logger,
		group:  group,
		fields: make([]zap.Field, len(s.fields)),
	}
	copy(next.fields, s.fields)

	return next
}

// appendAttr flattens a slog attribute into zap fields, joining group names with
// dots since zap fields are a flat namespace.
func appendAttr(fields []zap.Field, group string, attr slog.Attr) []zap.Field {
	if attr.Equal(slog.Attr{}) {
		return fields
	}

	key := attr.Key
	if group != "" {
		key = group + "." + key
	}

	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		for _, nested := range value.Group() {
			fields = appendAttr(fields, key, nested)
		}

		return fields
	}

	return append(fields, zap.Any(key, value.Any()))
}

func zapLevel(level slog.Level) zapcore.Level {
	switch {
	case level >= slog.LevelError:
		return zapcore.ErrorLevel
	case level >= slog.LevelWarn:
		return zapcore.WarnLevel
	case level >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}
