package encryption

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type tokenVector struct {
	Name       string `json:"name"`
	KeyVersion int16  `json:"key_version"`
	KeyB64     string `json:"key_b64"`
	NonceHex   string `json:"nonce_hex"`
	BotId      uint64 `json:"bot_id"`
	Plaintext  string `json:"plaintext"`
	SealedHex  string `json:"sealed_hex"`
}

func loadVectors(t *testing.T) []tokenVector {
	t.Helper()

	raw, err := os.ReadFile("testdata/token_vectors.json")
	require.NoError(t, err)

	var vectors []tokenVector
	require.NoError(t, json.Unmarshal(raw, &vectors))
	require.NotEmpty(t, vectors)

	return vectors
}

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func testRing(t *testing.T, active string) *Keyring {
	t.Helper()

	ring, err := ParseKeyring("1:"+b64(testKey(0x11))+",2:"+b64(testKey(0x22)), active)
	require.NoError(t, err)

	return ring
}

func TestVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			key, err := base64.StdEncoding.DecodeString(v.KeyB64)
			require.NoError(t, err)

			nonce, err := hex.DecodeString(v.NonceHex)
			require.NoError(t, err)

			want, err := hex.DecodeString(v.SealedHex)
			require.NoError(t, err)

			got, err := sealWithNonce(key, nonce, []byte(v.Plaintext), BotIdAAD(v.BotId))
			require.NoError(t, err)
			require.Equal(t, want, got)

			opened, err := DecryptWithAAD(key, want, BotIdAAD(v.BotId))
			require.NoError(t, err)
			require.Equal(t, v.Plaintext, string(opened))

			ring, err := ParseKeyring(fmt.Sprintf("%d:%s", v.KeyVersion, v.KeyB64), fmt.Sprint(v.KeyVersion))
			require.NoError(t, err)

			token, err := ring.Open(v.BotId, v.KeyVersion, want)
			require.NoError(t, err)
			require.Equal(t, v.Plaintext, token)
		})
	}
}

func TestBotIdAAD(t *testing.T) {
	tests := []struct {
		name  string
		botId uint64
		want  []byte
	}{
		{"zero", 0, []byte{0, 0, 0, 0, 0, 0, 0, 0}},
		{"one", 1, []byte{0, 0, 0, 0, 0, 0, 0, 1}},
		{"big endian", 0x0102030405060708, []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{"max", ^uint64(0), bytes.Repeat([]byte{0xff}, 8)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BotIdAAD(tt.botId))
		})
	}
}

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		botId uint64
		token string
	}{
		{"typical", 123456789, "fake-token"},
		{"bot id zero", 0, "fake-token"},
		{"bot id max", ^uint64(0), "fake-token"},
		{"long", 7, strings.Repeat("x", 4096)},
		{"single byte", 7, "x"},
		{"non ascii", 7, "fake-töken"},
	}

	ring := testRing(t, "2")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sealed, version, err := ring.Seal(tt.botId, tt.token)
			require.NoError(t, err)
			require.Equal(t, int16(2), version)
			require.GreaterOrEqual(t, len(sealed), MinSealedLen)

			got, err := ring.Open(tt.botId, version, sealed)
			require.NoError(t, err)
			require.Equal(t, tt.token, got)
		})
	}

	t.Run("nonces differ between seals", func(t *testing.T) {
		a, _, err := ring.Seal(1, "fake-token")
		require.NoError(t, err)
		b, _, err := ring.Seal(1, "fake-token")
		require.NoError(t, err)
		require.NotEqual(t, a, b)
	})

	t.Run("empty token refused", func(t *testing.T) {
		_, _, err := ring.Seal(1, "")
		require.ErrorIs(t, err, ErrEmptyToken)
	})
}

func TestWrongBotId(t *testing.T) {
	ring := testRing(t, "1")

	sealed, version, err := ring.Seal(100, "fake-token")
	require.NoError(t, err)

	_, err = ring.Open(101, version, sealed)
	require.ErrorIs(t, err, ErrDecrypt)

	_, err = ring.Open(0, version, sealed)
	require.ErrorIs(t, err, ErrDecrypt)
}

func TestTamper(t *testing.T) {
	ring := testRing(t, "1")

	sealed, version, err := ring.Seal(5, "fake-token-for-tamper-test")
	require.NoError(t, err)

	tests := []struct {
		name  string
		index int
	}{
		{"first nonce byte", 0},
		{"last nonce byte", nonceLen - 1},
		{"first body byte", nonceLen},
		{"last body byte", len(sealed) - tagLen - 1},
		{"first tag byte", len(sealed) - tagLen},
		{"last tag byte", len(sealed) - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := bytes.Clone(sealed)
			mutated[tt.index] ^= 0x01

			_, err := ring.Open(5, version, mutated)
			require.ErrorIs(t, err, ErrDecrypt)
		})
	}

	t.Run("truncated by one byte", func(t *testing.T) {
		_, err := ring.Open(5, version, sealed[:len(sealed)-1])
		require.ErrorIs(t, err, ErrDecrypt)
	})
}

