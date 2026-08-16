package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// Key format: pooml_{id}_{secret}. The embedded row id makes Verify a single
// argon2 run against one row instead of trying every stored hash.
const (
	apiKeyPrefix    = "pooml"
	apiKeySecretLen = 32

	// OWASP-recommended argon2id parameters (19 MiB, t=2, p=1)
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

type ApiKey struct {
	ID        int64
	Label     string
	CreatedAt int64
}

// ApiKeysService stores argon2id hashes in meta.db and keeps an in-memory
// cache of verified keys, so the ingest hot path pays the KDF cost once per
// key per process lifetime, not per request.
type ApiKeysService struct {
	metaDB *sql.DB

	mu       sync.RWMutex
	verified map[[sha256.Size]byte]int64 // sha256(presented key) -> key id
}

func NewApiKeysService(metaDB *sql.DB) *ApiKeysService {
	return &ApiKeysService{
		metaDB:   metaDB,
		verified: make(map[[sha256.Size]byte]int64),
	}
}

// Create mints a key and returns the plaintext exactly once; only the argon2id
// hash of the secret part is stored.
func (s *ApiKeysService) Create(ctx context.Context, label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 100 {
		return "", errors.New("label must be 1-100 characters")
	}

	raw := make([]byte, apiKeySecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)

	hash, err := hashSecret(secret)
	if err != nil {
		return "", err
	}
	res, err := s.metaDB.ExecContext(ctx,
		"INSERT INTO api_keys(label, key_hash, created_at) VALUES (?, ?, ?)",
		label, hash, time.Now().UnixMilli())
	if err != nil {
		return "", fmt.Errorf("store key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%d_%s", apiKeyPrefix, id, secret), nil
}

func (s *ApiKeysService) List(ctx context.Context) ([]ApiKey, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT id, label, created_at FROM api_keys ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []ApiKey
	for rows.Next() {
		var k ApiKey
		if err := rows.Scan(&k.ID, &k.Label, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *ApiKeysService) Revoke(ctx context.Context, id int64) error {
	if _, err := s.metaDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = ?", id); err != nil {
		return err
	}
	s.mu.Lock()
	for digest, keyID := range s.verified {
		if keyID == id {
			delete(s.verified, digest)
		}
	}
	s.mu.Unlock()
	return nil
}

// Verify reports whether key is a valid, non-revoked API key. Malformed keys
// are rejected before any DB or KDF work; wrong secrets pay full argon2 cost
// every time, which is fine - the auth-failure throttling locks abusers out.
func (s *ApiKeysService) Verify(ctx context.Context, key string) bool {
	digest := sha256.Sum256([]byte(key))
	s.mu.RLock()
	_, ok := s.verified[digest]
	s.mu.RUnlock()
	if ok {
		return true
	}

	// SplitN, not Split: the base64url alphabet includes '_', so the secret
	// itself may contain underscores
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 || parts[0] != apiKeyPrefix || parts[2] == "" {
		return false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}

	var phc string
	if err := s.metaDB.QueryRowContext(ctx,
		"SELECT key_hash FROM api_keys WHERE id = ?", id).Scan(&phc); err != nil {
		return false
	}
	if !verifySecret(parts[2], phc) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// re-check under the lock: a Revoke during the argon2 run above purges
	// the cache BEFORE this insert, and the revoked key would then stay
	// valid via the cache fast path until restart
	var one int
	if err := s.metaDB.QueryRowContext(ctx,
		"SELECT 1 FROM api_keys WHERE id = ?", id).Scan(&one); err != nil {
		return false
	}
	s.verified[digest] = id
	return true
}

// hashSecret produces a PHC-format string: parameters travel with the hash, so
// they can change later without invalidating stored keys.
func hashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(secret), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash)), nil
}

// each argon2 run costs 19 MiB + real CPU; without a cap, N parallel
// wrong-key requests from distinct IPs (throttling is per-IP) burn N of
// those at once - an easy memory spike on a small box
var argonSem = make(chan struct{}, 4)

func verifySecret(secret, phc string) bool {
	argonSem <- struct{}{}
	defer func() { <-argonSem }()
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
