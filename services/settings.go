package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Known settings keys. Secrets are encrypted at rest; see CONTEXT.md > Secret
// Storage.
const (
	SettingPushoverAppToken = "pushover.app_token" // encrypted
	SettingPushoverUserKey  = "pushover.user_key"  // encrypted
	SettingCampfireBaseURL  = "campfire.base_url"  // plain
	SettingCampfireBotKey   = "campfire.bot_key"   // encrypted
)

type SettingsService struct {
	metaDB *sql.DB
	enc    *EncryptionService
}

func NewSettingsService(metaDB *sql.DB, enc *EncryptionService) *SettingsService {
	return &SettingsService{metaDB: metaDB, enc: enc}
}

// Get returns the decrypted value, or "" when the key is unset.
func (s *SettingsService) Get(ctx context.Context, key string) (string, error) {
	var value string
	var encrypted bool
	err := s.metaDB.QueryRowContext(ctx,
		"SELECT value, is_encrypted FROM settings WHERE key = ?", key).Scan(&value, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if encrypted {
		plain, err := s.enc.Decrypt(value)
		if err != nil {
			return "", fmt.Errorf("setting %s: %w (was POOML_ENCRYPTION_KEY rotated? re-enter the secret in Settings)", key, err)
		}
		return plain, nil
	}
	return value, nil
}

func (s *SettingsService) Set(ctx context.Context, key, value string, encrypted bool) error {
	stored := value
	if encrypted {
		var err error
		if stored, err = s.enc.Encrypt(value); err != nil {
			return err
		}
	}
	_, err := s.metaDB.ExecContext(ctx,
		`INSERT INTO settings (key, value, is_encrypted, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, is_encrypted = excluded.is_encrypted, updated_at = excluded.updated_at`,
		key, stored, encrypted, time.Now().UnixMilli())
	return err
}

// IsSet reports presence without decrypting - for "configured" UI states that
// must not surface secret values.
func (s *SettingsService) IsSet(ctx context.Context, key string) bool {
	var one int
	err := s.metaDB.QueryRowContext(ctx, "SELECT 1 FROM settings WHERE key = ? AND value != ''", key).Scan(&one)
	return err == nil
}
