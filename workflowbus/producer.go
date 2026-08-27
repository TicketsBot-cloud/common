package workflowbus

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const maxLenApproxWorkflows int64 = 50000

type Producer struct {
	redis  *redis.Client
	logger *zap.Logger
	signer *Signer
}

func NewProducer(redisClient *redis.Client, logger *zap.Logger, signer *Signer) (*Producer, error) {
	if redisClient == nil {
		return &Producer{logger: logger, signer: signer}, nil
	}

	return &Producer{redis: redisClient, logger: logger, signer: signer}, nil
}

func (p *Producer) Close() {}

func (p *Producer) Emit(_ context.Context, triggerType string, guildId uint64, causationId string, payload any) {
	if p == nil || p.redis == nil {
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

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		err := p.redis.XAdd(ctx, &redis.XAddArgs{
			Stream:       TopicWorkflowTriggers,
			MaxLenApprox: maxLenApproxWorkflows,
			ID:           "*",
			Values:       map[string]interface{}{"data": string(envBytes)},
		}).Err()

		if err != nil && p.logger != nil {
			p.logger.Error("workflowbus: produce failed",
				zap.String("trigger", triggerType),
				zap.Uint64("guild_id", guildId),
				zap.Error(err))
		}
	}()
}

var (
	globalProducer *Producer
	globalMu       sync.RWMutex
)

func SetGlobal(p *Producer) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalProducer = p
}

func Emit(ctx context.Context, triggerType string, guildId uint64, causationId string, payload any) {
	globalMu.RLock()
	p := globalProducer
	globalMu.RUnlock()
	if p == nil {
		return
	}
	p.Emit(ctx, triggerType, guildId, causationId, payload)
}
