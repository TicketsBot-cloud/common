package rpc

import (
	"context"

	"github.com/TicketsBot-cloud/common/utils"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kversion"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type Client struct {
	config Config
	client *kgo.Client
	logger *zap.Logger

	consumerRunning *atomic.Bool
	listeners       map[string]Listener

	cancelFunc context.CancelFunc
}

type Config struct {
	Brokers             []string
	ConsumerGroup       string
	ConsumerConcurrency int
}

func NewClient(logger *zap.Logger, config Config, listeners map[string]Listener) (*Client, error) {
	kafkaClient, err := connectKafka(config.Brokers, config.ConsumerGroup, utils.Keys(listeners))
	if err != nil {
		return nil, err
	}

	return &Client{
		config:          config,
		client:          kafkaClient,
		logger:          logger,
		consumerRunning: atomic.NewBool(false),
		listeners:       listeners,
	}, nil
}

func (c *Client) Shutdown() {
	c.client.Close()

	if c.cancelFunc != nil {
		c.cancelFunc()
	}
}

func connectKafka(brokers []string, consumerGroup string, topics []string) (*kgo.Client, error) {
	return kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(consumerGroup),
		kgo.ConsumeTopics(topics...),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		// franz-go v1.18 probes up to Kafka 3.8 API versions (incl. METADATA v13).
		// Broker is pinned at Kafka 3.7.2 and closes sockets on v13. Cap at V3_7_0
		// so every consumer built from this client stays within the broker's surface.
		kgo.MaxVersions(kversion.V3_7_0()),
	)
}
