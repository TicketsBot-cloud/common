package botpermissions

import (
	"github.com/TicketsBot-cloud/gdl/objects/channel"
	"github.com/TicketsBot-cloud/gdl/objects/guild"
	"github.com/TicketsBot-cloud/gdl/permission"
)

// EffectivePermissions computes the effective permission bitfield for a member in a
// channel following the standard Discord permission resolution algorithm:
// https://docs.discord.com/developers/topics/permissions#permission-overwrites
//
// All required data (roles, overwrites) must be pre-fetched by the caller.
func EffectivePermissions(
	guildId, userId uint64,
	memberRoles []uint64,
	overwrites []channel.PermissionOverwrite,
	roleMap map[uint64]guild.Role,
) uint64 {
	// Step 1: base = @everyone permissions OR'd with all member role permissions.
	// (@everyone role has the same ID as the guild.)
	var base uint64
	if everyoneRole, ok := roleMap[guildId]; ok {
		base = everyoneRole.Permissions
	}
	for _, roleId := range memberRoles {
		if r, ok := roleMap[roleId]; ok {
			base |= r.Permissions
		}
	}

	// Step 2: Administrator at guild level grants everything.
	if permission.HasPermissionRaw(base, permission.Administrator) {
		return ^uint64(0)
	}

	// Step 3: Build overwrite lookup.
	owMap := make(map[uint64]channel.PermissionOverwrite, len(overwrites))
	for _, ow := range overwrites {
		owMap[ow.Id] = ow
	}

	channelPerms := base

	// Step 4: Apply @everyone channel overwrite.
	if ow, ok := owMap[guildId]; ok {
		channelPerms &^= ow.Deny
		channelPerms |= ow.Allow
	}

	// Step 5: Collect and apply all role overwrites for the member's roles.
	var allowBits, denyBits uint64
	for _, roleId := range memberRoles {
		if ow, ok := owMap[roleId]; ok {
			denyBits |= ow.Deny
			allowBits |= ow.Allow
		}
	}
	channelPerms &^= denyBits
	channelPerms |= allowBits

	// Step 6: Apply member overwrite.
	if ow, ok := owMap[userId]; ok {
		channelPerms &^= ow.Deny
		channelPerms |= ow.Allow
	}

	// Step 7: Administrator via overwrites also grants everything.
	if permission.HasPermissionRaw(channelPerms, permission.Administrator) {
		return ^uint64(0)
	}

	return channelPerms
}

// MissingPermissions returns the subset of required permissions that the member lacks
// in the given channel.
func MissingPermissions(
	guildId, userId uint64,
	memberRoles []uint64,
	overwrites []channel.PermissionOverwrite,
	roleMap map[uint64]guild.Role,
	required []permission.Permission,
) []permission.Permission {
	effective := EffectivePermissions(guildId, userId, memberRoles, overwrites, roleMap)

	var missing []permission.Permission
	for _, p := range required {
		if !permission.HasPermissionRaw(effective, p) {
			missing = append(missing, p)
		}
	}
	return missing
}
