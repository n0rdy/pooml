package services

import (
	"context"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/db"
)

func newApiKeysService(t *testing.T) *ApiKeysService {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	return NewApiKeysService(pools.Meta)
}

func TestApiKeyCreateVerifyRoundTrip(t *testing.T) {
	s := newApiKeysService(t)
	ctx := context.Background()

	key, err := s.Create(ctx, "fluentbit-prod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "pooml_") {
		t.Errorf("key = %q, want pooml_ prefix", key)
	}

	if !s.Verify(ctx, key) {
		t.Error("freshly created key does not verify")
	}
	// second verify goes through the cache
	if !s.Verify(ctx, key) {
		t.Error("cached verify failed")
	}

	// stored hash is PHC-format argon2id, never the plaintext
	var stored string
	if err := s.metaDB.QueryRow("SELECT key_hash FROM api_keys").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "$argon2id$v=") {
		t.Errorf("stored hash = %q, want PHC argon2id", stored)
	}
	if strings.Contains(key, stored) || strings.Contains(stored, strings.SplitN(key, "_", 3)[2]) {
		t.Error("plaintext secret leaked into storage")
	}
}

func TestApiKeyVerifyRejects(t *testing.T) {
	s := newApiKeysService(t)
	ctx := context.Background()

	key, err := s.Create(ctx, "real")
	if err != nil {
		t.Fatal(err)
	}

	bad := []string{
		"",
		"garbage",
		"pooml_1",                              // missing secret
		"nope_1_" + strings.Repeat("a", 43),    // wrong prefix
		"pooml_999_" + strings.Repeat("a", 43), // nonexistent id
		"pooml_x_" + strings.Repeat("a", 43),   // non-numeric id
		key[:len(key)-1] + "X",                 // right id, corrupted secret
	}
	for _, k := range bad {
		if s.Verify(ctx, k) {
			t.Errorf("Verify(%q) = true, want false", k)
		}
	}
	if !s.Verify(ctx, key) {
		t.Error("real key stopped verifying")
	}
}

func TestApiKeyRevoke(t *testing.T) {
	s := newApiKeysService(t)
	ctx := context.Background()

	key1, _ := s.Create(ctx, "one")
	key2, _ := s.Create(ctx, "two")

	// warm the cache for both, then revoke one
	if !s.Verify(ctx, key1) || !s.Verify(ctx, key2) {
		t.Fatal("setup verify failed")
	}
	keys, err := s.List(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("list = %v, %v", keys, err)
	}

	var id1 int64
	for _, k := range keys {
		if k.Label == "one" {
			id1 = k.ID
		}
	}
	if err := s.Revoke(ctx, id1); err != nil {
		t.Fatal(err)
	}

	// revocation must also evict the cache, or revoked keys keep working
	if s.Verify(ctx, key1) {
		t.Error("revoked key still verifies (cache not purged?)")
	}
	if !s.Verify(ctx, key2) {
		t.Error("unrelated key was affected by revoke")
	}
	if keys, _ := s.List(ctx); len(keys) != 1 || keys[0].Label != "two" {
		t.Errorf("list after revoke = %+v", keys)
	}
}

func TestApiKeyLabelValidation(t *testing.T) {
	s := newApiKeysService(t)
	ctx := context.Background()

	for _, label := range []string{"", "   ", strings.Repeat("x", 101)} {
		if _, err := s.Create(ctx, label); err == nil {
			t.Errorf("Create(%q) succeeded, want error", label)
		}
	}
	if _, err := s.Create(ctx, "  padded  "); err != nil {
		t.Errorf("trimmed label rejected: %v", err)
	}
}

func TestVerifySecretPHCCompat(t *testing.T) {
	// params come from the stored PHC string, not from current constants:
	// a hash written with different params must still verify
	phc, err := hashSecret("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if !verifySecret("s3cret", phc) {
		t.Error("round trip failed")
	}
	if verifySecret("wrong", phc) {
		t.Error("wrong secret verified")
	}
	if verifySecret("s3cret", "$argon2id$v=19$m=bad$x$y") {
		t.Error("malformed PHC verified")
	}
}
