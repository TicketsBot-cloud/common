package featureflags

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-redis/redis/v8"
)

// payloadKey holds the last-known-good GrowthBook payload.
//
// Deliberately has no TTL. A stale ruleset is far better than none: if
// GrowthBook has been down for longer than any TTL we would have picked, a
// restarting worker should still come up with the rules it had before rather
// than with every flag off.
const payloadKey = "featureflags:payload"

type payloadCache struct {
	redis *redis.Client
}

// get returns the cached payload, or an empty string if nothing is cached.
func (p *payloadCache) get(ctx context.Context) (string, error) {
	payload, err := p.redis.Get(ctx, payloadKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", nil
		}

		return "", fmt.Errorf("featureflags: reading %s: %w", payloadKey, err)
	}

	return payload, nil
}

func (p *payloadCache) set(ctx context.Context, payload string) error {
	if err := p.redis.Set(ctx, payloadKey, payload, 0).Err(); err != nil {
		return fmt.Errorf("featureflags: writing %s: %w", payloadKey, err)
	}

	return nil
}
