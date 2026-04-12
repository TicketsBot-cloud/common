package botpermissions

import "github.com/TicketsBot-cloud/gdl/permission"

// StandardPermissions are the permissions granted to users inside a ticket channel.
var StandardPermissions = []permission.Permission{
	permission.AddReactions,
	permission.ViewChannel,
	permission.SendMessages,
	permission.SendTTSMessages,
	permission.EmbedLinks,
	permission.AttachFiles,
	permission.MentionEveryone,
	permission.UseExternalEmojis,
	permission.ReadMessageHistory,
	permission.UseApplicationCommands,
	permission.UseExternalStickers,
	permission.SendVoiceMessages,
}

// MinimalPermissions are the base permissions the bot needs for any channel interaction.
var MinimalPermissions = []permission.Permission{
	permission.ViewChannel,
	permission.SendMessages,
	permission.ReadMessageHistory,
	permission.UseApplicationCommands,
}

// ThreadModeRequired are the permissions the bot needs on the panel channel in thread mode.
var ThreadModeRequired = []permission.Permission{
	permission.ViewChannel,
	permission.ReadMessageHistory,
	permission.EmbedLinks,
	permission.AttachFiles,
	permission.UseExternalEmojis,
	permission.CreatePrivateThreads,
	permission.SendMessagesInThreads,
	permission.ManageThreads,
}

// ChannelModeRequired are the permissions the bot needs on the ticket category in channel mode.
// It is StandardPermissions with ManageChannels prepended.
var ChannelModeRequired = func() []permission.Permission {
	perms := make([]permission.Permission, 0, 1+len(StandardPermissions))
	perms = append(perms, permission.ManageChannels)
	perms = append(perms, StandardPermissions...)
	return perms
}()

// NotifChannelRequired are the permissions the bot needs on the notification channel (thread mode only).
// It is MinimalPermissions with EmbedLinks and AttachFiles appended.
var NotifChannelRequired = func() []permission.Permission {
	perms := make([]permission.Permission, 0, len(MinimalPermissions)+2)
	perms = append(perms, MinimalPermissions...)
	perms = append(perms, permission.EmbedLinks, permission.AttachFiles)
	return perms
}()

// TranscriptChannelRequired are the permissions the bot needs on the transcript channel (any mode).
// Equivalent to MinimalPermissions.
var TranscriptChannelRequired = MinimalPermissions
