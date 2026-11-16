package gdprrelay

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/TicketsBot-cloud/database"
	"github.com/go-redis/redis/v8"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/stretchr/testify/require"
)

// The golden strings are the JSON the bot, dashboard and gdpr-worker exchanged before this package existed.
func TestWireFormat(t *testing.T) {
	queuedAt := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	full := GDPRRequest{
		Type:               RequestTypeExportGuild,
		UserId:             1,
		GuildIds:           []uint64{2, 3},
		GuildNames:         map[uint64]string{2: "a"},
		TicketIds:          []int{4},
		Categories:         []string{"settings"},
		ExportId:           5,
		Source:             SourceDashboard,
		Language:           "en-GB",
		InteractionToken:   "tok",
		InteractionGuildId: 6,
		ApplicationId:      7,
	}
	fullJSON := `{"type":4,"user_id":1,"guild_ids":[2,3],"guild_names":{"2":"a"},"ticket_ids":[4],"categories":["settings"],"export_id":5,"source":"dashboard","language":"en-GB","interaction_token":"tok","interaction_guild_id":6,"application_id":7}`

	tests := []struct {
		name   string
		value  any
		golden string
	}{
		{"request", full, fullJSON},
		{"minimal request", GDPRRequest{UserId: 1}, `{"type":0,"user_id":1}`},
		{"published", QueuedRequest{Request: GDPRRequest{UserId: 1}, QueuedAt: queuedAt, RequestID: 2},
			`{"request":{"type":0,"user_id":1},"queued_at":"2026-11-16T10:00:00Z","retry_count":0,"last_attempt_at":"0001-01-01T00:00:00Z","request_id":2}`},
		{"retried", QueuedRequest{Request: full, QueuedAt: queuedAt, RetryCount: 1, LastAttemptAt: queuedAt.Add(time.Minute), RequestID: 8, TranscriptsDeleted: 9, MessagesDeleted: 10},
			`{"request":` + fullJSON + `,"queued_at":"2026-11-16T10:00:00Z","retry_count":1,"last_attempt_at":"2026-11-16T10:01:00Z","request_id":8,"transcripts_deleted":9,"messages_deleted":10}`},
		{"notification", Notification{UserId: 1, RequestId: 2, RequestType: RequestTypeExportUser, Status: NotificationStatusCompleted, ExportId: 3, Error: "e", Count: 4},
			`{"user_id":1,"request_id":2,"request_type":5,"status":"completed","export_id":3,"error":"e","count":4}`},
		{"minimal notification", Notification{UserId: 1, Status: NotificationStatusNoData},
			`{"user_id":1,"request_id":0,"request_type":0,"status":"no_data","count":0}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			marshalled, err := json.Marshal(tc.value)
			require.NoError(t, err)
			require.Equal(t, tc.golden, string(marshalled))

			decoded := reflect.New(reflect.TypeOf(tc.value))
			require.NoError(t, json.Unmarshal([]byte(tc.golden), decoded.Interface()))
			require.Equal(t, tc.value, decoded.Elem().Interface())
		})
	}
}

func TestQueueIntegration(t *testing.T) {
	dsn, redisUrl := os.Getenv("GDPR_TEST_DATABASE_URL"), os.Getenv("GDPR_TEST_REDIS_URL")
	if dsn == "" || redisUrl == "" {
		t.Skip("GDPR_TEST_DATABASE_URL or GDPR_TEST_REDIS_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.Connect(ctx, dsn)
	require.NoError(t, err)
	defer pool.Close()

	db := database.NewDatabase(pool)
	for _, schema := range []string{db.GdprLogs.Schema(), db.GdprExports.Schema()} {
		_, err := pool.Exec(ctx, schema)
		require.NoError(t, err)
	}

	opts, err := redis.ParseURL(redisUrl)
	require.NoError(t, err)
	rc := redis.NewClient(opts)
	defer rc.Close()
	require.NoError(t, rc.Del(ctx, KeyPending).Err())
	t.Cleanup(func() { rc.Del(context.Background(), KeyPending) })

	pop := func(t *testing.T) QueuedRequest {
		t.Helper()
		raw, err := rc.RPop(ctx, KeyPending).Result()
		require.NoError(t, err)
		var queued QueuedRequest
		require.NoError(t, json.Unmarshal([]byte(raw), &queued))
		return queued
	}

	logRow := func(t *testing.T, id int) (requester, requestType, status string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT requester, request_type, status FROM gdpr_logs WHERE id = $1`, id).Scan(&requester, &requestType, &status))
		return
	}

	exportStatus := func(t *testing.T, logId int) (status database.GdprExportStatus) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM gdpr_exports WHERE gdpr_log_id = $1`, logId).Scan(&status))
		return
	}

	t.Run("log request", func(t *testing.T) {
		userId := uint64(rand.Int64())
		req := GDPRRequest{Type: RequestTypeAllTranscripts, UserId: userId, GuildIds: []uint64{1}, Source: SourceDashboard}

		logId, next, err := Queue(ctx, rc, db, req)
		require.NoError(t, err)
		require.Nil(t, next)

		queued := pop(t)
		require.Equal(t, logId, queued.RequestID)
		require.Equal(t, req, queued.Request)

		requester, requestType, status := logRow(t, logId)
		require.Equal(t, []string{database.HashGdprRequester(userId), "AllTranscripts", database.GdprLogStatusQueued}, []string{requester, requestType, status})
	})

	t.Run("export request and limit", func(t *testing.T) {
		userId := uint64(rand.Int64())
		req := GDPRRequest{Type: RequestTypeExportUser, UserId: userId, Categories: []string{database.GdprExportCategoryTranscripts}}

		logId, next, err := Queue(ctx, rc, db, req)
		require.NoError(t, err)
		require.Nil(t, next)

		queued := pop(t)
		require.Equal(t, logId, queued.RequestID)
		export, err := db.GdprExports.Get(ctx, queued.Request.ExportId)
		require.NoError(t, err)
		require.Equal(t, database.GdprExportTypeUser, export.Type)
		require.Equal(t, logId, export.GdprLogId)
		require.Equal(t, database.GdprExportStatusQueued, export.Status)

		_, requestType, _ := logRow(t, logId)
		require.Equal(t, "ExportUser", requestType)

		blockedId, next, err := Queue(ctx, rc, db, req)
		require.NoError(t, err)
		require.Zero(t, blockedId)
		require.NotNil(t, next)
		require.Zero(t, rc.LLen(ctx, KeyPending).Val())
	})

	t.Run("publish failure marks the rows failed", func(t *testing.T) {
		broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: time.Second})
		defer broken.Close()
		userId := uint64(rand.Int64())

		logId, next, err := Queue(ctx, broken, db, GDPRRequest{Type: RequestTypeExportGuild, UserId: userId, GuildIds: []uint64{1}, Categories: []string{"settings"}})
		require.Error(t, err)
		require.Nil(t, next)
		_, _, status := logRow(t, logId)
		require.Equal(t, database.GdprLogStatusFailed, status)
		require.Equal(t, database.GdprExportStatusFailed, exportStatus(t, logId))

		nextAllowed, err := db.GdprExports.NextAllowedAt(ctx, userId, database.GdprExportTypeServer)
		require.NoError(t, err)
		require.Nil(t, nextAllowed)

		logId, _, err = Queue(ctx, broken, db, GDPRRequest{Type: RequestTypeAllMessages, UserId: userId, GuildIds: []uint64{1}})
		require.Error(t, err)
		_, _, status = logRow(t, logId)
		require.Equal(t, database.GdprLogStatusFailed, status)
	})
}
