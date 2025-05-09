package premium

import (
	"context"
	"time"

	"github.com/TicketsBot-cloud/common/model"
	"github.com/TicketsBot-cloud/common/sentry"
	"github.com/TicketsBot-cloud/gdl/objects/guild"
)

const GracePeriod = time.Hour // TODO: Reduce this to zero?

func (p *PremiumLookupClient) GetTierByGuild(ctx context.Context, guild guild.Guild) (_tier PremiumTier, _src model.EntitlementSource, _err error) {
	_tier = None
	_src = ""

	defer func() {
		// cache result
		if _err == nil {
			go func() {
				err := p.SetCachedTier(ctx, guild.Id, CachedTier{
					Tier:   int8(_tier),
					Source: _src,
				})

				if err != nil {
					sentry.Error(err)
				}
			}()
		}
	}()

	// check entitlements db
	subscriptions, err := sentry.WithSpan2(ctx, "ListGuildSubscriptions", func(span *sentry.Span) ([]model.GuildEntitlementEntry, error) {
		return p.database.Entitlements.ListGuildSubscriptions(ctx, guild.Id, guild.OwnerId, GracePeriod)
	})
	if err != nil {
		return None, "", err
	}

	if maxSubscription := findMaxTier(subscriptions); maxSubscription != nil {
		return TierFromEntitlement(maxSubscription.Tier), maxSubscription.Source, nil
	}

	return None, "", nil
}

func findMaxTier(subscriptions []model.GuildEntitlementEntry) *model.GuildEntitlementEntry {
	if len(subscriptions) == 0 {
		return nil
	}

	maxTier := subscriptions[0]
	for _, entry := range subscriptions[1:] {
		if entry.SkuPriority > maxTier.SkuPriority {
			maxTier = entry
		}
	}

	return &maxTier
}
