package rpc

import (
	"context"
	"errors"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/panjf2000/ants/v2"
	"go.uber.org/zap"
)

const (
	maxEventsPerPoll    = 100
	pollBlockDuration   = 1 * time.Second
	maintenanceInterval = 30 * time.Second
	autoClaimMinIdle    = 5 * time.Minute
	autoClaimBatchSize  = 100
	defaultMaxLen       = int64(50000)
)

func (c *Client) StartConsumer() {
	if c.consumerRunning.Swap(true) {
		c.logger.Fatal("Consumer already running")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancelFunc = cancel

	pool, err := ants.NewPool(c.config.ConsumerConcurrency)
	if err != nil {
		c.logger.Fatal("Failed to create worker pool", zap.Error(err))
		return
	}

	streams := make([]string, 0, len(c.listeners)*2)
	for stream := range c.listeners {
		streams = append(streams, stream)
	}
	for range c.listeners {
		streams = append(streams, ">")
	}

	for stream := range c.listeners {
		go c.maintenanceLoop(ctx, stream)
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			messages, err := c.poll(ctx, streams)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					c.logger.Info("Context cancelled, stopping read loop")
					return
				}
				c.logger.Error("Failed to poll records", zap.Error(err))
				continue
			}

			for _, xStream := range messages {
				listener, ok := c.listeners[xStream.Stream]
				if !ok {
					c.logger.Warn("No listener found for stream", zap.String("stream", xStream.Stream))
					continue
				}

				streamName := xStream.Stream
				for _, msg := range xStream.Messages {
					value, ok := msg.Values["data"]
					if !ok {
						c.logger.Warn("Message missing data field", zap.String("stream", streamName), zap.String("id", msg.ID))
						c.redis.XAck(ctx, streamName, c.config.ConsumerGroup, msg.ID)
						continue
					}

					data := []byte(value.(string))
					msgID := msg.ID

					if err := pool.Submit(func() {
						listenerCtx, listenerCancel := listener.BuildContext()
						defer listenerCancel()

						listener.HandleMessage(listenerCtx, data)
						c.redis.XAck(ctx, streamName, c.config.ConsumerGroup, msgID)
					}); err != nil {
						c.logger.Error("Failed to submit task to worker pool", zap.Error(err))
						continue
					}
				}
			}
		}
	}
}

func (c *Client) poll(ctx context.Context, streams []string) ([]redis.XStream, error) {
	result, err := c.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.config.ConsumerGroup,
		Consumer: c.config.ConsumerName,
		Streams:  streams,
		Count:    maxEventsPerPoll,
		Block:    pollBlockDuration,
	}).Result()

	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}

	return result, nil
}

func (c *Client) maintenanceLoop(ctx context.Context, stream string) {
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.trimStream(ctx, stream)
			c.autoClaimStale(ctx, stream)
		}
	}
}

func (c *Client) trimStream(ctx context.Context, stream string) {
	maxLen := c.config.MaxLen
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}

	trimmed, err := c.redis.XTrimMaxLenApprox(ctx, stream, maxLen, 0).Result()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			c.logger.Warn("Failed to trim stream",
				zap.String("stream", stream),
				zap.Error(err))
		}
		return
	}

	if trimmed > 0 {
		c.logger.Debug("Trimmed stream",
			zap.String("stream", stream),
			zap.Int64("trimmed", trimmed))
	}
}

func (c *Client) autoClaimStale(ctx context.Context, stream string) {
	// go-redis/v8's XAutoClaim parser expects 2 response elements, but Redis 7+
	// returns 3 (messages, next start ID, deleted entry IDs). Use a raw Do call
	// and parse only the messages array ourselves.
	result, err := c.redis.Do(ctx,
		"XAUTOCLAIM", stream, c.config.ConsumerGroup, c.config.ConsumerName,
		int64(autoClaimMinIdle/time.Millisecond), "0-0", "COUNT", autoClaimBatchSize,
	).Result()

	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, redis.Nil) {
			c.logger.Warn("Failed to auto-claim stale messages",
				zap.String("stream", stream),
				zap.Error(err))
		}
		return
	}

	parts, ok := result.([]interface{})
	if !ok || len(parts) < 2 {
		return
	}

	msgs, ok := parts[1].([]interface{})
	if !ok || len(msgs) == 0 {
		return
	}

	listener, ok := c.listeners[stream]
	if !ok {
		return
	}

	c.logger.Info("Auto-claimed stale messages",
		zap.String("stream", stream),
		zap.Int("count", len(msgs)))

	for _, raw := range msgs {
		entry, ok := raw.([]interface{})
		if !ok || len(entry) < 2 {
			continue
		}

		msgID, _ := entry[0].(string)
		fields, _ := entry[1].([]interface{})

		var data string
		for i := 0; i+1 < len(fields); i += 2 {
			if key, _ := fields[i].(string); key == "data" {
				data, _ = fields[i+1].(string)
				break
			}
		}

		if data == "" {
			c.redis.XAck(ctx, stream, c.config.ConsumerGroup, msgID)
			continue
		}

		listenerCtx, listenerCancel := listener.BuildContext()
		listener.HandleMessage(listenerCtx, []byte(data))
		listenerCancel()
		c.redis.XAck(ctx, stream, c.config.ConsumerGroup, msgID)
	}
}
