package claimrelay

import (
	"context"
	"encoding/json"

	"github.com/TicketsBot-cloud/common/utils"
	"github.com/go-redis/redis/v8"
)

type TicketClaim struct {
	GuildId  uint64 `json:"guild_id"`
	TicketId int    `json:"ticket_id"`
	UserId   uint64 `json:"user_id"` // who performed the action
	Claim    bool   `json:"claim"`   // true = claim, false = unclaim
}

const key = "tickets:claim"

func Publish(redis *redis.Client, data TicketClaim) error {
	marshalled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return redis.RPush(utils.DefaultContext(), key, string(marshalled)).Err()
}

func Listen(redis *redis.Client, ch chan TicketClaim) {
	for {
		res, err := redis.BLPop(context.Background(), 0, key).Result()
		if err != nil {
			continue
		}

		var data TicketClaim
		if err := json.Unmarshal([]byte(res[1]), &data); err != nil {
			continue
		}

		ch <- data
	}
}
