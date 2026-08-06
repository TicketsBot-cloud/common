package featureflags

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeSink struct {
	mu       sync.Mutex
	batches  [][]RecordedExposure
	err      error
	inserted []RecordedExposure
}

func (f *fakeSink) InsertExposures(_ context.Context, exposures []RecordedExposure) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	batch := append([]RecordedExposure(nil), exposures...)
	f.batches = append(f.batches, batch)
	f.inserted = append(f.inserted, batch...)

	return nil
}

func (f *fakeSink) all() []RecordedExposure {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]RecordedExposure(nil), f.inserted...)
}

func (f *fakeSink) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.batches)
}

// fakeDeduper claims a key the first time it is seen, mimicking SETNX.
type fakeDeduper struct {
	mu     sync.Mutex
	seen   map[string]struct{}
	err    error
	badLen bool
	calls  int
}

func newFakeDeduper() *fakeDeduper {
	return &fakeDeduper{seen: map[string]struct{}{}}
}

func (f *fakeDeduper) Claim(_ context.Context, keys []string, _ time.Duration) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++

	if f.err != nil {
		return nil, f.err
	}

	if f.badLen {
		return []bool{true}, nil
	}

	claimed := make([]bool, 0, len(keys))
	for _, key := range keys {
		_, exists := f.seen[key]
		claimed = append(claimed, !exists)
		f.seen[key] = struct{}{}
	}

	return claimed, nil
}

func exposure(experiment string, guildId uint64) Exposure {
	return Exposure{
		ExperimentKey:  experiment,
		VariationId:    1,
		IdentifierType: AttrGuild,
		Identifier:     strconv.FormatUint(guildId, 10),
		FeatureKey:     "feature-" + experiment,
	}
}

// eventually polls rather than sleeping a fixed duration, so the tests are not
// timing-fragile on a loaded machine.
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	require.Eventually(t, condition, 3*time.Second, 5*time.Millisecond)
}

func TestRecorderConfigDefaults(t *testing.T) {
	cfg := RecorderConfig{}.withDefaults()

	require.Positive(t, cfg.QueueSize)
	require.Positive(t, cfg.BatchSize)
	require.Positive(t, cfg.FlushInterval)
	require.Positive(t, cfg.WriteTimeout)
	require.Equal(t, 24*time.Hour, cfg.DedupeTTL)
	require.Positive(t, cfg.LocalCacheEntries)
	require.Positive(t, cfg.LocalCacheRotation)
}

func TestRecorderConfigKeepsExplicitValues(t *testing.T) {
	cfg := RecorderConfig{
		QueueSize:          7,
		BatchSize:          3,
		FlushInterval:      time.Second,
		WriteTimeout:       2 * time.Second,
		DedupeTTL:          time.Hour,
		LocalCacheEntries:  11,
		LocalCacheRotation: time.Minute,
	}.withDefaults()

	require.Equal(t, 7, cfg.QueueSize)
	require.Equal(t, 3, cfg.BatchSize)
	require.Equal(t, time.Hour, cfg.DedupeTTL)
	require.Equal(t, 11, cfg.LocalCacheEntries)
}

func TestDedupeKeyIgnoresVariation(t *testing.T) {
	// Assignment is stable, so a variation change for the same unit means the
	// bucketing changed. That must not produce a second row.
	a := exposure("exp", 1)
	b := exposure("exp", 1)
	b.VariationId = 0

	require.Equal(t, dedupeKey(a), dedupeKey(b))
}

func TestDedupeKeyDistinguishesUnitsExperimentsAndTypes(t *testing.T) {
	base := exposure("exp", 1)

	otherGuild := exposure("exp", 2)
	otherExperiment := exposure("exp2", 1)

	otherType := base
	otherType.IdentifierType = AttrUser

	keys := map[string]struct{}{
		dedupeKey(base):            {},
		dedupeKey(otherGuild):      {},
		dedupeKey(otherExperiment): {},
		dedupeKey(otherType):       {},
	}

	require.Len(t, keys, 4)
}

