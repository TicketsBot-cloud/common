package featureflags

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// rolloutRuleset defines two flags at the same 10% coverage, keyed on guild_id.
// GrowthBook seeds the rollout hash with the feature key, so the two are expected
// to enrol different guilds. The old implementation used guildId % 100 with no
// seed, which made every flag select the identical cohort.
const rolloutRuleset = `{
  "flag-a": {
    "defaultValue": false,
    "rules": [{"force": true, "coverage": 0.1, "hashAttribute": "guild_id"}]
  },
  "flag-b": {
    "defaultValue": false,
    "rules": [{"force": true, "coverage": 0.1, "hashAttribute": "guild_id"}]
  },
  "flag-none": {
    "defaultValue": false,
    "rules": [{"force": true, "coverage": 0, "hashAttribute": "guild_id"}]
  },
  "flag-all": {
    "defaultValue": false,
    "rules": [{"force": true, "coverage": 1, "hashAttribute": "guild_id"}]
  }
}`

func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zap.NewNop()
}

// snowflakes builds IDs shaped like real Discord snowflakes: a millisecond
// timestamp in the high bits, then worker, process and a sequence counter in the
// low 22 bits. The low bits are the reason a plain modulo is not a fair hash.
func snowflakes(n int) []uint64 {
	ids := make([]uint64, 0, n)
	base := uint64(1_700_000_000_000)

	for i := 0; i < n; i++ {
		ms := base + uint64(i*37)
		worker := uint64(i % 8)
		process := uint64(i % 4)
		seq := uint64(i % 4096)
		ids = append(ids, (ms<<22)|(worker<<17)|(process<<12)|seq)
	}

	return ids
}

func TestConfigEnabled(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		expected bool
	}{
		{"both set", Config{ApiHost: "https://gb", ClientKey: "sdk-1"}, true},
		{"host missing", Config{ClientKey: "sdk-1"}, false},
		{"key missing", Config{ApiHost: "https://gb"}, false},
		{"empty", Config{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.cfg.Enabled())
		})
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{LoadTimeout: time.Second, PollInterval: time.Minute, CacheRefreshInterval: time.Minute, UseSSE: true}

	tests := []struct {
		name    string
		mutate  func(Config) Config
		wantErr bool
	}{
		{"valid", func(c Config) Config { return c }, false},
		{"zero load timeout", func(c Config) Config { c.LoadTimeout = 0; return c }, true},
		{"negative load timeout", func(c Config) Config { c.LoadTimeout = -time.Second; return c }, true},
		{"zero cache refresh", func(c Config) Config { c.CacheRefreshInterval = 0; return c }, true},
		{"poll interval irrelevant while SSE on", func(c Config) Config { c.PollInterval = 0; return c }, false},
		{"poll interval required without SSE", func(c Config) Config { c.UseSSE = false; c.PollInterval = 0; return c }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mutate(valid).validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAttributesSnowflakesStayExactStrings(t *testing.T) {
	// Above 2^53 a float64 can no longer represent consecutive integers, so a
	// snowflake passed as a number would be rounded and could change bucket.
	const guildId uint64 = 1328073426023219221
	require.Greater(t, guildId, uint64(1)<<53)

	attrs := ForGuild(guildId).toGrowthBook()

	require.Equal(t, "1328073426023219221", attrs[AttrGuild])
	require.Equal(t, "1328073426023219221", attrs["id"])
	require.IsType(t, "", attrs[AttrGuild])
}

func TestAttributesPrimaryUnit(t *testing.T) {
	tests := []struct {
		name        string
		attrs       Attributes
		wantId      string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "guild",
			attrs:       ForGuild(10),
			wantId:      "10",
			wantPresent: []string{AttrGuild},
			wantAbsent:  []string{AttrUser, AttrDashboardUser},
		},
		{
			name:        "user carries guild but buckets on user",
			attrs:       ForUser(10, 20),
			wantId:      "20",
			wantPresent: []string{AttrGuild, AttrUser},
			wantAbsent:  []string{AttrDashboardUser},
		},
		{
			name:        "dashboard user",
			attrs:       ForDashboardUser(30),
			wantId:      "30",
			wantPresent: []string{AttrDashboardUser},
			wantAbsent:  []string{AttrGuild, AttrUser},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.attrs.toGrowthBook()
			require.Equal(t, tt.wantId, got["id"])

			for _, key := range tt.wantPresent {
				require.Contains(t, got, key)
			}
			for _, key := range tt.wantAbsent {
				require.NotContains(t, got, key)
			}
		})
	}
}

