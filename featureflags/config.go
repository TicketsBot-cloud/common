package featureflags

import (
	"fmt"
	"time"
)

// Config describes how to reach GrowthBook. When both ApiHost and ClientKey are
// empty the whole subsystem degrades to compiled defaults rather than failing,
// so self-hosted deployments and local development work with no configuration.
// Setting only one of the two is rejected by validate as a broken config,
// rather than being treated the same as neither being set.
type Config struct {
	// ApiHost is the GrowthBook backend as reached from inside the cluster, so
	// http://growthbook-backend.growthbook.svc:3100 rather than the Twingate
	// alias. The alias exists for staff browsers; pointing services at it would
	// send in-cluster traffic out through the tunnel and back.
	ApiHost string `env:"GROWTHBOOK_API_HOST"`

	// ClientKey is an SDK connection key from GrowthBook, not an API key. It only
	// grants read access to the flag payload for one environment.
	ClientKey string `env:"GROWTHBOOK_CLIENT_KEY"`

	// DecryptionKey is only needed if the SDK connection in GrowthBook has
	// encrypted payloads enabled.
	DecryptionKey string `env:"GROWTHBOOK_DECRYPTION_KEY"`

	// UseSSE opts into streaming flag updates instead of polling.
	//
	// Defaults to false because a stock self-hosted GrowthBook does not serve
	// streams: the payload response carries no x-sse-support header and
	// /sub/<clientKey> returns 401. Streaming is a GrowthBook Proxy feature, so
	// only enable this once the proxy is deployed, otherwise startup fails to
	// load any rules and every flag evaluates to off.
	UseSSE bool `env:"GROWTHBOOK_USE_SSE" envDefault:"false"`

	// PollInterval is used when UseSSE is false. The SDK sends a conditional
	// request and gets a 304 when nothing changed, so a short interval is cheap.
	// This is also the upper bound on how long a kill switch takes to reach a
	// running process.
	PollInterval time.Duration `env:"GROWTHBOOK_POLL_INTERVAL" envDefault:"30s"`

	// LoadTimeout bounds how long startup waits for the first payload before
	// falling back to the cached copy in Redis. Keep it short: a process must
	// never block on GrowthBook being reachable.
	LoadTimeout time.Duration `env:"GROWTHBOOK_LOAD_TIMEOUT" envDefault:"5s"`

	// CacheRefreshInterval is how often the last-known-good payload is written
	// to Redis so restarting processes have something to boot from.
	CacheRefreshInterval time.Duration `env:"GROWTHBOOK_CACHE_REFRESH_INTERVAL" envDefault:"5m"`
}

// Enabled reports whether enough configuration is present to talk to GrowthBook.
func (c Config) Enabled() bool {
	return c.ApiHost != "" && c.ClientKey != ""
}

// Attempted reports whether any GrowthBook configuration was supplied at all,
// as opposed to neither ApiHost nor ClientKey being set.
//
// This is deliberately looser than Enabled: it is true for a valid config and
// also for a broken, partially-set one (only one of the two present). Callers
// deciding whether to fail closed on a construction error must use this, not
// Enabled, otherwise a config missing just one of the two env vars (a routine
// secret-rotation or ConfigMap mistake) is indistinguishable from "GrowthBook
// was never configured" and the caller fails open instead of closed. validate
// rejects a partial config outright, so in practice Attempted only needs to
// cover the error path from New; a config that reaches Enabled() has already
// passed validate and cannot be partial.
func (c Config) Attempted() bool {
	return c.ApiHost != "" || c.ClientKey != ""
}

func (c Config) validate() error {
	// Half-configured is worse than unconfigured: unconfigured deliberately
	// defaults every flag to enabled (see the package doc comment), but a config
	// missing just one of ApiHost/ClientKey would otherwise fall into that same
	// branch by accident, silently turning every kill switch on. Reject it
	// outright instead of letting Enabled() treat "one set" and "none set" the
	// same way.
	if (c.ApiHost == "") != (c.ClientKey == "") {
		return fmt.Errorf("featureflags: partial GrowthBook configuration (ApiHost set=%v, ClientKey set=%v): both must be set, or neither", c.ApiHost != "", c.ClientKey != "")
	}

	if c.LoadTimeout <= 0 {
		return fmt.Errorf("featureflags: LoadTimeout must be positive, got %s", c.LoadTimeout)
	}

	if !c.UseSSE && c.PollInterval <= 0 {
		return fmt.Errorf("featureflags: PollInterval must be positive when SSE is disabled, got %s", c.PollInterval)
	}

	if c.CacheRefreshInterval <= 0 {
		return fmt.Errorf("featureflags: CacheRefreshInterval must be positive, got %s", c.CacheRefreshInterval)
	}

	return nil
}