func TestSealedLengths(t *testing.T) {
	ring := testRing(t, "1")
	key := testKey(0x11)

	tests := []struct {
		name     string
		length   int
		tooShort bool
	}{
		{"nil", 0, true},
		{"11", 11, true},
		{"12", 12, true},
		{"27", 27, true},
		{"28 garbage", 28, false},
		{"29 garbage", 29, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := make([]byte, tt.length)

			require.NotPanics(t, func() {
				_, err := DecryptWithAAD(key, input, nil)
				require.ErrorIs(t, err, ErrDecrypt)
				require.Equal(t, tt.tooShort, errorsIs(err, ErrSealedTooShort))

				_, err = ring.Open(1, 1, input)
				require.ErrorIs(t, err, ErrDecrypt)
			})
		})
	}

	t.Run("nil sealed slice", func(t *testing.T) {
		_, err := DecryptWithAAD(key, nil, nil)
		require.ErrorIs(t, err, ErrSealedTooShort)
	})

	t.Run("28 bytes valid empty plaintext", func(t *testing.T) {
		sealed, err := EncryptWithAAD(key, nil, BotIdAAD(1))
		require.NoError(t, err)
		require.Len(t, sealed, MinSealedLen)

		plaintext, err := DecryptWithAAD(key, sealed, BotIdAAD(1))
		require.NoError(t, err)
		require.Empty(t, plaintext)
	})
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, e := range u.Unwrap() {
				if errorsIs(e, target) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

func TestKeyLengths(t *testing.T) {
	tests := []struct {
		name   string
		keyLen int
	}{
		{"empty", 0},
		{"16", 16},
		{"31", 31},
		{"33", 33},
		{"64", 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := make([]byte, tt.keyLen)

			_, err := EncryptWithAAD(key, []byte("x"), nil)
			require.ErrorIs(t, err, ErrInvalidKey)

			_, err = DecryptWithAAD(key, make([]byte, 40), nil)
			require.ErrorIs(t, err, ErrInvalidKey)
		})
	}
}

func TestParseKeyringFailures(t *testing.T) {
	good := b64(testKey(0x33))
	secret := b64(bytes.Repeat([]byte{0x44}, 31))

	tests := []struct {
		name   string
		keys   string
		active string
	}{
		{"empty keys", "", "1"},
		{"whitespace keys", "   ", "1"},
		{"empty active", "1:" + good, ""},
		{"missing colon", "1" + good, "1"},
		{"empty entry", "1:" + good + ",", "1"},
		{"empty key part", "1:", "1"},
		{"bad base64", "1:not*base64*not*base64*not*base64*not*base64!!", "1"},
		{"base64 with newline", "1:" + good[:20] + "\n" + good[20:], "1"},
		{"url safe alphabet", "1:" + strings.NewReplacer("+", "-", "/", "_").Replace(b64(bytes.Repeat([]byte{0xfb}, 32))), "1"},
		{"non canonical trailing bits", "1:" + good[:len(good)-2] + "B=", "1"},
		{"wrong length 31", "1:" + secret, "1"},
		{"wrong length 16", "1:" + b64(testKey(1)[:16]), "1"},
		{"wrong length 33", "1:" + b64(bytes.Repeat([]byte{1}, 33)), "1"},
		{"duplicate version", "1:" + good + ",1:" + b64(testKey(0x55)), "1"},
		{"version zero", "0:" + good, "0"},
		{"version too large", "32768:" + good, "32768"},
		{"version negative", "-1:" + good, "1"},
		{"version leading zero", "01:" + good, "1"},
		{"version not numeric", "a:" + good, "1"},
		{"version overflow", "99999999999999999999:" + good, "1"},
		{"active absent", "1:" + good, "2"},
		{"active not numeric", "1:" + good, "latest"},
		{"active zero", "1:" + good, "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ring, err := ParseKeyring(tt.keys, tt.active)
			require.Nil(t, ring)
			require.ErrorIs(t, err, ErrKeyConfig)

			msg := err.Error()
			require.NotContains(t, msg, good)
			require.NotContains(t, msg, secret)
			if len(tt.keys) > 8 {
				require.NotContains(t, msg, tt.keys)
				require.NotContains(t, msg, tt.keys[2:])
			}
		})
	}
}

