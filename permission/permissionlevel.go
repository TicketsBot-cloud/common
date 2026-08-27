package permission

type PermissionLevel int

const (
	Everyone PermissionLevel = iota
	Support
	Admin
)

func (l PermissionLevel) Int() int {
	return int(l)
}

type PermissionSource string

const (
	SourceNone          PermissionSource = ""
	SourceBotAdmin      PermissionSource = "bot_admin"
	SourceBotStaff      PermissionSource = "bot_staff"
	SourceAdministrator PermissionSource = "administrator"
	SourceGuildOwner    PermissionSource = "guild_owner"
	SourceAddsupport    PermissionSource = "addsupport"
	SourceAdminRole     PermissionSource = "admin_role"
	SourceStaffTeam     PermissionSource = "staff_team"
	SourceSupportRole   PermissionSource = "support_role"
	SourceTeamRole      PermissionSource = "team_role"
)
