package rpc

import (
	"context"
	"fmt"
	"os"

	"github.com/go-redis/redis/v8"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type Client struct {
	config Config
	redis  *redis.Client
	logger *zap.Logger

	consumerRunning *atomic.Bool
	listeners       map[string]Listener

	cancelFunc context.CancelFunc
}

type Config struct {
	Redis               *redis.Client
	ConsumerGroup       string
	ConsumerName        string
	ConsumerConcurrency int
	MaxLen              int64
}

func NewClient(logger *zap.Logger, config Config, listeners map[string]Listener) (*Client, error) {
	if config.ConsumerName == "" {
		hostname, _ := os.Hostname()
		config.ConsumerName = hostname
	}

	ctx := context.Background()
	for stream := range listeners {
		err := config.Redis.XGroupCreateMkStream(ctx, stream, config.ConsumerGroup, "$").Err()
		if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return nil, fmt.Errorf("create consumer group for stream %s: %w", stream, err)
		}
	}

	return &Client{
		config:          config,
		redis:           config.Redis,
		logger:          logger,
		consumerRunning: atomic.NewBool(false),
		listeners:       listeners,
	}, nil
}

func (c *Client) Shutdown() {
	if c.cancelFunc != nil {
		c.cancelFunc()
	}
}
