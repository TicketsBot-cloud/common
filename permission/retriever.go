package permission

import (
	"context"

	"github.com/TicketsBot-cloud/database"
	"github.com/TicketsBot-cloud/gdl/objects/member"
	"github.com/TicketsBot-cloud/gdl/permission"
)

type Retriever interface {
	Db() *database.Database
	Cache() PermissionCache
	IsBotAdmin(ctx context.Context, userId uint64) bool
	GetGuildOwner(ctx context.Context, guildId uint64) (uint64, error)
}

func GetPermissionLevel(ctx context.Context, retriever Retriever, member member.Member, guildId uint64) (permLevel PermissionLevel, returnedError error) {
	// Check user ID in cache
	if cached, err := retriever.Cache().GetCachedPermissionLevel(ctx, guildId, member.User.Id); err == nil {
		return cached, nil
	} else if err != ErrNotCached {
		return Everyone, err
	}

	// Check if the user is a bot admin user
	if retriever.IsBotAdmin(ctx, member.User.Id) {
		return Admin, nil
	}

	// Don't recache if already cached (for now?)
	defer func() {
		if returnedError == nil {
			returnedError = retriever.Cache().SetCachedPermissionLevel(ctx, guildId, member.User.Id, permLevel)
		}
	}()

	// Check if user has Administrator permission
	if member.Permissions > 0 && permission.HasPermissionRaw(member.Permissions, permission.Administrator) {
		return Admin, nil
	}

	// Check if user is guild owner
	if guildOwner, err := retriever.GetGuildOwner(ctx, guildId); err == nil {
		if member.User.Id == guildOwner {
			return Admin, nil
		}
	} else {
		return Everyone, err
	}

	// Check user perms for admin
	if adminUser, err := retriever.Db().Permissions.IsAdmin(ctx, guildId, member.User.Id); err == nil {
		if adminUser {
			return Admin, nil
		}
	} else {
		return Everyone, err
	}

	// Check roles from DB
	adminRoles, err := retriever.Db().RolePermissions.GetAdminRoles(ctx, guildId)
	if err != nil {
		return Everyone, err
	}

	for _, adminRoleId := range adminRoles {
		if member.HasRole(adminRoleId) {
			return Admin, nil
		}
	}

	// Check user perms for support
	if isSupport, err := retriever.Db().Permissions.IsSupport(ctx, guildId, member.User.Id); err == nil {
		if isSupport {
			return Support, nil
		}
	} else {
		return Everyone, err
	}

	// Check if user is a member of a support team
	if isSupport, err := retriever.Db().SupportTeamMembers.IsSupport(ctx, guildId, member.User.Id); err == nil {
		if isSupport {
			return Support, nil
		}
	} else {
		return Everyone, err
	}

	// Check DB for support roles
	supportRoles, err := retriever.Db().RolePermissions.GetSupportRoles(ctx, guildId)
	if err != nil {
		return Everyone, err
	}

	for _, supportRoleId := range supportRoles {
		if member.HasRole(supportRoleId) {
			return Support, nil
		}
	}

	// Check if user has a role assigned to a support team
	if isSupport, err := retriever.Db().SupportTeamRoles.IsSupportAny(ctx, guildId, member.Roles); err == nil {
		if isSupport {
			return Support, nil
		}
	} else {
		return Everyone, err
	}

	return Everyone, nil
}

// GetPermissionLevelWithSource mirrors the logic of GetPermissionLevel but also
// returns a PermissionSource indicating which check granted the permission. This
// function bypasses the cache because the cache only stores the level, not the source.
func GetPermissionLevelWithSource(ctx context.Context, retriever Retriever, member member.Member, guildId uint64) (PermissionLevel, PermissionSource, error) {
	// Check if the user is a bot admin user
	if retriever.IsBotAdmin(ctx, member.User.Id) {
		return Admin, SourceBotAdmin, nil
	}

	// Check if user has Administrator permission
	if member.Permissions > 0 && permission.HasPermissionRaw(member.Permissions, permission.Administrator) {
		return Admin, SourceAdministrator, nil
	}

	// Check if user is guild owner
	if guildOwner, err := retriever.GetGuildOwner(ctx, guildId); err == nil {
		if member.User.Id == guildOwner {
			return Admin, SourceGuildOwner, nil
		}
	} else {
		return Everyone, SourceNone, err
	}

	// Check user perms for admin
	if adminUser, err := retriever.Db().Permissions.IsAdmin(ctx, guildId, member.User.Id); err == nil {
		if adminUser {
			return Admin, SourceAddsupport, nil
		}
	} else {
		return Everyone, SourceNone, err
	}

	// Check roles from DB
	adminRoles, err := retriever.Db().RolePermissions.GetAdminRoles(ctx, guildId)
	if err != nil {
		return Everyone, SourceNone, err
	}

	for _, adminRoleId := range adminRoles {
		if member.HasRole(adminRoleId) {
			return Admin, SourceAdminRole, nil
		}
	}

	// Check user perms for support
	if isSupport, err := retriever.Db().Permissions.IsSupport(ctx, guildId, member.User.Id); err == nil {
		if isSupport {
			return Support, SourceAddsupport, nil
		}
	} else {
		return Everyone, SourceNone, err
	}

	// Check if user is a member of a support team
	if isSupport, err := retriever.Db().SupportTeamMembers.IsSupport(ctx, guildId, member.User.Id); err == nil {
		if isSupport {
			return Support, SourceStaffTeam, nil
		}
	} else {
		return Everyone, SourceNone, err
	}

	// Check DB for support roles
	supportRoles, err := retriever.Db().RolePermissions.GetSupportRoles(ctx, guildId)
	if err != nil {
		return Everyone, SourceNone, err
	}

	for _, supportRoleId := range supportRoles {
		if member.HasRole(supportRoleId) {
			return Support, SourceSupportRole, nil
		}
	}

	// Check if user has a role assigned to a support team
	if isSupport, err := retriever.Db().SupportTeamRoles.IsSupportAny(ctx, guildId, member.Roles); err == nil {
		if isSupport {
			return Support, SourceTeamRole, nil
		}
	} else {
		return Everyone, SourceNone, err
	}

	return Everyone, SourceNone, nil
}
