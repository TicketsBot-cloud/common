// Package featureflags evaluates feature flags and experiments against
// GrowthBook.
//
// Flag definitions live in GrowthBook, not in Go code, so adding a flag needs no
// deploy and no tagged release of this module. Evaluation is entirely in-process:
// the SDK holds the ruleset in memory and performs no network call per call to
// IsEnabled, which is a hard requirement for the worker's event hot path.
//
// The package is designed to fail open on availability rather than correctness.
// If GrowthBook is unreachable at startup the last-known-good payload is read
// from Redis; if that is missing too, every flag evaluates to its zero value and
// the process still boots. A deployment with no GrowthBook configured at all
// behaves the same way, which is what self-hosted installations get.
package featureflags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	gb "github.com/growthbook/growthbook-golang"
	"go.uber.org/zap"
)

// Exposure records that a unit was genuinely enrolled in an experiment. It is
// the input to the assignment table GrowthBook reads when computing results.
type Exposure struct {
	// ExperimentKey identifies the experiment.
	ExperimentKey string
	// VariationId is the zero-based index of the assigned variation.
	VariationId int
	// IdentifierType is the attribute assignment was keyed on, one of AttrGuild,
	// AttrUser or AttrDashboardUser.
	IdentifierType string
	// Identifier is the value of that attribute.
	Identifier string
	// FeatureKey is the flag the experiment was reached through, if any.
	FeatureKey string
}

// ExposureRecorder persists exposures. Implementations must be non-blocking:
// Record is called on the evaluation path, which for the worker means inside
// Discord event handling.
type ExposureRecorder interface {
	Record(ctx context.Context, exposure Exposure)
}

// Result is the outcome of evaluating a flag.
type Result struct {
	// On is the flag's truthiness, for boolean rollouts and kill switches.
	On bool
	// Value is the raw value, for multivariate flags.
	Value any
	// InExperiment reports whether this evaluation enrolled the unit in an
	// experiment, as opposed to matching a plain targeting or rollout rule.
	InExperiment bool
	// VariationId is the assigned variation index when InExperiment is true.
	VariationId int
	// Source describes which rule produced the value, for debugging.
	Source string
}

// Client evaluates flags. It is safe for concurrent use.
type Client struct {
	cfg      Config
	logger   *zap.Logger
	gb       *gb.Client
	cache    *payloadCache
	recorder ExposureRecorder

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New builds a Client.
//
// redisClient may be nil, in which case the last-known-good payload is not
// cached and a process starting while GrowthBook is down has no rules to fall
// back to. recorder may be nil, in which case exposures are not recorded and
// experiments cannot be analysed, though flags still evaluate normally.
//
// New never returns an error because GrowthBook is unreachable. It only fails on
// misconfiguration.
func New(
	ctx context.Context,
	cfg Config,
	logger *zap.Logger,
	redisClient *redis.Client,
	recorder ExposureRecorder,
) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	c := &Client{
		cfg:      cfg,
		logger:   logger,
		recorder: recorder,
		stop:     make(chan struct{}),
	}

	if !cfg.Enabled() {
		logger.Warn("featureflags: GrowthBook not configured, all flags evaluate to their zero value")
		return c, nil
	}

	if redisClient != nil {
		c.cache = &payloadCache{redis: redisClient}
	}

	opts := []gb.ClientOption{
		gb.WithApiHost(cfg.ApiHost),
		gb.WithClientKey(cfg.ClientKey),
		gb.WithLogger(newSlogAdapter(logger)),
		gb.WithExperimentCallback(c.onExperimentViewed),
	}

	if cfg.DecryptionKey != "" {
		opts = append(opts, gb.WithDecryptionKey(cfg.DecryptionKey))
	}

	if cfg.UseSSE {
		opts = append(opts, gb.WithSseDataSource())
	} else {
		opts = append(opts, gb.WithPollDataSource(cfg.PollInterval))
	}

	client, err := gb.NewClient(ctx, opts...)
	if err != nil {
		// A construction failure is a configuration problem, not a transient one.
		return nil, fmt.Errorf("featureflags: creating GrowthBook client: %w", err)
	}

	c.gb = client
	c.primeRuleset(ctx)

	c.wg.Add(1)
	go c.cacheRefreshLoop()

	return c, nil
}

