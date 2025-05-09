package chatrelay

import (
	"context"
	"encoding/json"

	"github.com/TicketsBot-cloud/common/utils"
	"github.com/TicketsBot-cloud/database"
	"github.com/TicketsBot-cloud/gdl/objects/channel/message"
	"github.com/go-redis/redis/v8"
)

type MessageData struct {
	Ticket  database.Ticket `json:"ticket"`
	Message message.Message `json:"message"`
}

const channel = "tickets:chatrelay"

func PublishMessage(redis *redis.Client, data MessageData) error {
	marshalled, err := json.Marshal(data)
	if err != nil {
		return err
	}

	return redis.Publish(utils.DefaultContext(), channel, string(marshalled)).Err()
}

func Listen(redis *redis.Client, ch chan MessageData) {
	for payload := range redis.Subscribe(context.Background(), channel).Channel() {
		var data MessageData

		if err := json.Unmarshal([]byte(payload.Payload), &data); err != nil {
			continue
		}

		ch <- data
	}
}
