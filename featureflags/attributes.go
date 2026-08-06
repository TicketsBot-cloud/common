package featureflags

import (
	"strconv"

	gb "github.com/growthbook/growthbook-golang"
)

// Identifier attribute names. These are the values a GrowthBook experiment's
// "assignment attribute" must be set to, and they double as the identifier_type
// recorded against an exposure.
const (
	AttrGuild         = "guild_id"
	AttrUser          = "user_id"
	AttrDashboardUser = "dashboard_user_id"
)

// Targeting attributes that are not assignment units. Rules match on these but
// never bucket on them.
const (
	AttrPremiumTier       = "premium_tier"
	AttrEntitlementSource = "entitlement_source"
	AttrShard             = "shard"
	AttrGuildSize         = "guild_size"
	AttrStaffTier         = "staff_tier"
)

// Attributes describes the entity a flag is being evaluated for. Build one with
// ForGuild, ForUser or ForDashboardUser so the bucketing unit is always explicit
// rather than inferred, then add optional targeting data with the With helpers.
//
// The old implementation bucketed on guildId % 100 with no per-flag salt, which
// meant every experiment enrolled the same guilds. GrowthBook hashes the primary
// identifier together with a per-flag seed, so cohorts are independent by
// construction. That only holds if the primary identifier is set, hence the
// constructors.
type Attributes struct {
	// primary is the attribute name GrowthBook buckets on by default.
	primary string

	guildId         uint64
	userId          uint64
	dashboardUserId uint64

	premiumTier       *int8
	entitlementSource string
	shard             *int
	guildSize         *int
	staffTier         string

	// extra carries attributes that do not warrant a field here. It exists so a
	// new targeting dimension does not require a tagged release of common and a
	// version bump in every consuming service.
	extra map[string]any
}

// ForGuild buckets on the guild, which is the right unit for most bot features.
func ForGuild(guildId uint64) Attributes {
	return Attributes{primary: AttrGuild, guildId: guildId}
}

// ForUser buckets on the Discord user. The guild is still recorded so rules can
// target both, but assignment follows the user across guilds.
func ForUser(guildId, userId uint64) Attributes {
	return Attributes{primary: AttrUser, guildId: guildId, userId: userId}
}

// ForDashboardUser buckets on the logged-in web user, for dashboard-only rollouts.
func ForDashboardUser(userId uint64) Attributes {
	return Attributes{primary: AttrDashboardUser, dashboardUserId: userId}
}

func (a Attributes) WithPremiumTier(tier int8) Attributes {
	a.premiumTier = &tier
	return a
}

func (a Attributes) WithEntitlementSource(source string) Attributes {
	a.entitlementSource = source
	return a
}

func (a Attributes) WithShard(shard int) Attributes {
	a.shard = &shard
	return a
}

func (a Attributes) WithGuildSize(size int) Attributes {
	a.guildSize = &size
	return a
}

// WithStaffTier marks the evaluation as being for a member of bot staff, so flags
// can be dogfooded internally before any customer sees them. Expects one of
// "helper", "admin" or "owner"; an empty string is treated as not staff.
//
// Callers must supply this for staff-targeted rules to match. A rule targeting
// staff at a call site that never sets it will silently never fire.
func (a Attributes) WithStaffTier(tier string) Attributes {
	a.staffTier = tier
	return a
}

// WithExtra attaches an arbitrary targeting attribute. Prefer a typed helper for
// anything used more than once.
func (a Attributes) WithExtra(key string, value any) Attributes {
	// Copy on write: Attributes is passed by value, so mutating a shared map
	// would leak between callers that derived from the same base.
	next := make(map[string]any, len(a.extra)+1)
	for k, v := range a.extra {
		next[k] = v
	}
	next[key] = value
	a.extra = next
	return a
}

// toGrowthBook converts to the SDK's attribute map.
//
// Snowflake IDs are always emitted as strings. They exceed the 2^53 integer
// range a JSON number can represent exactly, so passing them as numbers risks
// silent precision loss, which would move a guild between buckets.
func (a Attributes) toGrowthBook() gb.Attributes {
	attrs := make(gb.Attributes, 8+len(a.extra))

	for k, v := range a.extra {
		attrs[k] = v
	}

	if a.guildId != 0 {
		attrs[AttrGuild] = strconv.FormatUint(a.guildId, 10)
	}

	if a.userId != 0 {
		attrs[AttrUser] = strconv.FormatUint(a.userId, 10)
	}

	if a.dashboardUserId != 0 {
		attrs[AttrDashboardUser] = strconv.FormatUint(a.dashboardUserId, 10)
	}

	if a.premiumTier != nil {
		attrs[AttrPremiumTier] = int(*a.premiumTier)
	}

	if a.entitlementSource != "" {
		attrs[AttrEntitlementSource] = a.entitlementSource
	}

	if a.shard != nil {
		attrs[AttrShard] = *a.shard
	}

	if a.guildSize != nil {
		attrs[AttrGuildSize] = *a.guildSize
	}

	if a.staffTier != "" {
		attrs[AttrStaffTier] = a.staffTier
	}

	// GrowthBook falls back to the "id" attribute when a feature does not name an
	// assignment attribute of its own, so mirror the chosen primary onto it. This
	// makes a flag created in the UI with default settings bucket on the unit the
	// caller intended instead of silently not bucketing at all.
	if primary, ok := attrs[a.primary]; ok {
		attrs["id"] = primary
	}

	return attrs
}
