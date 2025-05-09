package restcache

import "github.com/TicketsBot-cloud/gdl/objects/guild"

type RestCache interface {
	GetGuildRoles(guildId uint64) ([]guild.Role, error)
}
