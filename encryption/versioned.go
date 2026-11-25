package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// MinSealedLen is the smallest valid sealed value: a 12 byte nonce plus a
	// 16 byte GCM tag around an empty plaintext.
	MinSealedLen = nonceLen + tagLen

	keyLen   = 32
	nonceLen = 12
	tagLen   = 16

	minKeyVersion = 1
	maxKeyVersion = 32767
)

var (
	// ErrDecrypt is returned when sealed data cannot be authenticated or is
	// structurally invalid. Callers must treat it as fatal for that value.
	ErrDecrypt = errors.New("encryption: decryption failed")
	// ErrSealedTooShort is wrapped alongside ErrDecrypt for undersized input.
	ErrSealedTooShort = errors.New("encryption: sealed value too short")
	// ErrInvalidKey is returned when a key is not exactly 32 bytes.
	ErrInvalidKey = errors.New("encryption: key must be 32 bytes")
	// ErrKeyConfig is returned for any malformed key configuration.
	ErrKeyConfig = errors.New("encryption: invalid key configuration")
	// ErrUnknownKeyVersion is returned when a version is not in the keyring.
	ErrUnknownKeyVersion = errors.New("encryption: unknown key version")
	// ErrMissingToken is returned when a stored row holds no usable token.
	ErrMissingToken = errors.New("encryption: no usable token stored")
	// ErrEmptyToken is returned when asked to seal an empty token.
	ErrEmptyToken = errors.New("encryption: refusing to seal an empty token")
)

// BotIdAAD binds a ciphertext to a bot so a sealed value cannot be moved
// between rows.
func BotIdAAD(botId uint64) []byte {
	aad := make([]byte, 8)
	binary.BigEndian.PutUint64(aad, botId)
	return aad
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, ErrInvalidKey
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating gcm: %w", err)
	}

	return gcm, nil
}

// sealWithNonce is split out so tests can reproduce fixed vectors.
func sealWithNonce(key, nonce, plaintext, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(nonce) != nonceLen {
		return nil, fmt.Errorf("encryption: nonce must be %d bytes", nonceLen)
	}

	out := make([]byte, 0, nonceLen+len(plaintext)+tagLen)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, aad), nil
}

// EncryptWithAAD returns nonce || ciphertext || tag using AES-256-GCM.
func EncryptWithAAD(key, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	return sealWithNonce(key, nonce, plaintext, aad)
}

// DecryptWithAAD reverses EncryptWithAAD. It never panics on short input.
func DecryptWithAAD(key, sealed, aad []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	if len(sealed) < MinSealedLen {
		return nil, fmt.Errorf("%w: %w", ErrDecrypt, ErrSealedTooShort)
	}

	plaintext, err := gcm.Open(nil, sealed[:nonceLen], sealed[nonceLen:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}

	return plaintext, nil
}

// Keyring holds versioned encryption keys and the version used for new seals.
type Keyring struct {
	keys   map[int16][]byte
	active int16
}

// ParseKeyring parses "1:<base64>,2:<base64>" and selects the active version.
// Errors never include the input, so key material cannot reach logs.
func ParseKeyring(keys, activeVersion string) (*Keyring, error) {
	if strings.TrimSpace(keys) == "" {
		return nil, fmt.Errorf("%w: no keys provided", ErrKeyConfig)
	}

	active, ok := parseVersion(strings.TrimSpace(activeVersion))
	if !ok {
		return nil, fmt.Errorf("%w: active version is not a valid version number", ErrKeyConfig)
	}

	ring := &Keyring{keys: make(map[int16][]byte), active: active}

	for i, entry := range strings.Split(keys, ",") {
		versionStr, keyStr, found := strings.Cut(strings.TrimSpace(entry), ":")
		if !found {
			return nil, fmt.Errorf("%w: entry %d is malformed", ErrKeyConfig, i+1)
		}

		version, ok := parseVersion(versionStr)
		if !ok {
			return nil, fmt.Errorf("%w: entry %d has an invalid version", ErrKeyConfig, i+1)
		}

		key, err := base64.StdEncoding.DecodeString(keyStr)
		if err != nil || base64.StdEncoding.EncodeToString(key) != keyStr {
			return nil, fmt.Errorf("%w: entry %d is not canonical base64", ErrKeyConfig, i+1)
		}

		if len(key) != keyLen {
			return nil, fmt.Errorf("%w: entry %d is not %d bytes", ErrKeyConfig, i+1, keyLen)
		}

		if _, dup := ring.keys[version]; dup {
			return nil, fmt.Errorf("%w: entry %d repeats an earlier version", ErrKeyConfig, i+1)
		}

		ring.keys[version] = key
	}

	if _, ok := ring.keys[active]; !ok {
		return nil, fmt.Errorf("%w: active version has no key", ErrKeyConfig)
	}

	return ring, nil
}

// parseVersion accepts canonical decimal integers in [1, 32767] only.
func parseVersion(s string) (int16, bool) {
	if len(s) == 0 || len(s) > 5 || s[0] == '0' {
		return 0, false
	}

	n := 0
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}

	if n < minKeyVersion || n > maxKeyVersion {
		return 0, false
	}

	return int16(n), true
}

func (k *Keyring) ActiveVersion() int16 {
	return k.active
}

// Seal encrypts a token under the active key and returns the version used.
func (k *Keyring) Seal(botId uint64, token string) ([]byte, int16, error) {
	if token == "" {
		return nil, 0, ErrEmptyToken
	}

	sealed, err := EncryptWithAAD(k.keys[k.active], []byte(token), BotIdAAD(botId))
	if err != nil {
		return nil, 0, fmt.Errorf("sealing token: %w", err)
	}

	return sealed, k.active, nil
}

// Open decrypts a sealed token with the key for the stated version.
func (k *Keyring) Open(botId uint64, version int16, sealed []byte) (string, error) {
	key, ok := k.keys[version]
	if !ok {
		return "", ErrUnknownKeyVersion
	}

	plaintext, err := DecryptWithAAD(key, sealed, BotIdAAD(botId))
	if err != nil {
		return "", fmt.Errorf("opening token: %w", err)
	}

	return string(plaintext), nil
}

// ResolveStored picks the token for a row during the migration window. A
// non-empty legacy value wins because a pod that predates encryption may have
// updated only the plaintext column, leaving the sealed copy stale. A failed
// decryption is never papered over by falling back to anything else.
func (k *Keyring) ResolveStored(botId uint64, legacy *string, sealed []byte, version *int16) (string, error) {
	if legacy != nil && *legacy != "" {
		return *legacy, nil
	}

	if len(sealed) == 0 || version == nil {
		return "", ErrMissingToken
	}

	token, err := k.Open(botId, *version, sealed)
	if err != nil {
		return "", err
	}

	if token == "" {
		return "", ErrMissingToken
	}

	return token, nil
}

const redacted = "Keyring{redacted}"

func (Keyring) String() string   { return redacted }
func (Keyring) GoString() string { return redacted }

// Value receivers are deliberate: formatting a Keyring copy (not just a
// pointer) must be redacted too. Format covers every verb, so no formatting
// path can reach the key map.
func (Keyring) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redacted)
}
