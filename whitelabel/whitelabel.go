package whitelabel

import (
	"context"
	"errors"

	"github.com/TicketsBot-cloud/database"
	"github.com/TicketsBot-cloud/gdl/objects/application"
	"github.com/TicketsBot-cloud/gdl/rest"
	"github.com/TicketsBot-cloud/gdl/rest/request"
)

// Discord only accepts the three "limited" intent flags on PATCH /applications/@me. The
// non-limited counterparts are granted by intents review, and asking to turn a limited bit
// on for an application that is already approved (or is over the exposure threshold) is
// rejected with APPLICATION_MAX_INTENTS_EXPOSURE_REACHED.
const writableFlags = application.FlagIntentGatewayPresenceLimited |
	application.FlagIntentGatewayGuildMembersLimited |
	application.FlagGatewayMessageContentLimited

const invalidFormBodyCode = 50035

const intentsExposureErrorCode = "APPLICATION_MAX_INTENTS_EXPOSURE_REACHED"

const IntentsRejectedMessage = "Discord refused to enable the Server Members and Message " +
	"Content intents for your bot: the application is exposed to too many users and must be " +
	"reviewed for privileged intents first. Apply for them in the Discord Developer Portal, " +
	"then try again."

// DesiredIntentFlags returns the flags value to send for an application whose flags are
// currently current, or nil if current already grants the intents the bot needs and the
// field should be omitted from the request.
func DesiredIntentFlags(current application.Flag) *application.Flag {
	desired := current & writableFlags

	if !current.Has(application.FlagIntentGatewayGuildMembers) {
		desired |= application.FlagIntentGatewayGuildMembersLimited
	}

	if !current.Has(application.FlagGatewayMessageContent) {
		desired |= application.FlagGatewayMessageContentLimited
	}

	if desired == current&writableFlags {
		return nil
	}

	return &desired
}

// IsIntentsRejection reports whether err is Discord refusing to enable a privileged intent
// because the application is exposed to too many users and has not been reviewed.
func IsIntentsRejection(err error) bool {
	var restError request.RestError
	if !errors.As(err, &restError) || restError.ApiError.Code != invalidFormBodyCode {
		return false
	}

	for _, fieldError := range restError.ApiError.Errors {
		if code, ok := fieldError.Code.(string); ok && code == intentsExposureErrorCode {
			return true
		}
	}

	return false
}

// ReapplyIntents reapplies the gateway intents to the whitelabel application, without
// touching the interactions endpoint URL. Used when resyncing a bot that is already set up.
func ReapplyIntents(ctx context.Context, token string) error {
	app, err := rest.GetCurrentApplication(ctx, token, nil)
	if err != nil {
		return err
	}

	var currentFlags application.Flag
	if app.Flags != nil {
		currentFlags = *app.Flags
	}

	flags := DesiredIntentFlags(currentFlags)
	if flags == nil {
		return nil
	}

	_, err = rest.EditCurrentApplication(ctx, token, nil, rest.EditCurrentApplicationData{
		Flags: flags,
	})
	return err
}

// SyncGuilds reconciles the whitelabel_guilds table for botId against the guilds the bot is
// actually a member of, fetched from Discord using its token. Guilds present on Discord but
// missing from the DB are added; guilds in the DB the bot is no longer in are removed.
// Deletion only happens after the full guild list has been enumerated successfully, so a
// partial fetch never purges valid rows.
func SyncGuilds(ctx context.Context, db *database.Database, token string, botId uint64) error {
	discord := make(map[uint64]struct{})

	var after uint64
	for {
		guilds, err := rest.GetCurrentUserGuilds(ctx, token, nil, rest.CurrentUserGuildsData{
			After: after,
			Limit: 200,
		})
		if err != nil {
			return err
		}

		for _, g := range guilds {
			discord[g.Id] = struct{}{}
			after = g.Id
		}

		if len(guilds) < 200 {
			break
		}
	}

	stored, err := db.WhitelabelGuilds.GetGuilds(ctx, botId)
	if err != nil {
		return err
	}

	storedSet := make(map[uint64]struct{}, len(stored))
	for _, id := range stored {
		storedSet[id] = struct{}{}
	}

	// Add guilds the bot is in that we don't have stored
	for id := range discord {
		if _, ok := storedSet[id]; !ok {
			if err := db.WhitelabelGuilds.Add(ctx, botId, id); err != nil {
				return err
			}
		}
	}

	// Remove stored guilds the bot is no longer in
	for _, id := range stored {
		if _, ok := discord[id]; !ok {
			if err := db.WhitelabelGuilds.Delete(ctx, botId, id); err != nil {
				return err
			}
		}
	}

	return nil
}
