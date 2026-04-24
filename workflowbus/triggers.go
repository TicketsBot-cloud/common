package workflowbus

import (
	"encoding/json"
	"time"
)

const (
	TopicWorkflowTriggers = "tickets.rpc.workflows"

	TriggerTicketCreated     = "ticket.created"
	TriggerTicketClaimed     = "ticket.claimed"
	TriggerTicketClosed      = "ticket.closed"
	TriggerTicketReopened    = "ticket.reopened"
	TriggerTicketTransferred = "ticket.transferred"
	TriggerCron              = "cron"
	TriggerWebhook           = "webhook"

	EnvelopeVersion = 1
)

// Envelope is the wire format for every message on TopicWorkflowTriggers.
// Payload is a trigger-type-specific JSON document.
//
// Signature is a base64 HMAC-SHA256 of the canonical envelope bytes (i.e. the
// envelope with Signature field empty, marshalled deterministically). It's
// populated by the Signer and verified by the executor when a shared secret is
// configured. When the secret is unset on both sides, Signature stays empty and
// is ignored — preserves backward compatibility during rollout.
type Envelope struct {
	Version     int             `json:"version"`
	TriggerType string          `json:"trigger_type"`
	GuildId     uint64          `json:"guild_id,string"`
	CausationId string          `json:"causation_id"`
	WorkflowId  int64           `json:"workflow_id,string,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Payload     json.RawMessage `json:"payload"`
	Signature   string          `json:"signature,omitempty"`
}

type TicketCreatedPayload struct {
	TicketId  int               `json:"ticket_id"`
	OpenerId  uint64            `json:"opener_id,string"`
	PanelId   *int              `json:"panel_id,omitempty"`
	ChannelId *uint64           `json:"channel_id,string,omitempty"`
	IsThread  bool              `json:"is_thread"`
	Form      map[string]string `json:"form,omitempty"`
}

type TicketClaimedPayload struct {
	TicketId  int    `json:"ticket_id"`
	ClaimedBy uint64 `json:"claimed_by,string"`
}

type TicketClosedPayload struct {
	TicketId int     `json:"ticket_id"`
	ClosedBy uint64  `json:"closed_by,string"`
	Reason   *string `json:"reason,omitempty"`
}

type TicketReopenedPayload struct {
	TicketId   int    `json:"ticket_id"`
	ReopenedBy uint64 `json:"reopened_by,string"`
}

type TicketTransferredPayload struct {
	TicketId      int    `json:"ticket_id"`
	FromUserId    uint64 `json:"from_user_id,string"`
	ToUserId      uint64 `json:"to_user_id,string"`
	TransferredBy uint64 `json:"transferred_by,string"`
}

type CronPayload struct {
	AutomationId int64 `json:"automation_id,string"`
}

type WebhookPayload struct {
	AutomationId int64             `json:"automation_id,string"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         json.RawMessage   `json:"body,omitempty"`
}
