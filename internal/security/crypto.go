package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EncryptionKeyEnv names the server environment variable holding the master
// key that seals user API keys. It is read from the environment only: it is
// never written to a file, a log, a message or the repository.
const EncryptionKeyEnv = "USER_PROVIDER_ENCRYPTION_KEY"

var (
	// ErrNoMasterKey means the operator never configured the master key.
	// User owned providers are refused rather than stored in plaintext.
	ErrNoMasterKey = errors.New("USER_PROVIDER_ENCRYPTION_KEY is not configured")
	// ErrInvalidSecret means a sealed value is malformed or was tampered
	// with. AES-GCM authenticates the ciphertext, so a modified blob fails
	// to open instead of yielding garbage.
	ErrInvalidSecret = errors.New("invalid or tampered secret")
)

// EncryptSecret seals plaintext with AES-256-GCM under masterKey and returns a
// self describing "v1:<base64 nonce+ciphertext>" string that is safe to store.
func EncryptSecret(plaintext, masterKey string) (string, error) {
	key, err := masterKeyBytes(masterKey)
	if err != nil {
		return "", err
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("reading nonce: %w", err)
	}

	// Seal appends the ciphertext to the nonce, so the blob carries its own
	// IV and needs no extra field.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)

	return "v1:" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// DecryptSecret opens a value produced by EncryptSecret. The plaintext lives
// only in the returned string: the caller must use it for the single request
// that needs it and then drop it.
func DecryptSecret(sealed, masterKey string) (string, error) {
	key, err := masterKeyBytes(masterKey)
	if err != nil {
		return "", err
	}

	version, blob, found := strings.Cut(strings.TrimSpace(sealed), ":")
	if !found || version != "v1" {
		return "", ErrInvalidSecret
	}

	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return "", ErrInvalidSecret
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}

	if len(raw) < gcm.NonceSize() {
		return "", ErrInvalidSecret
	}

	plaintext, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		// A wrong key and a tampered blob are the same failure here, and
		// neither is ever reported with any part of the secret in it.
		return "", ErrInvalidSecret
	}

	return string(plaintext), nil
}

// MasterKeyFromEnv reads the master key from the server environment. An empty
// value disables user owned providers instead of falling back to plaintext.
func MasterKeyFromEnv() string {
	return strings.TrimSpace(os.Getenv(EncryptionKeyEnv))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	return gcm, nil
}

// masterKeyBytes turns the operator supplied master key into a 32 byte
// AES-256 key. A 64 character hex string is decoded as is, anything else is
// stretched with SHA-256 so a passphrase works too.
func masterKeyBytes(masterKey string) ([]byte, error) {
	masterKey = strings.TrimSpace(masterKey)
	if masterKey == "" {
		return nil, ErrNoMasterKey
	}

	if len(masterKey) == 64 {
		if raw, err := hex.DecodeString(masterKey); err == nil {
			return raw, nil
		}
	}

	sum := sha256.Sum256([]byte(masterKey))

	return sum[:], nil
}