// NewOffline builds a Client that evaluates a fixed ruleset with no network
// access, no Redis and no background work.
//
// featuresJSON is a GrowthBook features map, the inner object of an SDK payload:
// {"my-flag": {"defaultValue": false, "rules": [...]}}. Use this for tests, and
// to ship compiled-in defaults where a deployment has no GrowthBook at all.
func NewOffline(
	ctx context.Context,
	logger *zap.Logger,
	featuresJSON string,
	recorder ExposureRecorder,
) (*Client, error) {
	c := &Client{
		cfg:      Config{},
		logger:   logger,
		recorder: recorder,
		stop:     make(chan struct{}),
	}

	client, err := gb.NewClient(
		ctx,
		gb.WithJsonFeatures(featuresJSON),
		gb.WithLogger(newSlogAdapter(logger)),
		gb.WithExperimentCallback(c.onExperimentViewed),
	)
	if err != nil {
		return nil, fmt.Errorf("featureflags: creating offline client: %w", err)
	}

	c.gb = client

	return c, nil
}

// primeRuleset gets a usable ruleset in place before New returns.
//
// The data source started asynchronously. Give it a bounded moment to deliver
// the first payload, and if it does not, fall back to the copy in Redis. The
// ordering matters: seeding from Redis unconditionally would race the live
// payload and could overwrite fresh rules with stale ones.
func (c *Client) primeRuleset(ctx context.Context) {
	loadCtx, cancel := context.WithTimeout(ctx, c.cfg.LoadTimeout)
	defer cancel()

	loadErr := c.gb.EnsureLoaded(loadCtx)
	if loadErr == nil {
		c.persistPayload(ctx)
		return
	}

	// Distinguish an exhausted deadline from a real failure. Reporting everything
	// as a timeout hides the actual cause, which is usually an unreachable host or
	// a rejected client key.
	if errors.Is(loadErr, context.DeadlineExceeded) {
		c.logger.Warn("featureflags: GrowthBook payload not loaded within timeout, falling back to cache",
			zap.Duration("timeout", c.cfg.LoadTimeout),
			zap.String("api_host", c.cfg.ApiHost))
	} else {
		c.logger.Warn("featureflags: loading GrowthBook payload failed, falling back to cache",
			zap.String("api_host", c.cfg.ApiHost),
			zap.Error(loadErr))
	}

	if c.cache == nil {
		c.logger.Error("featureflags: no Redis cache configured, starting with no rules")
		return
	}

	payload, err := c.cache.get(ctx)
	if err != nil {
		c.logger.Error("featureflags: reading cached payload failed, starting with no rules", zap.Error(err))
		return
	}

	if payload == "" {
		c.logger.Error("featureflags: no cached payload available, starting with no rules")
		return
	}

	if err := c.gb.UpdateFromApiResponseJSON(payload); err != nil {
		c.logger.Error("featureflags: cached payload rejected, starting with no rules", zap.Error(err))
		return
	}

	c.logger.Info("featureflags: loaded rules from cached payload")
}

// cacheRefreshLoop keeps the Redis copy of the payload current so a restarting
// process has something recent to boot from.
func (c *Client) cacheRefreshLoop() {
	defer c.wg.Done()

	if c.cache == nil {
		return
	}

	ticker := time.NewTicker(c.cfg.CacheRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c.persistPayload(ctx)
			cancel()
		}
	}
}