func TestAttributesOptionalTargeting(t *testing.T) {
	attrs := ForGuild(1).
		WithPremiumTier(1).
		WithEntitlementSource("patreon").
		WithShard(7).
		WithGuildSize(4200).
		WithExtra("region", "eu").
		toGrowthBook()

	require.Equal(t, 1, attrs["premium_tier"])
	require.Equal(t, "patreon", attrs["entitlement_source"])
	require.Equal(t, 7, attrs["shard"])
	require.Equal(t, 4200, attrs["guild_size"])
	require.Equal(t, "eu", attrs["region"])
}

func TestAttributesStaffTier(t *testing.T) {
	attrs := ForDashboardUser(1).WithStaffTier("admin").toGrowthBook()
	require.Equal(t, "admin", attrs[AttrStaffTier])

	// Empty means not staff, and must be absent rather than "" so a rule matching
	// on presence does not fire for everyone.
	require.NotContains(t, ForDashboardUser(1).WithStaffTier("").toGrowthBook(), AttrStaffTier)
}

// WithGuild lets a dashboard-user evaluation carry a guild ID, so "Specific
// servers" and "Percentage of servers" rules can match on a dashboard flag. It
// must not change which attribute is the bucketing unit.
func TestAttributesWithGuildOnDashboardUser(t *testing.T) {
	attrs := ForDashboardUser(30).WithGuild(10).toGrowthBook()

	require.Equal(t, "30", attrs[AttrDashboardUser])
	require.Equal(t, "10", attrs[AttrGuild])

	// The bucketing fallback must still mirror the primary unit, not the guild
	// that was merely attached for targeting.
	require.Equal(t, "30", attrs["id"])
}

