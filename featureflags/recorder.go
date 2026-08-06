package featureflags

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
)

// RecordedExposure is an Exposure stamped with the moment it happened, rather
// than the moment it reached the database. Assignment analysis cares when the
// unit saw the variation.
type RecordedExposure struct {
	Exposure
	ExposedAt time.Time
}

// ExposureSink persists a batch of exposures. Kept as an interface so this
// package does not depend on the database module, which matters because common
// consumes database as a pinned version.
type ExposureSink interface {
	InsertExposures(ctx context.Context, exposures []RecordedExposure) error
}

// SinkFunc adapts a plain function to ExposureSink, so a service can supply the
// Postgres write at its own call site without this package importing database:
//
//	sink := featureflags.SinkFunc(func(ctx context.Context, exposures []featureflags.RecordedExposure) error {
//		rows := make([]database.ExperimentExposure, 0, len(exposures))
//		for _, e := range exposures {
//			rows = append(rows, database.ExperimentExposure{
//				ExperimentKey:  e.ExperimentKey,
//				VariationId:    e.VariationId,
//				IdentifierType: e.IdentifierType,
//				Identifier:     e.Identifier,
//				FeatureKey:     e.FeatureKey,
//				ExposedAt:      e.ExposedAt,
//			})
//		}
//		return dbclient.Client.ExperimentExposures.InsertBatch(ctx, rows)
//	})
type SinkFunc func(ctx context.Context, exposures []RecordedExposure) error

func (f SinkFunc) InsertExposures(ctx context.Context, exposures []RecordedExposure) error {
	return f(ctx, exposures)
}

// ExposureDeduper claims exposures across processes, returning for each key
// whether this caller won the claim. Keys already claimed by another pod return
// false and are dropped rather than written.
type ExposureDeduper interface {
	Claim(ctx context.Context, keys []string, ttl time.Duration) ([]bool, error)
}

// RecorderConfig tunes the exposure pipeline. Zero values take the defaults
// applied by withDefaults.
type RecorderConfig struct {
	// QueueSize bounds memory and, more importantly, bounds how far behind the
	// writer can fall before shedding load.
	QueueSize int
	// BatchSize is how many rows one COPY carries.
	BatchSize int
	// FlushInterval caps how long a partial batch waits.
	FlushInterval time.Duration
	// WriteTimeout bounds one flush.
	WriteTimeout time.Duration
	// DedupeTTL is how long a claimed exposure suppresses further rows for the
	// same unit and experiment.
	DedupeTTL time.Duration
	// LocalCacheEntries caps the per-generation size of the in-process set.
	LocalCacheEntries int
	// LocalCacheRotation is how often the in-process set drops a generation.
	LocalCacheRotation time.Duration
}

func (c RecorderConfig) withDefaults() RecorderConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = 4096
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.DedupeTTL <= 0 {
		c.DedupeTTL = 24 * time.Hour
	}
	if c.LocalCacheEntries <= 0 {
		c.LocalCacheEntries = 100_000
	}
	if c.LocalCacheRotation <= 0 {
		c.LocalCacheRotation = 30 * time.Minute
	}

	return c
}

// RecorderStats is a snapshot of pipeline counters. Services expose these as
// metrics; this package does not depend on any metrics library.
type RecorderStats struct {
	// Enqueued exposures accepted onto the queue.
	Enqueued uint64
	// SuppressedLocally were already in the in-process set, so cost no I/O.
	SuppressedLocally uint64
	// Dropped were shed because the queue was full. Non-zero means experiment
	// data is being lost and the writer cannot keep up.
	Dropped uint64
	// SuppressedRemotely lost the cross-process claim, meaning another pod
	// already recorded this unit.
	SuppressedRemotely uint64
	// Written rows reached the sink.
	Written uint64
	// FailedWrites are flushes that errored. Those exposures are lost.
	FailedWrites uint64
	// LocalCacheEntries currently held in the in-process set.
	LocalCacheEntries uint64
}

// Recorder implements ExposureRecorder. It keeps all I/O off the caller's
// goroutine: Record does a read-locked map lookup and a non-blocking channel
// send, nothing more.
type Recorder struct {
	logger  *zap.Logger
	sink    ExposureSink
	deduper ExposureDeduper
	cfg     RecorderConfig

	queue chan RecordedExposure
	seen  *rotatingSet

	enqueued           atomic.Uint64
	suppressedLocally  atomic.Uint64
	dropped            atomic.Uint64
	suppressedRemotely atomic.Uint64
	written            atomic.Uint64
	failedWrites       atomic.Uint64

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	// now is overridable for tests.
	now func() time.Time
}

// NewRecorder starts the background writer. deduper may be nil, in which case
// deduplication is in-process only and duplicate rows across pods are expected.
func NewRecorder(logger *zap.Logger, sink ExposureSink, deduper ExposureDeduper, cfg RecorderConfig) *Recorder {
	cfg = cfg.withDefaults()

	r := &Recorder{
		logger:  logger,
		sink:    sink,
		deduper: deduper,
		cfg:     cfg,
		queue:   make(chan RecordedExposure, cfg.QueueSize),
		seen:    newRotatingSet(cfg.LocalCacheEntries, cfg.LocalCacheRotation),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		now:     time.Now,
	}

	go r.run()

	return r
}

