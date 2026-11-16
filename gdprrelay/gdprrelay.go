package gdprrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TicketsBot-cloud/database"
	"github.com/go-redis/redis/v8"
)

type RequestType int

const (
	RequestTypeAllTranscripts RequestType = iota
	RequestTypeSpecificTranscripts
	RequestTypeAllMessages
	RequestTypeSpecificMessages
	RequestTypeExportGuild
	RequestTypeExportUser
)

var LogNames = map[RequestType]string{
	RequestTypeAllTranscripts:      "AllTranscripts",
	RequestTypeSpecificTranscripts: "SpecificTranscripts",
	RequestTypeAllMessages:         "AllMessages",
	RequestTypeSpecificMessages:    "SpecificMessages",
	RequestTypeExportGuild:         "ExportGuild",
	RequestTypeExportUser:          "ExportUser",
}

var ExportTypes = map[RequestType]database.GdprExportType{
	RequestTypeExportGuild: database.GdprExportTypeServer,
	RequestTypeExportUser:  database.GdprExportTypeUser,
}

type GDPRRequest struct {
	Type               RequestType       `json:"type"`
	UserId             uint64            `json:"user_id"`
	GuildIds           []uint64          `json:"guild_ids,omitempty"`
	GuildNames         map[uint64]string `json:"guild_names,omitempty"`
	TicketIds          []int             `json:"ticket_ids,omitempty"`
	Categories         []string          `json:"categories,omitempty"`
	ExportId           int64             `json:"export_id,omitempty"`
	Source             string            `json:"source,omitempty"`
	Language           string            `json:"language,omitempty"`
	InteractionToken   string            `json:"interaction_token,omitempty"`
	InteractionGuildId uint64            `json:"interaction_guild_id,omitempty"`
	ApplicationId      uint64            `json:"application_id,omitempty"`
}

const SourceDashboard = "dashboard"

type QueuedRequest struct {
	Request            GDPRRequest `json:"request"`
	QueuedAt           time.Time   `json:"queued_at"`
	RetryCount         int         `json:"retry_count"`
	LastAttemptAt      time.Time   `json:"last_attempt_at,omitempty"`
	RequestID          int         `json:"request_id"`
	TranscriptsDeleted int         `json:"transcripts_deleted,omitempty"`
	MessagesDeleted    int         `json:"messages_deleted,omitempty"`
}

const (
	KeyPending       = "tickets:gdpr:pending"
	KeyHeartbeat     = "tickets:gdpr:worker:heartbeat"
	KeyNotifications = "tickets:gdpr:notifications"
)

func IsWorkerAlive(redisClient *redis.Client) bool {
	val, err := redisClient.Get(context.Background(), KeyHeartbeat).Result()
	return err == nil && val != ""
}

func Publish(ctx context.Context, redisClient *redis.Client, data GDPRRequest, logId int) error {
	marshalled, err := json.Marshal(QueuedRequest{
		Request:   data,
		QueuedAt:  time.Now(),
		RequestID: logId,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal GDPR request: %w", err)
	}

	return redisClient.LPush(ctx, KeyPending, string(marshalled)).Err()
}

// Queue returns a non-nil next, and queues nothing, while the 24-hour export limit blocks the request.
func Queue(ctx context.Context, redisClient *redis.Client, db *database.Database, req GDPRRequest) (logId int, next *time.Time, err error) {
	if exportType, ok := ExportTypes[req.Type]; ok {
		export, next, err := db.GdprExports.CreateQueued(ctx, database.GdprExport{
			Type:        exportType,
			RequesterId: req.UserId,
			GuildIds:    req.GuildIds,
			Categories:  req.Categories,
			TicketIds:   req.TicketIds,
		}, LogNames[req.Type])
		if err != nil || next != nil {
			return 0, next, err
		}

		req.ExportId, logId = export.Id, export.GdprLogId
	} else if logId, err = db.GdprLogs.InsertLog(database.HashGdprRequester(req.UserId), LogNames[req.Type], database.GdprLogStatusQueued); err != nil {
		return 0, nil, err
	}

	if err := Publish(ctx, redisClient, req, logId); err != nil {
		// A fresh context, as a row left queued would block the user's next export for 24 hours.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if req.ExportId != 0 {
			err = errors.Join(err, db.GdprExports.SetStatus(cleanupCtx, req.ExportId, database.GdprExportStatusFailed))
		}

		return logId, nil, errors.Join(err, db.GdprLogs.UpdateLogStatus(logId, database.GdprLogStatusFailed))
	}

	return logId, nil, nil
}

const (
	NotificationStatusCompleted = "completed"
	NotificationStatusNoData    = "no_data"
	NotificationStatusFailed    = "failed"
)

type Notification struct {
	UserId      uint64      `json:"user_id"`
	RequestId   int         `json:"request_id"`
	RequestType RequestType `json:"request_type"`
	Status      string      `json:"status"`
	ExportId    int64       `json:"export_id,omitempty"`
	Error       string      `json:"error,omitempty"`
	Count       int         `json:"count"`
}

func Notify(ctx context.Context, redisClient *redis.Client, notification Notification) error {
	marshalled, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal GDPR notification: %w", err)
	}

	return redisClient.RPush(ctx, KeyNotifications, string(marshalled)).Err()
}