func (c *Client) persistPayload(ctx context.Context) {
	if c.cache == nil {
		return
	}

	// Fetch through the SDK rather than reconstructing the payload from
	// Features(), so what gets cached is byte-compatible with what
	// UpdateFromApiResponseJSON expects on the way back in.
	resp, err := c.gb.CallFeatureApi(ctx, "")
	if err != nil {
		c.logger.Warn("featureflags: fetching payload for cache failed", zap.Error(err))
		return
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		c.logger.Warn("featureflags: encoding payload for cache failed", zap.Error(err))
		return
	}

	if err := c.cache.set(ctx, string(encoded)); err != nil {
		c.logger.Warn("featureflags: writing cached payload failed", zap.Error(err))
	}
}

func (c *Client) onExperimentViewed(ctx context.Context, exp *gb.Experiment, result *gb.ExperimentResult, _ any) {
	if c.recorder == nil || exp == nil || result == nil {
		return
	}

	// Only genuine enrolment counts. Plain rollout rules and kill switches reach
	// this callback without putting the unit in an experiment, and recording
	// those would inflate the assignment table with rows no analysis wants.
	if !result.InExperiment || result.HashAttribute == "" || result.HashValue == "" {
		return
	}

	c.recorder.Record(ctx, Exposure{
		ExperimentKey:  exp.Key,
		VariationId:    result.VariationId,
		IdentifierType: result.HashAttribute,
		Identifier:     result.HashValue,
		FeatureKey:     result.FeatureId,
	})
}

// Eval evaluates key for attrs. It never returns an error: an unknown flag, an
// unconfigured client or an empty ruleset all yield the zero Result, so callers
// read as "off unless explicitly turned on".
//
// A nil receiver is valid and evaluates to the zero Result. Services hold this as
// a package-level variable assigned during startup, and the old implementation's
// settable global could be read before it was set and panic. Treating nil as
// "every flag off" removes that failure mode entirely.
func (c *Client) Eval(ctx context.Context, key string, attrs Attributes) Result {
	if c == nil || c.gb == nil {
		return Result{}
	}

	scoped, err := c.gb.WithAttributes(attrs.toGrowthBook())
	if err != nil {
		c.logger.Error("featureflags: applying attributes failed",
			zap.String("flag", key), zap.Error(err))
		return Result{}
	}

	res := scoped.EvalFeature(ctx, key)
	if res == nil {
		return Result{}
	}

	out := Result{
		On:     res.On,
		Value:  res.Value,
		Source: string(res.Source),
	}

	if res.ExperimentResult != nil {
		out.InExperiment = res.ExperimentResult.InExperiment
		out.VariationId = res.ExperimentResult.VariationId
	}

	return out
}

// IsEnabled reports whether a boolean flag is on. This is the direct replacement
// for the old HasFeature.
func (c *Client) IsEnabled(ctx context.Context, key string, attrs Attributes) bool {
	return c.Eval(ctx, key, attrs).On
}

// StringValue returns a string-valued flag, or def if the flag is missing or
// holds a non-string value.
func (c *Client) StringValue(ctx context.Context, key string, attrs Attributes, def string) string {
	value, ok := c.Eval(ctx, key, attrs).Value.(string)
	if !ok {
		return def
	}

	return value
}

// IntValue returns a numeric flag, or def if the flag is missing or holds a
// non-numeric value.
//
// GrowthBook payloads arrive as JSON, so whole numbers decode to float64. Both
// are accepted to keep callers from having to know that.
func (c *Client) IntValue(ctx context.Context, key string, attrs Attributes, def int) int {
	switch value := c.Eval(ctx, key, attrs).Value.(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return def
	}
}

// Close stops background work and releases the SDK's connections. A nil receiver
// is a no-op.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}

	c.stopOnce.Do(func() {
		close(c.stop)
	})

	c.wg.Wait()

	if c.gb == nil {
		return nil
	}

	if err := c.gb.Close(); err != nil {
		return fmt.Errorf("featureflags: closing GrowthBook client: %w", err)
	}

	return nil
}
