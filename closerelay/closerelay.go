package closerelay

import (
	"context"
	"encoding/json"
	"time"

	"github.com/TicketsBot-cloud/common/utils"
	"github.com/go-redis/redis/v8"
)

type TicketClose struct {
	GuildId     uint64 `json:"guild_id"`
	TicketId    int    `json:"ticket_id"`
	UserId      uint64 `json:"user_id"`
	Reason      string `json:"reason"`
	ResponseKey string `json:"response_key,omitempty"`
}

type CloseResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

const (
	key       = "tickets:close"
	resultTTL = 2 * time.Minute
)

func Publish(redis *redis.Client, data TicketClose) error {
	marshalled, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return redis.RPush(utils.DefaultContext(), key, string(marshalled)).Err()
}

func PublishResult(client *redis.Client, responseKey string, result CloseResult) error {
	marshalled, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := client.RPush(utils.DefaultContext(), responseKey, string(marshalled)).Err(); err != nil {
		return err
	}
	_ = client.Expire(utils.DefaultContext(), responseKey, resultTTL)
	return nil
}

func WaitForResult(client *redis.Client, responseKey string, timeout time.Duration) (CloseResult, error) {
	res, err := client.BLPop(context.Background(), timeout, responseKey).Result()
	if err != nil {
		return CloseResult{}, err
	}

	var result CloseResult
	if err := json.Unmarshal([]byte(res[1]), &result); err != nil {
		return CloseResult{}, err
	}

	return result, nil
}

func Listen(redis *redis.Client, ch chan TicketClose) {
	for {
		res, err := redis.BLPop(context.Background(), 0, key).Result()
		if err != nil {
			continue
		}

		var data TicketClose
		if err := json.Unmarshal([]byte(res[1]), &data); err != nil {
			continue
		}

		ch <- data
	}
}
