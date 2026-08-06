package featureflags

import (
	"hash/maphash"
	"sync"
	"time"
)

// rotatingSet is a bounded, approximate "have I seen this recently" set. It sits
// in front of the cross-process deduplication check so repeat traffic for a unit
// already recorded costs nothing but a read lock.
//
// Two design choices are driven by the worker's 256Mi memory limit:
//
// Keys are stored as 64-bit hashes rather than strings. A worker pod consumes
// gateway events from a Redis Stream consumer group, so any pod can see any
// guild; at 500k guilds across several live experiments the string form would run
// to tens of megabytes per pod. Hashing costs a false-positive rate of roughly
// n^2/2^65, which at these sizes is around one in ten billion. A false positive
// skips one exposure for one unit, which cannot meaningfully move an experiment
// result, and no false negatives are possible.
//
// Entries expire by generation rather than per-key TTL: a full map is dropped
// wholesale, which is cheap and needs no timestamps. Effective lifetime is
// therefore between one and two rotation intervals.
type rotatingSet struct {
	mu       sync.RWMutex
	seed     maphash.Seed
	current  map[uint64]struct{}
	previous map[uint64]struct{}

	maxEntries  int
	rotateEvery time.Duration
	lastRotate  time.Time

	// now is overridable so rotation can be tested without sleeping.
	now func() time.Time
}

func newRotatingSet(maxEntries int, rotateEvery time.Duration) *rotatingSet {
	return &rotatingSet{
		seed:        maphash.MakeSeed(),
		current:     make(map[uint64]struct{}),
		previous:    make(map[uint64]struct{}),
		maxEntries:  maxEntries,
		rotateEvery: rotateEvery,
		now:         time.Now,
		lastRotate:  time.Now(),
	}
}

func (s *rotatingSet) contains(key string) bool {
	hashed := maphash.String(s.seed, key)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.current[hashed]; ok {
		return true
	}

	_, ok := s.previous[hashed]
	return ok
}

func (s *rotatingSet) add(key string) {
	hashed := maphash.String(s.seed, key)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Rotate on age or on size, whichever comes first. The size trigger is what
	// keeps the footprint bounded when a high-coverage experiment is running.
	if s.now().Sub(s.lastRotate) >= s.rotateEvery || len(s.current) >= s.maxEntries {
		s.previous = s.current
		s.current = make(map[uint64]struct{})
		s.lastRotate = s.now()
	}

	s.current[hashed] = struct{}{}
}

func (s *rotatingSet) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.current) + len(s.previous)
}
