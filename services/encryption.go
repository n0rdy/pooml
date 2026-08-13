package services

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// EncryptionService is AES-256-GCM keyed from POOML_ENCRYPTION_KEY (sha256 of
// the configured secret; the env var is already length-enforced at startup).
// Used for secrets stored in meta.db - see CONTEXT.md > Secret Storage.
type EncryptionService struct {
	aead cipher.AEAD
}

func NewEncryptionService(secret string) (*EncryptionService, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("cipher init: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm init: %w", err)
	}
	return &EncryptionService{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext).
func (e *EncryptionService) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	sealed := e.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (e *EncryptionService) Decrypt(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if len(raw) < e.aead.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:e.aead.NonceSize()], raw[e.aead.NonceSize():]
	plain, err := e.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		// wrong key or tampered data; the caller surfaces "re-enter the
		// secret" guidance (see CONTEXT.md > Backups > restore caveat)
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plain), nil
}
