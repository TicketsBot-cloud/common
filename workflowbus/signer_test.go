package workflowbus

import (
	"encoding/json"
	"testing"
	"time"
)

func mkEnvelope() Envelope {
	return Envelope{
		Version:     EnvelopeVersion,
		TriggerType: TriggerTicketCreated,
		GuildId:     777,
		CausationId: "abc",
		OccurredAt:  time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC),
		Payload:     json.RawMessage(`{"ticket_id": 123}`),
	}
}

func TestSigner_Inactive_AcceptsAnything(t *testing.T) {
	s := NewSigner(nil)
	if s.Active() {
		t.Fatal("empty-secret signer should be inactive")
	}
	env := mkEnvelope()
	if err := s.Sign(&env); err != nil {
		t.Fatalf("Sign on inactive signer should be no-op: %v", err)
	}
	if env.Signature != "" {
		t.Fatal("inactive Sign should leave Signature empty")
	}
	if err := s.Verify(&env, true); err != nil {
		t.Fatalf("inactive Verify should accept unsigned: %v", err)
	}
}

func TestSigner_SignAndVerify(t *testing.T) {
	s := NewSigner([]byte("shared-secret"))
	env := mkEnvelope()
	if err := s.Sign(&env); err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	if env.Signature == "" {
		t.Fatal("active Sign should populate Signature")
	}
	if err := s.Verify(&env, true); err != nil {
		t.Fatalf("freshly-signed envelope failed verify: %v", err)
	}
}

func TestSigner_RejectsTamperedPayload(t *testing.T) {
	s := NewSigner([]byte("shared-secret"))
	env := mkEnvelope()
	_ = s.Sign(&env)
	env.Payload = json.RawMessage(`{"ticket_id": 999}`)
	if err := s.Verify(&env, true); err == nil {
		t.Fatal("expected tampered payload to fail verification")
	}
}

func TestSigner_RejectsMissingSignatureInStrictMode(t *testing.T) {
	s := NewSigner([]byte("shared-secret"))
	env := mkEnvelope() // no Sign called
	if err := s.Verify(&env, true); err == nil {
		t.Fatal("strict mode should reject unsigned envelope")
	}
	if err := s.Verify(&env, false); err != nil {
		t.Fatalf("non-strict mode should accept unsigned: %v", err)
	}
}

func TestSigner_RejectsWrongSecret(t *testing.T) {
	produced := NewSigner([]byte("producer-secret"))
	consumed := NewSigner([]byte("consumer-secret"))
	env := mkEnvelope()
	_ = produced.Sign(&env)
	if err := consumed.Verify(&env, true); err == nil {
		t.Fatal("verify with mismatched secret should fail")
	}
}