// The whole point of the local set: repeat traffic for a unit already queued must
// not reach the queue at all.
func TestRecordSuppressesRepeatsLocally(t *testing.T) {
	sink := &fakeSink{}
	deduper := newFakeDeduper()

	recorder := NewRecorder(zap.NewNop(), sink, deduper, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	ctx := context.Background()
	for i := 0; i < 100; i++ {
		recorder.Record(ctx, exposure("exp", 42))
	}

	eventually(t, func() bool { return len(sink.all()) == 1 })

	stats := recorder.Stats()
	require.Equal(t, uint64(1), stats.Enqueued)
	require.Equal(t, uint64(99), stats.SuppressedLocally)
	require.Zero(t, stats.Dropped)
}

func TestRecordStampsExposureTime(t *testing.T) {
	sink := &fakeSink{}
	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	frozen := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return frozen }

	recorder.Record(context.Background(), exposure("exp", 1))
	eventually(t, func() bool { return len(sink.all()) == 1 })

	require.Equal(t, frozen, sink.all()[0].ExposedAt)
}

// A full queue must shed load rather than block the caller, and a shed exposure
// must remain eligible so a later event retries it.
func TestRecordShedsLoadWithoutBlockingAndRetriesLater(t *testing.T) {
	sink := &fakeSink{}
	blocked := make(chan struct{})

	recorder := &Recorder{
		logger: zap.NewNop(),
		sink:   sink,
		cfg:    RecorderConfig{QueueSize: 2}.withDefaults(),
		queue:  make(chan RecordedExposure, 2),
		seen:   newRotatingSet(1000, time.Hour),
		stop:   make(chan struct{}),
		done:   blocked,
		now:    time.Now,
	}

	ctx := context.Background()

	// Writer is not running, so the queue fills and stays full.
	recorder.Record(ctx, exposure("exp", 1))
	recorder.Record(ctx, exposure("exp", 2))

	done := make(chan struct{})
	go func() {
		defer close(done)
		recorder.Record(ctx, exposure("exp", 3))
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Record blocked when the queue was full")
	}

	require.Equal(t, uint64(2), recorder.Stats().Enqueued)
	require.Equal(t, uint64(1), recorder.Stats().Dropped)

	// Guild 3 was shed, so it must not be marked seen; otherwise the unit would
	// be silently lost for a whole rotation window.
	require.False(t, recorder.seen.contains(dedupeKey(exposure("exp", 3))))
	require.True(t, recorder.seen.contains(dedupeKey(exposure("exp", 1))))
}

func TestRecorderDedupesAcrossProcesses(t *testing.T) {
	sink := &fakeSink{}
	// One shared deduper stands in for Redis seen by two pods.
	deduper := newFakeDeduper()

	cfg := RecorderConfig{BatchSize: 1, FlushInterval: 10 * time.Millisecond}

	first := NewRecorder(zap.NewNop(), sink, deduper, cfg)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second := NewRecorder(zap.NewNop(), sink, deduper, cfg)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	ctx := context.Background()
	first.Record(ctx, exposure("exp", 99))
	eventually(t, func() bool { return len(sink.all()) == 1 })

	// The second pod has its own empty local set, so it reaches the shared claim
	// and must lose.
	second.Record(ctx, exposure("exp", 99))
	eventually(t, func() bool { return second.Stats().SuppressedRemotely == 1 })

	require.Len(t, sink.all(), 1)
}

