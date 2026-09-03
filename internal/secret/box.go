// Package secret provides small, purpose-built helpers for protecting relay
// credentials at rest and for deriving non-reversible client token digests.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// Box encrypts values with AES-256-GCM. The key is retained only in memory.
type Box struct {
	aead cipher.AEAD
	key  [32]byte
}

// New constructs a box from a standard-base64 encoded 32-byte key.
func New(encoded string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, errors.New("RELAY_ENCRYPTION_KEY must decode to 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	var raw [32]byte
	copy(raw[:], key)
	return &Box{aead: aead, key: raw}, nil
}

// Seal returns a standard-base64 value containing nonce followed by ciphertext.
func (b *Box) Seal(plain string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("secret box is not initialized")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate encryption nonce: %w", err)
	}
	ciphertext := b.aead.Seal(nil, nonce, []byte(plain), nil)
	output := make([]byte, 0, len(nonce)+len(ciphertext))
	output = append(output, nonce...)
	output = append(output, ciphertext...)
	return base64.StdEncoding.EncodeToString(output), nil
}

// Open decrypts a value produced by Seal and rejects malformed or tampered data.
func (b *Box) Open(encoded string) (string, error) {
	if b == nil || b.aead == nil {
		return "", errors.New("secret box is not initialized")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	if len(data) < b.aead.NonceSize()+b.aead.Overhead() {
		return "", errors.New("ciphertext is too short")
	}
	nonce, ciphertext := data[:b.aead.NonceSize()], data[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("ciphertext authentication failed")
	}
	return string(plain), nil
}

// Digest returns a stable keyed HMAC-SHA-256 digest for a client token.
// It is suitable for persistence and constant-time lookup, but cannot recover
// the original token.
func (b *Box) Digest(token string) string {
	return hex.EncodeToString(b.DigestBytes(token))
}

// DigestBytes is the raw keyed HMAC-SHA-256 digest for a client token.
func (b *Box) DigestBytes(token string) []byte {
	if b == nil {
		return nil
	}
	mac := hmac.New(sha256.New, b.key[:])
	_, _ = mac.Write([]byte(token))
	return mac.Sum(nil)
}
