package eventforwarding

import (
	"encoding/json"
	"testing"

	"github.com/TicketsBot-cloud/common/tokenchange"
	"github.com/stretchr/testify/require"
)

func TestPayloadsCarryNoToken(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{"event", Event{BotId: 1, IsWhitelabel: true, ShardId: 2, Event: json.RawMessage(`{}`)}},
		{"event zero", Event{}},
		{"interaction", Interaction{BotId: 1, IsWhitelabel: true, InteractionType: 2, Event: json.RawMessage(`{}`)}},
		{"interaction zero", Interaction{}},
		{"token change", tokenchange.TokenChangeData{NewId: 1, OldId: 2}},
		{"token change zero", tokenchange.TokenChangeData{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.value)
			require.NoError(t, err)

			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &fields))

			for key := range fields {
				require.NotContains(t, key, "token")
			}
		})
	}
}

func TestLegacyPayloadWithTokenStillDecodesAndDropsIt(t *testing.T) {
	// Producers that have not been updated still send bot_token. It must be
	// ignored rather than rejected, and must not survive re-marshalling.
	legacy := []byte(`{"bot_token":"secret","bot_id":5,"is_whitelabel":true,"shard_id":1,"event":{}}`)

	var event Event
	require.NoError(t, json.Unmarshal(legacy, &event))
	require.EqualValues(t, 5, event.BotId)

	raw, err := json.Marshal(event)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret")
}
