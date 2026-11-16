package utils

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/TicketsBot-cloud/gdl/objects/guild"
	"github.com/TicketsBot-cloud/gdl/rest/request"
	"golang.org/x/sync/errgroup"
)

func FormatGuildDisplay(guildId uint64, name string) string {
	if name != "" {
		return name + " (" + strconv.FormatUint(guildId, 10) + ")"
	}
	return strconv.FormatUint(guildId, 10)
}

// Only a 403 or 404 means the bot can't see a guild; any other error leaves ownership unknown, so it is returned.
func SplitOwnedGuilds(ctx context.Context, userId uint64, guildIds []uint64, fetch func(context.Context, uint64) (guild.Guild, error)) (owned []guild.Guild, notOwned, unreachable []uint64, err error) {
	guilds := make([]guild.Guild, len(guildIds))
	errs := make([]error, len(guildIds))

	var group errgroup.Group
	group.SetLimit(10)
	for i, guildId := range guildIds {
		group.Go(func() error {
			guilds[i], errs[i] = fetch(ctx, guildId)
			return nil
		})
	}
	_ = group.Wait()

	for i, guildId := range guildIds {
		var restErr request.RestError
		switch {
		case errors.As(errs[i], &restErr) && (restErr.StatusCode == http.StatusForbidden || restErr.StatusCode == http.StatusNotFound):
			unreachable = append(unreachable, guildId)
		case errs[i] != nil:
			return nil, nil, nil, errs[i]
		case guilds[i].OwnerId != userId:
			notOwned = append(notOwned, guildId)
		default:
			owned = append(owned, guilds[i])
		}
	}

	return owned, notOwned, unreachable, nil
}
