package workflowbus

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// SecretEnvVar is the environment variable every service in this system reads
// to find the shared HMAC secret for workflowbus envelopes. Using the same name
// in every service prevents the fiddly asymmetric configuration failure mode
// where producer and consumer have different secrets and every envelope fails
// verification with no obvious cause.
const SecretEnvVar = "WORKFLOWBUS_HMAC_SECRET"

// ErrInvalidSignature is returned by Verify when the envelope's signature
// doesn't match the re-computed HMAC. Consumers typically drop the envelope
// and log when this happens — never execute.
var ErrInvalidSignature = errors.New("workflowbus: envelope signature invalid")

// ErrMissingSignature is returned when a signer is configured but the envelope
// arrived unsigned. Consumers can choose to reject strictly (recommended once
// the rollout has completed) or log and accept (during rollout).
var ErrMissingSignature = errors.New("workflowbus: envelope missing signature")

// Signer signs and verifies envelopes with HMAC-SHA256. Nil-safe: a Signer
// created with an empty secret acts as a no-op — Sign leaves Signature empty
// and Verify accepts any envelope. Services read the secret from SecretEnvVar
// and construct a Signer once at startup.
type Signer struct {
	secret []byte
}

// NewSigner returns a Signer for the given secret. If secret is empty the
// returned Signer is a pass-through: Sign does nothing, Verify accepts all.
// This is the safe default during gradual rollout — add the secret to one side
// at a time, then flip the strict flag once both sides have it.
func NewSigner(secret []byte) *Signer {
	if len(secret) == 0 {
		return &Signer{}
	}
	// Defensive copy — callers may reuse the backing slice.
	s := make([]byte, len(secret))
	copy(s, secret)
	return &Signer{secret: s}
}

// Active reports whether this Signer will produce / require signatures.
func (s *Signer) Active() bool {
	return s != nil && len(s.secret) > 0
}

// Sign computes the envelope's HMAC and writes it to env.Signature.
// No-op when the Signer is inactive.
func (s *Signer) Sign(env *Envelope) error {
	if !s.Active() {
		return nil
	}
	mac, err := s.compute(env)
	if err != nil {
		return err
	}
	env.Signature = base64.StdEncoding.EncodeToString(mac)
	return nil
}

// Verify checks the envelope's signature against a freshly-computed HMAC.
// Returns nil when:
//   - the Signer is inactive (no secret configured; accept all), OR
//   - the envelope's signature matches the computed HMAC.
//
// When strictOnMissing is true, envelopes with an empty Signature are rejected
// with ErrMissingSignature — use this after the rollout is complete to lock
// out unsigned traffic entirely.
func (s *Signer) Verify(env *Envelope, strictOnMissing bool) error {
	if !s.Active() {
		return nil
	}
	if env.Signature == "" {
		if strictOnMissing {
			return ErrMissingSignature
		}
		return nil
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature)
	if err != nil {
		return ErrInvalidSignature
	}
	mac, err := s.compute(env)
	if err != nil {
		return err
	}
	if !hmac.Equal(sig, mac) {
		return ErrInvalidSignature
	}
	return nil
}

// compute serialises the envelope with Signature empty and HMACs the result.
// The empty-Signature canonicalisation means the signed blob and the
// to-be-signed blob agree byte-for-byte — without this, Sign on an already-
// signed envelope would be ambiguous.
func (s *Signer) compute(env *Envelope) ([]byte, error) {
	clone := *env
	clone.Signature = ""
	raw, err := json.Marshal(clone)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(raw)
	return mac.Sum(nil), nil
}
