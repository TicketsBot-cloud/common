package premium

import (
	"encoding/json"
	"fmt"
	"time"
)

type cachedTier struct {
	Tier       int    `json:"tier"`
	FromVoting bool   `json:"from_voting"`
}

const timeout = time.Minute * 5

// Functions can take a user ID or guild ID

func (p *PremiumLookupClient) getCachedTier(id uint64) (tier cachedTier, err error) {
	key := fmt.Sprintf("premium:%d", id)

	res, err := p.redis.Get(key).Result(); if err != nil {
		return
	}

	err = json.Unmarshal([]byte(res), &tier)
	return
}

func (p *PremiumLookupClient) setCachedTier(id uint64, data cachedTier) (err error) {
	key := fmt.Sprintf("premium:%d", id)

	marshalled, err := json.Marshal(data); if err != nil {
		return
	}

	return p.redis.Set(key, string(marshalled), timeout).Err()
}