// Record is called from the evaluation path, which for the worker means inside
// Discord event handling. It must never block and never perform I/O. A nil
// receiver is a no-op, so a service can wire flags without exposure recording.
func (r *Recorder) Record(_ context.Context, exposure Exposure) {
	if r == nil {
		return
	}

	key := dedupeKey(exposure)

	if r.seen.contains(key) {
		r.suppressedLocally.Add(1)
		return
	}

	select {
	case r.queue <- RecordedExposure{Exposure: exposure, ExposedAt: r.now()}:
		// Marked seen only after a successful send. Marking before would mean a
		// shed exposure was never retried, silently losing that unit for a whole
		// rotation window.
		r.seen.add(key)
		r.enqueued.Add(1)
	default:
		r.dropped.Add(1)
	}
}

func (r *Recorder) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]RecordedExposure, 0, r.cfg.BatchSize)

	for {
		select {
		case <-r.stop:
			// Drain whatever is already queued so a graceful shutdown does not
			// discard exposures that were accepted.
			for {
				select {
				case exposure := <-r.queue:
					batch = append(batch, exposure)
					if len(batch) >= r.cfg.BatchSize {
						r.flush(batch)
						batch = batch[:0]
					}
				default:
					r.flush(batch)
					return
				}
			}
		case exposure := <-r.queue:
			batch = append(batch, exposure)
			if len(batch) >= r.cfg.BatchSize {
				r.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				r.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

func (r *Recorder) flush(batch []RecordedExposure) {
	if len(batch) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.WriteTimeout)
	defer cancel()

	batch = r.claim(ctx, batch)
	if len(batch) == 0 {
		return
	}

	if err := r.sink.InsertExposures(ctx, batch); err != nil {
		r.failedWrites.Add(uint64(len(batch)))
		r.logger.Error("featureflags: writing exposures failed",
			zap.Int("count", len(batch)), zap.Error(err))
		return
	}

	r.written.Add(uint64(len(batch)))
}

// claim filters the batch down to exposures this process won the claim for.
//
// On deduper failure it deliberately fails open and keeps everything. Losing
// exposures biases an experiment's results, whereas duplicate rows are harmless
// provided the assignment query takes the first exposure per unit, which it must
// do anyway to handle deduplication-window boundaries.
func (r *Recorder) claim(ctx context.Context, batch []RecordedExposure) []RecordedExposure {
	if r.deduper == nil {
		return batch
	}

	keys := make([]string, 0, len(batch))
	for _, exposure := range batch {
		keys = append(keys, dedupeKey(exposure.Exposure))
	}

	claimed, err := r.deduper.Claim(ctx, keys, r.cfg.DedupeTTL)
	if err != nil {
		r.logger.Warn("featureflags: claiming exposures failed, writing without deduplication",
			zap.Int("count", len(batch)), zap.Error(err))
		return batch
	}

	if len(claimed) != len(batch) {
		r.logger.Warn("featureflags: deduper returned mismatched results, writing without deduplication",
			zap.Int("want", len(batch)), zap.Int("got", len(claimed)))
		return batch
	}

	kept := batch[:0]
	for i, ok := range claimed {
		if ok {
			kept = append(kept, batch[i])
			continue
		}

		r.suppressedRemotely.Add(1)
	}

	return kept
}

func (r *Recorder) Stats() RecorderStats {
	return RecorderStats{
		Enqueued:           r.enqueued.Load(),
		SuppressedLocally:  r.suppressedLocally.Load(),
		Dropped:            r.dropped.Load(),
		SuppressedRemotely: r.suppressedRemotely.Load(),
		Written:            r.written.Load(),
		FailedWrites:       r.failedWrites.Load(),
		LocalCacheEntries:  uint64(r.seen.len()),
	}
}

// Close stops accepting work, flushes what is queued, and waits for the writer.
func (r *Recorder) Close() error {
	r.stopOnce.Do(func() {
		close(r.stop)
	})

	<-r.done

	return nil
}

// dedupeKey identifies one unit's enrolment in one experiment. The variation is
// deliberately excluded: assignment is stable, so including it would let a
// bucketing change produce a second row for the same unit.
func dedupeKey(exposure Exposure) string {
	var b strings.Builder
	b.Grow(len(exposure.ExperimentKey) + len(exposure.IdentifierType) + len(exposure.Identifier) + 2)
	b.WriteString(exposure.ExperimentKey)
	b.WriteByte(':')
	b.WriteString(exposure.IdentifierType)
	b.WriteByte(':')
	b.WriteString(exposure.Identifier)

	return b.String()
}

// redisDeduper claims exposures in Redis so that all pods together write one row
// per unit per experiment per DedupeTTL.
type redisDeduper struct {
	redis *redis.Client
}

// NewRedisDeduper returns an ExposureDeduper backed by SETNX.
func NewRedisDeduper(client *redis.Client) ExposureDeduper {
	return &redisDeduper{redis: client}
}

func (d *redisDeduper) Claim(ctx context.Context, keys []string, ttl time.Duration) ([]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	pipe := d.redis.Pipeline()

	cmds := make([]*redis.BoolCmd, 0, len(keys))
	for _, key := range keys {
		cmds = append(cmds, pipe.SetNX(ctx, "featureflags:exposure:"+key, 1, ttl))
	}

	// Exec reports the first command error. redis.Nil is not meaningful for
	// SETNX, so it is not treated as a failure.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("featureflags: claiming exposures: %w", err)
	}

	claimed := make([]bool, 0, len(cmds))
	for _, cmd := range cmds {
		ok, err := cmd.Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return nil, fmt.Errorf("featureflags: claiming exposures: %w", err)
		}

		claimed = append(claimed, ok)
	}

	return claimed, nil
}
