package rpc

import (
	"context"
	"encoding/json"

	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
)

const defaultMaxLenApprox int64 = 50000

func (c *Client) ProduceSync(ctx context.Context, stream string, message []byte) error {
	c.logger.Debug("Producing message", zap.String("stream", stream), zap.ByteString("message", message))

	return c.redis.XAdd(ctx, &redis.XAddArgs{
		Stream:       stream,
		MaxLenApprox: defaultMaxLenApprox,
		ID:           "*",
		Values:       map[string]interface{}{"data": string(message)},
	}).Err()
}

func (c *Client) ProduceSyncJson(ctx context.Context, stream string, message any) error {
	bytes, err := json.Marshal(message)
	if err != nil {
		return err
	}

	return c.ProduceSync(ctx, stream, bytes)
}