// Losing exposures biases a result; duplicates do not, provided the assignment
// query takes the first per unit. So a deduper outage must fail open.
func TestClaimFailureFailsOpen(t *testing.T) {
	sink := &fakeSink{}
	deduper := newFakeDeduper()
	deduper.err = errors.New("redis down")

	recorder := NewRecorder(zap.NewNop(), sink, deduper, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	recorder.Record(context.Background(), exposure("exp", 1))

	eventually(t, func() bool { return len(sink.all()) == 1 })
	require.Zero(t, recorder.Stats().SuppressedRemotely)
}

func TestClaimLengthMismatchFailsOpen(t *testing.T) {
	sink := &fakeSink{}
	deduper := newFakeDeduper()
	deduper.badLen = true

	recorder := NewRecorder(zap.NewNop(), sink, deduper, RecorderConfig{
		BatchSize:     2,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	ctx := context.Background()
	recorder.Record(ctx, exposure("exp", 1))
	recorder.Record(ctx, exposure("exp", 2))

	eventually(t, func() bool { return len(sink.all()) == 2 })
}

func TestBatchesFillToBatchSize(t *testing.T) {
	sink := &fakeSink{}

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize: 10,
		// Long enough that only the size trigger can fire.
		FlushInterval: time.Hour,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		recorder.Record(ctx, exposure("exp", uint64(i+1)))
	}

	eventually(t, func() bool { return len(sink.all()) == 10 })
	require.Equal(t, 1, sink.batchCount(), "expected a single batched write")
}

func TestPartialBatchFlushesOnInterval(t *testing.T) {
	sink := &fakeSink{}

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1000,
		FlushInterval: 20 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	recorder.Record(context.Background(), exposure("exp", 1))

	eventually(t, func() bool { return len(sink.all()) == 1 })
}

// Accepted exposures must not be discarded by shutdown.
func TestCloseDrainsQueue(t *testing.T) {
	sink := &fakeSink{}

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1000,
		FlushInterval: time.Hour,
	})

	ctx := context.Background()
	for i := 0; i < 50; i++ {
		recorder.Record(ctx, exposure("exp", uint64(i+1)))
	}

	require.NoError(t, recorder.Close())
	require.Len(t, sink.all(), 50)
}

func TestCloseIsIdempotentForRecorder(t *testing.T) {
	recorder := NewRecorder(zap.NewNop(), &fakeSink{}, nil, RecorderConfig{})

	require.NoError(t, recorder.Close())
	require.NoError(t, recorder.Close())
}

func TestSinkFailureIsCountedNotFatal(t *testing.T) {
	sink := &fakeSink{err: errors.New("postgres down")}

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	recorder.Record(context.Background(), exposure("exp", 1))

	eventually(t, func() bool { return recorder.Stats().FailedWrites == 1 })
	require.Zero(t, recorder.Stats().Written)
}

func TestRecordIsConcurrencySafe(t *testing.T) {
	sink := &fakeSink{}
	recorder := NewRecorder(zap.NewNop(), sink, newFakeDeduper(), RecorderConfig{
		QueueSize:     8192,
		BatchSize:     100,
		FlushInterval: 5 * time.Millisecond,
	})

	ctx := context.Background()
	const goroutines = 16
	const perGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				recorder.Record(ctx, exposure("exp", uint64(offset*perGoroutine+i+1)))
			}
		}(g)
	}
	wg.Wait()

	require.NoError(t, recorder.Close())

	stats := recorder.Stats()
	require.Equal(t, uint64(goroutines*perGoroutine), stats.Enqueued+stats.Dropped+stats.SuppressedLocally)
	require.Len(t, sink.all(), int(stats.Written))
}

// End-to-end wiring: an experiment evaluated through the Client must land in the
// sink, while a plain rollout must not.
func TestClientToRecorderIntegration(t *testing.T) {
	const ruleset = `{
	  "exp-flag": {
	    "defaultValue": "control",
	    "rules": [{
	      "key": "exp-key",
	      "hashAttribute": "guild_id",
	      "variations": ["control", "treatment"],
	      "weights": [0.5, 0.5],
	      "coverage": 1
	    }]
	  },
	  "rollout-flag": {
	    "defaultValue": false,
	    "rules": [{"force": true, "coverage": 1, "hashAttribute": "guild_id"}]
	  }
	}`

	sink := &fakeSink{}
	recorder := NewRecorder(zap.NewNop(), sink, newFakeDeduper(), RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	client, err := NewOffline(context.Background(), zap.NewNop(), ruleset, recorder)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	client.IsEnabled(ctx, "rollout-flag", ForGuild(1234))
	client.Eval(ctx, "exp-flag", ForGuild(1234))

	eventually(t, func() bool { return len(sink.all()) == 1 })

	written := sink.all()[0]
	require.Equal(t, "exp-key", written.ExperimentKey)
	require.Equal(t, AttrGuild, written.IdentifierType)
	require.Equal(t, "1234", written.Identifier)
	require.False(t, written.ExposedAt.IsZero())
}

// SinkFunc is how services supply the Postgres write without this package
// importing the database module.
func TestSinkFuncAdaptsAFunction(t *testing.T) {
	var got []RecordedExposure

	sink := SinkFunc(func(_ context.Context, exposures []RecordedExposure) error {
		got = append(got, exposures...)
		return nil
	})

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})

	recorder.Record(context.Background(), exposure("exp", 7))
	require.NoError(t, recorder.Close())

	require.Len(t, got, 1)
	require.Equal(t, "7", got[0].Identifier)
}