func TestParseKeyringSuccess(t *testing.T) {
	a, b := b64(testKey(1)), b64(testKey(2))

	tests := []struct {
		name   string
		keys   string
		active string
		want   int16
	}{
		{"single", "1:" + a, "1", 1},
		{"two keys first active", "1:" + a + ",2:" + b, "1", 1},
		{"two keys second active", "1:" + a + ",2:" + b, "2", 2},
		{"max version", "32767:" + a, "32767", 32767},
		{"spaces around entries", " 1:" + a + " , 2:" + b + " ", " 2 ", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ring, err := ParseKeyring(tt.keys, tt.active)
			require.NoError(t, err)
			require.Equal(t, tt.want, ring.ActiveVersion())
		})
	}
}

func TestRotation(t *testing.T) {
	oldRing := testRing(t, "1")
	sealedOld, oldVersion, err := oldRing.Seal(9, "fake-token")
	require.NoError(t, err)
	require.Equal(t, int16(1), oldVersion)

	rotated := testRing(t, "2")
	require.Equal(t, int16(2), rotated.ActiveVersion())

	got, err := rotated.Open(9, oldVersion, sealedOld)
	require.NoError(t, err)
	require.Equal(t, "fake-token", got)

	sealedNew, newVersion, err := rotated.Seal(9, "fake-token")
	require.NoError(t, err)
	require.Equal(t, int16(2), newVersion)

	got, err = rotated.Open(9, newVersion, sealedNew)
	require.NoError(t, err)
	require.Equal(t, "fake-token", got)

	t.Run("new seal does not open under old version", func(t *testing.T) {
		_, err := rotated.Open(9, 1, sealedNew)
		require.ErrorIs(t, err, ErrDecrypt)
	})

	t.Run("ring holding both keys opens either version", func(t *testing.T) {
		_, err := oldRing.Open(9, 2, sealedNew)
		require.NoError(t, err)
	})
}

func TestUnknownVersion(t *testing.T) {
	ring := testRing(t, "1")
	sealed, _, err := ring.Seal(1, "fake-token")
	require.NoError(t, err)

	for _, version := range []int16{-1, 0, 3, 32767} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			_, err := ring.Open(1, version, sealed)
			require.ErrorIs(t, err, ErrUnknownKeyVersion)
		})
	}
}

func TestResolveStored(t *testing.T) {
	ring := testRing(t, "1")
	sealed, version, err := ring.Seal(77, "sealed-token")
	require.NoError(t, err)

	str := func(s string) *string { return &s }
	ver := func(v int16) *int16 { return &v }

	corrupt := bytes.Clone(sealed)
	corrupt[len(corrupt)-1] ^= 0x01

	tests := []struct {
		name    string
		legacy  *string
		sealed  []byte
		version *int16
		want    string
		wantErr error
	}{
		{"legacy wins over sealed", str("legacy-token"), sealed, &version, "legacy-token", nil},
		{"legacy wins over corrupt sealed", str("legacy-token"), corrupt, &version, "legacy-token", nil},
		{"legacy only", str("legacy-token"), nil, nil, "legacy-token", nil},
		{"sealed when legacy nil", nil, sealed, &version, "sealed-token", nil},
		{"sealed when legacy empty", str(""), sealed, &version, "sealed-token", nil},
		{"corrupt sealed is error", nil, corrupt, &version, "", ErrDecrypt},
		{"corrupt sealed with empty legacy is error", str(""), corrupt, &version, "", ErrDecrypt},
		{"unknown version is error", nil, sealed, ver(9), "", ErrUnknownKeyVersion},
		{"short sealed is error", nil, make([]byte, 5), &version, "", ErrDecrypt},
		{"sealed without version", nil, sealed, nil, "", ErrMissingToken},
		{"version without sealed", nil, nil, &version, "", ErrMissingToken},
		{"empty sealed slice", nil, []byte{}, &version, "", ErrMissingToken},
		{"nothing stored", nil, nil, nil, "", ErrMissingToken},
		{"empty legacy and nothing else", str(""), nil, nil, "", ErrMissingToken},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ring.ResolveStored(77, tt.legacy, tt.sealed, tt.version)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Empty(t, got)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("wrong bot id is error", func(t *testing.T) {
		_, err := ring.ResolveStored(78, nil, sealed, &version)
		require.ErrorIs(t, err, ErrDecrypt)
	})
}

func TestKeyringRedaction(t *testing.T) {
	key := testKey(0x66)
	ring, err := ParseKeyring("1:"+b64(key), "1")
	require.NoError(t, err)

	forms := []string{
		ring.String(),
		fmt.Sprintf("%v", ring),
		fmt.Sprintf("%+v", ring),
		fmt.Sprintf("%#v", ring),
		fmt.Sprintf("%s", ring),
		fmt.Sprintf("%x", ring),
		fmt.Sprintf("%v", *ring),
		fmt.Sprintf("%+v", struct{ R *Keyring }{ring}),
	}

	for _, form := range forms {
		require.NotContains(t, form, b64(key))
		require.NotContains(t, form, hex.EncodeToString(key))
		require.NotContains(t, form, "102 102") // decimal bytes of 0x66
	}
}
