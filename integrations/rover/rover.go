package rover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pkg/errors"

	"github.com/TicketsBot-cloud/common/webproxy"
)

type RoverResponse struct {
	RobloxId int `json:"robloxId,string"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

var (
	ErrQuotaExceeded = errors.New("rover api quota exceeded")
	ErrUserNotFound  = errors.New("user not found")
)

func RequestUserId(ctx context.Context, proxy *webproxy.WebProxy, roverApiKey string, guildId uint64, userId uint64) (int, error) {
	url := fmt.Sprintf("https://registry.rover.link/api/guilds/%d/discord-to-roblox/%d", guildId, userId)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", roverApiKey))

	res, err := proxy.Do(req)
	if err != nil {
		return 0, err
	}

	switch res.StatusCode {
	case http.StatusOK:
		break // continue
	case http.StatusNotFound:
		return 0, ErrUserNotFound
	case http.StatusTooManyRequests:
		return 0, ErrQuotaExceeded
	default:
		var errorResponse ErrorResponse

		if err := json.NewDecoder(res.Body).Decode(&errorResponse); err != nil {
			return 0, errors.Wrapf(err, "failed to decode rover error response - status code was %d", res.StatusCode)
		}

		return 0, errors.Wrap(errors.New(errorResponse.Error), "rover api returned error")
	}

	var response RoverResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return 0, err
	}

	return response.RobloxId, nil
}
