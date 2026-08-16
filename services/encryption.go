package services

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/argon2"
)

// EncryptionService is AES-256-GCM keyed from POOML_ENCRYPTION_KEY via
// argon2id with a per-install salt (stored plaintext in meta.db, so it
// travels with backups). A bare hash would let anyone holding a meta.db
// backup run an offline dictionary attack on a passphrase-style key.
// Used for secrets stored in meta.db - see CONTEXT.md > Secret Storage.
type EncryptionService struct {
	aead cipher.AEAD
}

func NewEncryptionService(secret string, metaDB *sql.DB) (*EncryptionService, error) {
	salt, err := ensureEncryptionSalt(metaDB)
	if err != nil {
		return nil, fmt.Errorf("encryption salt: %w", err)
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemoryKiB, argonThreads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher init: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm init: %w", err)
	}
	return &EncryptionService{aead: aead}, nil
}

func ensureEncryptionSalt(metaDB *sql.DB) ([]byte, error) {
	fresh := make([]byte, 16)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	// insert-if-absent then read back: first boot wins, every later boot
	// (and any concurrent starter) reads the same stored salt
	if _, err := metaDB.Exec(
		`INSERT INTO settings (key, value, is_encrypted, updated_at) VALUES ('encryption.salt', ?, 0, ?)
		 ON CONFLICT(key) DO NOTHING`,
		base64.StdEncoding.EncodeToString(fresh), time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	var stored string
	if err := metaDB.QueryRow("SELECT value FROM settings WHERE key = 'encryption.salt'").Scan(&stored); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(stored)
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
