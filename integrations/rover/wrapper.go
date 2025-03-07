package rover

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TicketsBot-cloud/common/integrations/roblox"
	"github.com/TicketsBot-cloud/common/webproxy"
	"github.com/go-redis/redis/v8"
)

type (
	RoverIntegration struct {
		redis  *redis.Client
		proxy  *webproxy.WebProxy
		apiKey string
	}

	cachedUser struct {
		User *roblox.User `json:"user"` // If the user does not exist, this will be nil, making a separate bool redundant
	}
)

func NewRoverIntegration(redis *redis.Client, proxy *webproxy.WebProxy, apiKey string) *RoverIntegration {
	return &RoverIntegration{
		redis:  redis,
		proxy:  proxy,
		apiKey: apiKey,
	}
}

func newCachedUser(user roblox.User) cachedUser {
	return cachedUser{
		User: &user,
	}
}

func newNullUser() cachedUser {
	return cachedUser{
		User: nil,
	}
}

const cacheLength = time.Hour * 24

func (i *RoverIntegration) GetRobloxUser(ctx context.Context, guildId, discordUserId uint64) (roblox.User, error) {
	redisKey := fmt.Sprintf("rover:%d", discordUserId)

	// See if we have a cached value
	cached, err := i.redis.Get(ctx, redisKey).Result()
	if err == nil {
		var user cachedUser
		if err := json.Unmarshal([]byte(cached), &user); err != nil {
			return roblox.User{}, err
		}

		if user.User == nil {
			return roblox.User{}, ErrUserNotFound
		} else {
			return *user.User, nil
		}
	} else if err != redis.Nil { // If the error is redis.Nil, this means that the key does not exist, and we should continue
		return roblox.User{}, err
	}

	// Fetch user ID from Bloxlink
	robloxId, err := RequestUserId(ctx, i.proxy, i.apiKey, guildId, discordUserId)
	if err != nil {
		if err == ErrUserNotFound { // If user not found, we should still cache this
			encoded, err := json.Marshal(newNullUser())
			if err != nil {
				return roblox.User{}, err
			}

			i.redis.SetEX(context.Background(), redisKey, encoded, cacheLength)
		}

		return roblox.User{}, err
	}

	// Fetch user object
	user, err := roblox.RequestUserData(ctx, i.proxy, robloxId)
	if err != nil {
		return roblox.User{}, err
	}

	// Cache response
	encoded, err := json.Marshal(newCachedUser(user))
	if err != nil {
		return roblox.User{}, err
	}

	i.redis.SetEX(ctx, redisKey, string(encoded), cacheLength)

	return user, nil
}