// Staff targeting is the one attribute a caller must remember to supply. Prove a
// staff rule does not match when it is absent, since a silent non-match is the
// failure mode.
func TestStaffRuleRequiresStaffAttribute(t *testing.T) {
	const ruleset = `{
	  "staff-only": {
	    "defaultValue": false,
	    "rules": [{
	      "condition": {"staff_tier": {"$in": ["helper", "admin", "owner"]}},
	      "force": true
	    }]
	  }
	}`

	client, err := NewOffline(context.Background(), testLogger(t), ruleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()

	require.True(t, client.IsEnabled(ctx, "staff-only", ForDashboardUser(1).WithStaffTier("helper")))
	require.False(t, client.IsEnabled(ctx, "staff-only", ForDashboardUser(1)))
}

func TestAttributesOmitsUnsetOptionalTargeting(t *testing.T) {
	// Absent must differ from zero: a guild with no premium must not match a
	// premium_tier == 0 rule, which is the Premium tier.
	attrs := ForGuild(1).toGrowthBook()

	require.NotContains(t, attrs, "premium_tier")
	require.NotContains(t, attrs, "shard")
	require.NotContains(t, attrs, "guild_size")
	require.NotContains(t, attrs, "entitlement_source")
}

func TestAttributesWithExtraDoesNotMutateBase(t *testing.T) {
	base := ForGuild(1).WithExtra("a", 1)
	first := base.WithExtra("b", 2)
	second := base.WithExtra("c", 3)

	require.NotContains(t, base.toGrowthBook(), "b")
	require.NotContains(t, first.toGrowthBook(), "c")
	require.NotContains(t, second.toGrowthBook(), "b")
	require.Contains(t, first.toGrowthBook(), "b")
	require.Contains(t, second.toGrowthBook(), "c")
}

// A client with no GrowthBook configured must evaluate everything to its zero
// value and never fail. This is the self-hosted and local-development path.
func TestDegradedClientEvaluatesToZeroValues(t *testing.T) {
	client, err := New(
		context.Background(),
		Config{LoadTimeout: time.Second, CacheRefreshInterval: time.Minute, UseSSE: true},
		testLogger(t),
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	attrs := ForGuild(1)

	require.False(t, client.IsEnabled(ctx, "anything", attrs))
	require.Equal(t, "fallback", client.StringValue(ctx, "anything", attrs, "fallback"))
	require.Equal(t, 42, client.IntValue(ctx, "anything", attrs, 42))
	require.Equal(t, Result{}, client.Eval(ctx, "anything", attrs))
}

// Polling is the default data source because a stock self-hosted GrowthBook does
// not serve SSE. This covers the real path end to end against a stub serving the
// documented payload shape.
func TestPollDataSourceLoadsPayload(t *testing.T) {
	var requested string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status": 200,
			"features": {
				"202608_NEW_PRICING": {"defaultValue": true},
				"off-by-default": {"defaultValue": false}
			},
			"dateUpdated": "2026-08-06T07:59:44.699Z"
		}`))
	}))
	t.Cleanup(server.Close)

	client, err := New(
		context.Background(),
		Config{
			ApiHost:              server.URL,
			ClientKey:            "sdk-test",
			UseSSE:               false,
			PollInterval:         time.Minute,
			LoadTimeout:          5 * time.Second,
			CacheRefreshInterval: time.Hour,
		},
		testLogger(t),
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	require.True(t, client.IsEnabled(ctx, "202608_NEW_PRICING", ForGuild(1)))
	require.False(t, client.IsEnabled(ctx, "off-by-default", ForGuild(1)))
	require.Contains(t, requested, "sdk-test", "SDK should request the payload for the client key")
}

// An unreachable backend must not stop startup, and must not return an error from
// New: the process boots with every flag off.
func TestUnreachableBackendStillStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	client, err := New(
		context.Background(),
		Config{
			ApiHost:              server.URL,
			ClientKey:            "sdk-test",
			UseSSE:               false,
			PollInterval:         time.Minute,
			LoadTimeout:          time.Second,
			CacheRefreshInterval: time.Hour,
		},
		testLogger(t),
		nil,
		nil,
	)
	require.NoError(t, err, "an unreachable backend must not fail startup")
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.False(t, client.IsEnabled(context.Background(), "anything", ForGuild(1)))
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	_, err := New(context.Background(), Config{}, testLogger(t), nil, nil)
	require.Error(t, err)
}

func TestUnknownFlagIsOff(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.False(t, client.IsEnabled(context.Background(), "no-such-flag", ForGuild(1)))
}

func TestRolloutBoundaries(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	ids := snowflakes(2000)

	tests := []struct {
		flag     string
		expected bool
	}{
		{"flag-none", false},
		{"flag-all", true},
	}

	for _, tt := range tests {
		t.Run(tt.flag, func(t *testing.T) {
			for _, id := range ids {
				require.Equal(t, tt.expected, client.IsEnabled(ctx, tt.flag, ForGuild(id)),
					"guild %d", id)
			}
		})
	}
}

// The distribution must actually be near the configured percentage over realistic
// snowflakes. guildId % 100 was both biased by the snowflake's low bits and off
// by one, so a nominal 10% was neither 10% nor uniform.
func TestRolloutHitsTargetPercentage(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	ids := snowflakes(20000)

	var enrolled int
	for _, id := range ids {
		if client.IsEnabled(ctx, "flag-a", ForGuild(id)) {
			enrolled++
		}
	}

	ratio := float64(enrolled) / float64(len(ids))
	// 3 sigma for n=20000, p=0.1 is about 0.6 percentage points; allow 1.5.
	require.InDelta(t, 0.1, ratio, 0.015, "enrolled %d of %d", enrolled, len(ids))
}

// Two flags at the same percentage must not select the same guilds, otherwise
// concurrent experiments contaminate each other.
func TestConcurrentFlagsEnrolIndependentCohorts(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	ids := snowflakes(20000)

	cohortA := map[uint64]struct{}{}
	cohortB := map[uint64]struct{}{}

	for _, id := range ids {
		if client.IsEnabled(ctx, "flag-a", ForGuild(id)) {
			cohortA[id] = struct{}{}
		}
		if client.IsEnabled(ctx, "flag-b", ForGuild(id)) {
			cohortB[id] = struct{}{}
		}
	}

	require.NotEmpty(t, cohortA)
	require.NotEmpty(t, cohortB)

	var overlap int
	for id := range cohortA {
		if _, ok := cohortB[id]; ok {
			overlap++
		}
	}

	// Independent 10% samples should overlap on roughly 10% of cohort A. Insist
	// it is well under half, which a shared-cohort implementation could never do:
	// the old modulo scheme would have produced complete overlap.
	require.Less(t, float64(overlap)/float64(len(cohortA)), 0.5,
		"cohorts overlap on %d of %d guilds", overlap, len(cohortA))
}

// Assignment must be a pure function of flag and unit, so a guild does not flip
// between variants across evaluations, pods or restarts.
func TestAssignmentIsStableAcrossClients(t *testing.T) {
	ctx := context.Background()
	ids := snowflakes(500)

	first, err := NewOffline(ctx, testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })

	second, err := NewOffline(ctx, testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	for _, id := range ids {
		want := first.IsEnabled(ctx, "flag-a", ForGuild(id))

		// Repeated within the same client.
		require.Equal(t, want, first.IsEnabled(ctx, "flag-a", ForGuild(id)), "guild %d", id)
		// And in a separately constructed client, standing in for another pod.
		require.Equal(t, want, second.IsEnabled(ctx, "flag-a", ForGuild(id)), "guild %d", id)
	}
}

func TestEvalIsConcurrencySafe(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	const goroutines = 16
	const perGoroutine = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				guildId := uint64(offset*perGoroutine + i + 1)
				client.IsEnabled(ctx, "flag-a", ForGuild(guildId))
			}
		}(g)
	}
	wg.Wait()
}

func TestMultivariateValues(t *testing.T) {
	const ruleset = `{
	  "greeting": {"defaultValue": "hello"},
	  "limit": {"defaultValue": 25},
	  "switch": {"defaultValue": true}
	}`

	client, err := NewOffline(context.Background(), testLogger(t), ruleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	attrs := ForGuild(1)

	require.Equal(t, "hello", client.StringValue(ctx, "greeting", attrs, "fallback"))
	// Whole numbers arrive from JSON as float64; IntValue hides that.
	require.Equal(t, 25, client.IntValue(ctx, "limit", attrs, 0))
	require.True(t, client.IsEnabled(ctx, "switch", attrs))

	// Type mismatches fall back rather than panicking.
	require.Equal(t, "fallback", client.StringValue(ctx, "limit", attrs, "fallback"))
	require.Equal(t, 99, client.IntValue(ctx, "greeting", attrs, 99))
}

type recordingRecorder struct {
	mu        sync.Mutex
	exposures []Exposure
}

func (r *recordingRecorder) Record(_ context.Context, exposure Exposure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exposures = append(r.exposures, exposure)
}

func (r *recordingRecorder) all() []Exposure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Exposure(nil), r.exposures...)
}

// A plain percentage rollout is not an experiment. Recording exposures for one
// would fill the assignment table with rows no analysis wants.
func TestRolloutDoesNotRecordExposures(t *testing.T) {
	recorder := &recordingRecorder{}

	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, recorder)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	for _, id := range snowflakes(500) {
		client.IsEnabled(ctx, "flag-a", ForGuild(id))
	}

	require.Empty(t, recorder.all())
}

// An experiment rule does record, and carries the identifier the assignment
// query needs.
func TestExperimentRecordsExposure(t *testing.T) {
	const ruleset = `{
	  "checkout-copy": {
	    "defaultValue": "control",
	    "rules": [{
	      "key": "checkout-copy-test",
	      "hashAttribute": "guild_id",
	      "variations": ["control", "treatment"],
	      "weights": [0.5, 0.5],
	      "coverage": 1
	    }]
	  }
	}`

	recorder := &recordingRecorder{}

	client, err := NewOffline(context.Background(), testLogger(t), ruleset, recorder)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	ctx := context.Background()
	const guildId uint64 = 1328073426023219221

	result := client.Eval(ctx, "checkout-copy", ForGuild(guildId))
	require.True(t, result.InExperiment)

	exposures := recorder.all()
	require.Len(t, exposures, 1)

	exposure := exposures[0]
	require.Equal(t, "checkout-copy-test", exposure.ExperimentKey)
	require.Equal(t, AttrGuild, exposure.IdentifierType)
	require.Equal(t, strconv.FormatUint(guildId, 10), exposure.Identifier)
	require.Equal(t, "checkout-copy", exposure.FeatureKey)
	require.Contains(t, []int{0, 1}, exposure.VariationId)
	require.Equal(t, result.VariationId, exposure.VariationId)
}

func TestNilRecorderIsSafe(t *testing.T) {
	const ruleset = `{
	  "exp": {
	    "defaultValue": "control",
	    "rules": [{
	      "key": "exp-test",
	      "hashAttribute": "guild_id",
	      "variations": ["control", "treatment"],
	      "weights": [0.5, 0.5],
	      "coverage": 1
	    }]
	  }
	}`

	client, err := NewOffline(context.Background(), testLogger(t), ruleset, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	require.NotPanics(t, func() {
		client.Eval(context.Background(), "exp", ForGuild(1))
	})
}

func TestCloseIsIdempotent(t *testing.T) {
	client, err := NewOffline(context.Background(), testLogger(t), rolloutRuleset, nil)
	require.NoError(t, err)

	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
}

func TestSlogAdapterForwardsToZap(t *testing.T) {
	// Exercises the bridge with attributes and groups so a malformed field cannot
	// panic inside the SDK's logging path.
	logger := newSlogAdapter(zap.NewNop())

	require.NotPanics(t, func() {
		logger.Info("plain")
		logger.With("key", "value").Warn("with attr")
		logger.WithGroup("outer").With("inner", 1).Error("grouped")
		logger.Info("mixed", "count", 3, "ok", true)
	})
}

func BenchmarkIsEnabled(b *testing.B) {
	client, err := NewOffline(context.Background(), zap.NewNop(), rolloutRuleset, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	ids := snowflakes(1024)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		client.IsEnabled(ctx, "flag-a", ForGuild(ids[i%len(ids)]))
	}
}
