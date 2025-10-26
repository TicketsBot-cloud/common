package experiments

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TicketsBot-cloud/database"
	"github.com/go-redis/redis/v8"
)

type Experiment string

const (
	COMPONENTS_V2_STATISTICS Experiment = "COMPONENTS_V2_STATISTICS"
	API_BASED_FORM_INPUTS    Experiment = "API_BASED_FORM_INPUTS"
)

var List = []Experiment{
	COMPONENTS_V2_STATISTICS,
	API_BASED_FORM_INPUTS,
}

type Manager struct {
	redisClient *redis.Client
	dbClient    *database.Database
}

var (
	globalManager *Manager
	managerMu     sync.RWMutex
)

func NewManager(redisClient *redis.Client, dbClient *database.Database) *Manager {
	return &Manager{
		redisClient: redisClient,
		dbClient:    dbClient,
	}
}

func SetGlobalManager(m *Manager) {
	managerMu.Lock()
	defer managerMu.Unlock()
	globalManager = m
}

func GetGlobalManager() *Manager {
	managerMu.RLock()
	defer managerMu.RUnlock()
	return globalManager
}

func (m *Manager) HasFeature(ctx context.Context, guildId uint64, experiment Experiment) bool {
	if os.Getenv("ENABLE_ALL_EXPERIMENTS") == "true" {
		return true
	}

	experimentServers := strings.Split(os.Getenv("EXPERIMENT_SERVERS"), ",")
	if slices.Contains(experimentServers, strconv.FormatUint(guildId, 10)) {
		return true
	}

	rolloutPercentage := 0

	redisPercentage, err := m.GetExperimentRolloutPercentage(ctx, strings.ToLower(string(experiment)))
	if err == nil {
		rolloutPercentage = redisPercentage
	} else {
		if err == redis.Nil {
			// Key does not exist, check database
			dbExperiment, dbErr := m.dbClient.Experiment.GetByName(ctx, string(experiment))
			if dbErr != nil || dbExperiment == nil {
				// If we can't find it in the database, default to 0%
				rolloutPercentage = 0
			} else {
				rolloutPercentage = dbExperiment.RolloutPercentage

				// Cache in Redis for future use
				_ = m.SetExperimentRolloutPercentage(ctx, strings.ToLower(string(experiment)), rolloutPercentage)
			}
		} else {
			rolloutPercentage = 0
		}
	}

	// If rollout is 100%, everyone is in the experiment
	if rolloutPercentage == 100 {
		return true
	}

	// If rollout is 0%, no one is in the experiment
	if rolloutPercentage == 0 {
		return false
	}

	return guildId%100 <= uint64(rolloutPercentage)
}

func (m *Manager) GetExperimentRolloutPercentage(ctx context.Context, experiment string) (int, error) {
	key := fmt.Sprintf("experiment:rollout:%s", experiment)

	percentage, err := m.redisClient.Get(ctx, key).Int()
	if err != nil {
		return 0, err
	}

	return percentage, nil
}

func (m *Manager) SetExperimentRolloutPercentage(ctx context.Context, experiment string, percentage int) error {
	key := fmt.Sprintf("experiment:rollout:%s", experiment)
	return m.redisClient.Set(ctx, key, percentage, 5*time.Minute).Err()
}
