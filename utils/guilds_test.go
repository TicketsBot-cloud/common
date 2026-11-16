package utils

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TicketsBot-cloud/gdl/objects/guild"
	"github.com/TicketsBot-cloud/gdl/rest/request"
	"github.com/stretchr/testify/require"
)

func TestSplitOwnedGuildsKeepsInputOrder(t *testing.T) {
	owners := map[uint64]uint64{1: 7, 2: 8, 4: 7}

	fetch := func(_ context.Context, guildId uint64) (guild.Guild, error) {
		// Higher IDs finish later, so completion order is the reverse of the input order below.
		time.Sleep(time.Duration(guildId) * 10 * time.Millisecond)

		switch guildId {
		case 3:
			return guild.Guild{}, request.RestError{StatusCode: http.StatusForbidden}
		case 5:
			return guild.Guild{}, request.RestError{StatusCode: http.StatusNotFound}
		}
		return guild.Guild{Id: guildId, Name: "g", OwnerId: owners[guildId]}, nil
	}

	owned, notOwned, unreachable, err := SplitOwnedGuilds(context.Background(), 7, []uint64{5, 4, 3, 2, 1}, fetch)
	require.NoError(t, err)
	require.Equal(t, []guild.Guild{{Id: 4, Name: "g", OwnerId: 7}, {Id: 1, Name: "g", OwnerId: 7}}, owned)
	require.Equal(t, []uint64{2}, notOwned)
	require.Equal(t, []uint64{5, 3}, unreachable)
}

func TestSplitOwnedGuildsReturnsOtherErrors(t *testing.T) {
	for _, fetchErr := range []error{request.RestError{StatusCode: http.StatusInternalServerError}, context.DeadlineExceeded} {
		fetch := func(_ context.Context, guildId uint64) (guild.Guild, error) {
			if guildId == 2 {
				return guild.Guild{}, fetchErr
			}
			return guild.Guild{Id: guildId, OwnerId: 7}, nil
		}

		owned, notOwned, unreachable, err := SplitOwnedGuilds(context.Background(), 7, []uint64{1, 2}, fetch)
		require.Equal(t, fetchErr, err)
		require.Nil(t, owned)
		require.Nil(t, notOwned)
		require.Nil(t, unreachable)
	}
}

func TestSplitOwnedGuildsLimitsConcurrency(t *testing.T) {
	var inFlight, peak atomic.Int32
	fetch := func(_ context.Context, guildId uint64) (guild.Guild, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		return guild.Guild{Id: guildId, OwnerId: 7}, nil
	}

	guildIds := make([]uint64, 30)
	for i := range guildIds {
		guildIds[i] = uint64(i + 1)
	}

	owned, _, _, err := SplitOwnedGuilds(context.Background(), 7, guildIds, fetch)
	require.NoError(t, err)
	require.Len(t, owned, 30)
	require.Equal(t, int32(10), peak.Load())
}
