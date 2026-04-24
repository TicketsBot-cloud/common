package workflowbus

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kversion"
	"go.uber.org/zap"
)

// Producer is a fire-and-forget Kafka producer for automation trigger events.
// It owns its own kgo.Client so it can be used from service modes that do not run
// a full RPC consumer (for example the INTERACTIONS worker and the dashboard).
type Producer struct {
	client *kgo.Client
	logger *zap.Logger
	signer *Signer
}

// NewProducer returns a Producer. If brokers is empty the returned Producer is a
// no-op — safe to call Emit on, but nothing is actually sent. This lets local
// development and tests run without Kafka configured.
//
// Pass signer=nil (or a Signer built from an empty secret) to skip HMAC signing;
// envelopes will travel without a Signature field. Consumers with verification
// disabled will accept them; consumers in strict mode will reject.
func NewProducer(brokers []string, logger *zap.Logger, signer *Signer) (*Producer, error) {
	if len(brokers) == 0 {
		return &Producer{logger: logger, signer: signer}, nil
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ProducerLinger(10*time.Millisecond),
		kgo.AllowAutoTopicCreation(),
		// Cap the API versions to ones Kafka 3.7.x supports. franz-go defaults to
		// probing its own latest, which for v1.18 includes METADATA v13 (3.8+).
		// Our broker is pinned at 3.7.2 and rejects v13, closing the socket.
		kgo.MaxVersions(kversion.V3_7_0()),
	)
	if err != nil {
		return nil, err
	}

	return &Producer{client: client, logger: logger, signer: signer}, nil
}

// Close flushes and disconnects the underlying client.
func (p *Producer) Close() {
	if p == nil || p.client == nil {
		return
	}
	p.client.Close()
}

// Emit sends a trigger event. Never blocks the caller on network I/O; errors are logged.
// causationId is optional — pass "" to have one generated.
//
// The caller's ctx is intentionally NOT forwarded to the Kafka Produce call. Emit
// is fire-and-forget from the Discord handler's point of view, and the handler's
// context is typically cancelled the moment the interaction finishes responding —
// which can happen before the producer's linger window (10ms) flushes. Propagating
// that cancellation into Produce would kill the in-flight record with "context
// canceled". Instead we use a bounded background context so the producer gets a
// real chance to flush, and the caller's cancellation doesn't leak into delivery.
func (p *Producer) Emit(_ context.Context, triggerType string, guildId uint64, causationId string, payload any) {
	if p == nil || p.client == nil {
		return
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		if p.logger != nil {
			p.logger.Error("workflowbus: failed to marshal trigger payload", zap.String("trigger", triggerType), zap.Error(err))
		}
		return
	}

	if causationId == "" {
		causationId = uuid.NewString()
	}

	env := Envelope{
		Version:     EnvelopeVersion,
		TriggerType: triggerType,
		GuildId:     guildId,
		CausationId: causationId,
		OccurredAt:  time.Now().UTC(),
		Payload:     payloadBytes,
	}

	// Sign before marshal so the signature lands in the on-wire JSON. No-op if
	// the signer is inactive (secret unset) — envelope goes out unsigned, which
	// the executor will accept unless running in strict mode.
	if err := p.signer.Sign(&env); err != nil {
		if p.logger != nil {
			p.logger.Error("workflowbus: failed to sign envelope", zap.Error(err))
		}
		return
	}

	envBytes, err := json.Marshal(env)
	if err != nil {
		if p.logger != nil {
			p.logger.Error("workflowbus: failed to marshal envelope", zap.Error(err))
		}
		return
	}

	produceCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	p.client.Produce(produceCtx, &kgo.Record{
		Topic: TopicWorkflowTriggers,
		Value: envBytes,
		Key:   []byte(uuidToBytes(guildId)),
	}, func(_ *kgo.Record, err error) {
		cancel()
		if err != nil && p.logger != nil {
			p.logger.Error("workflowbus: produce failed", zap.String("trigger", triggerType), zap.Uint64("guild_id", guildId), zap.Error(err))
		}
	})
}

// uuidToBytes partitions by guildId so a given guild's triggers land on one partition,
// preserving ordering per guild.
func uuidToBytes(guildId uint64) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte{
		byte(guildId), byte(guildId >> 8), byte(guildId >> 16), byte(guildId >> 24),
		byte(guildId >> 32), byte(guildId >> 40), byte(guildId >> 48), byte(guildId >> 56),
	}).String()
}

// --- Global convenience ---

var (
	globalProducer *Producer
	globalMu       sync.RWMutex
)

// SetGlobal registers a package-level Producer accessible via Emit.
// Services call this once at startup.
func SetGlobal(p *Producer) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalProducer = p
}

// Emit sends a trigger event via the global Producer (if one has been registered).
// If no producer is registered, Emit is a silent no-op — callers from Discord event
// handlers can therefore invoke this unconditionally without guarding for startup ordering.
func Emit(ctx context.Context, triggerType string, guildId uint64, causationId string, payload any) {
	globalMu.RLock()
	p := globalProducer
	globalMu.RUnlock()
	if p == nil {
		return
	}
	p.Emit(ctx, triggerType, guildId, causationId, payload)
}