func TestSinkFuncPropagatesErrors(t *testing.T) {
	sink := SinkFunc(func(_ context.Context, _ []RecordedExposure) error {
		return errors.New("write failed")
	})

	recorder := NewRecorder(zap.NewNop(), sink, nil, RecorderConfig{
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	t.Cleanup(func() { require.NoError(t, recorder.Close()) })

	recorder.Record(context.Background(), exposure("exp", 1))

	eventually(t, func() bool { return recorder.Stats().FailedWrites == 1 })
}

// Services hold these as package-level variables assigned during startup, so a
// read before assignment must degrade rather than panic. This is the failure mode
// the old settable singleton had.
func TestNilReceiversAreSafe(t *testing.T) {
	var client *Client
	var recorder *Recorder

	ctx := context.Background()

	require.NotPanics(t, func() {
		require.False(t, client.IsEnabled(ctx, "flag", ForGuild(1)))
		require.Equal(t, "def", client.StringValue(ctx, "flag", ForGuild(1), "def"))
		require.Equal(t, 5, client.IntValue(ctx, "flag", ForGuild(1), 5))
		require.Equal(t, Result{}, client.Eval(ctx, "flag", ForGuild(1)))
		require.NoError(t, client.Close())

		recorder.Record(ctx, exposure("exp", 1))
	})
}

func TestRotatingSetBasics(t *testing.T) {
	set := newRotatingSet(1000, time.Hour)

	require.False(t, set.contains("a"))
	set.add("a")
	require.True(t, set.contains("a"))
	require.False(t, set.contains("b"))
}

// Size-triggered rotation is what bounds memory when a high-coverage experiment
// is running across 500k guilds on a 256Mi pod.
func TestRotatingSetRotatesOnSize(t *testing.T) {
	set := newRotatingSet(10, time.Hour)

	for i := 0; i < 10; i++ {
		set.add(strconv.Itoa(i))
	}

	// The 11th insert rotates: current becomes previous, a fresh map starts.
	set.add("trigger")

	// Still visible via the previous generation.
	require.True(t, set.contains("0"))
	require.True(t, set.contains("trigger"))

	// Filling a second generation evicts the first.
	for i := 0; i < 10; i++ {
		set.add("second-" + strconv.Itoa(i))
	}
	set.add("trigger-2")

	require.False(t, set.contains("0"), "first generation should have been evicted")
	require.True(t, set.contains("trigger-2"))
	require.LessOrEqual(t, set.len(), 22)
}

func TestRotatingSetRotatesOnAge(t *testing.T) {
	set := newRotatingSet(1_000_000, time.Minute)

	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	set.now = func() time.Time { return now }
	set.lastRotate = now

	set.add("first")
	require.True(t, set.contains("first"))

	now = now.Add(2 * time.Minute)
	set.add("second")
	require.True(t, set.contains("first"), "one rotation keeps the previous generation")

	now = now.Add(2 * time.Minute)
	set.add("third")
	require.False(t, set.contains("first"), "two rotations evict")
	require.True(t, set.contains("second"))
	require.True(t, set.contains("third"))
}

func TestRotatingSetIsConcurrencySafe(t *testing.T) {
	set := newRotatingSet(500, 10*time.Millisecond)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := strconv.Itoa(offset*500 + i)
				set.add(key)
				set.contains(key)
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkRecordSuppressed(b *testing.B) {
	recorder := NewRecorder(zap.NewNop(), &fakeSink{}, nil, RecorderConfig{
		QueueSize:     1024,
		BatchSize:     1000,
		FlushInterval: time.Hour,
	})
	defer func() { _ = recorder.Close() }()

	ctx := context.Background()
	e := exposure("exp", 1)
	recorder.Record(ctx, e)

	b.ReportAllocs()
	b.ResetTimer()

	// The steady-state hot path: a unit already recorded, so no I/O and no queue.
	for i := 0; i < b.N; i++ {
		recorder.Record(ctx, e)
	}
}
