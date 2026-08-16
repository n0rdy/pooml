package services_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/services"
)

func TestBackupRunNow(t *testing.T) {
	var mu sync.Mutex
	uploads := map[string][]byte{}
	fakeS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			uploads[r.URL.Path] = body
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeS3.Close()

	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	enc, err := services.NewEncryptionService("0123456789abcdef0123456789abcdef", pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	settings := services.NewSettingsService(pools.Meta, enc)
	ctx := context.Background()

	mustExec(t, pools.LogsWrite, "INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (1,1,2,'svc','h','backed up line')")

	bs := services.NewBackupService(settings, pools.LogsRead, pools.Metrics, pools.Meta)

	// unconfigured: fails loudly and records the failure
	if err := bs.RunNow(ctx); err == nil {
		t.Fatal("unconfigured backup must error")
	}
	if v, _ := settings.Get(ctx, services.SettingBackupLastResult); v == "" || v == "ok" {
		t.Errorf("failure not recorded: %q", v)
	}

	for key, val := range map[string]string{
		services.SettingBackupEndpoint: fakeS3.URL,
		services.SettingBackupBucket:   "bkt",
		services.SettingBackupPrefix:   "pooml",
		services.SettingBackupRegion:   "auto",
	} {
		if err := settings.Set(ctx, key, val, false); err != nil {
			t.Fatal(err)
		}
	}
	for key, val := range map[string]string{
		services.SettingBackupAccessKey: "AKIATEST",
		services.SettingBackupSecretKey: "secret",
	} {
		if err := settings.Set(ctx, key, val, true); err != nil {
			t.Fatal(err)
		}
	}

	if err := bs.RunNow(ctx); err != nil {
		t.Fatalf("backup: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(uploads) != 3 {
		t.Fatalf("uploads = %d (%v), want 3", len(uploads), keysOf(uploads))
	}
	for path, body := range uploads {
		if !strings.HasPrefix(path, "/bkt/pooml/") {
			t.Errorf("path %q missing bucket/prefix", path)
		}
		if !bytes.HasPrefix(body, []byte("SQLite format 3\x00")) {
			t.Errorf("%s is not a SQLite file (%d bytes)", path, len(body))
		}
	}
	found := false
	for path, body := range uploads {
		if strings.HasSuffix(path, "/logs.db") {
			found = true
			if !bytes.Contains(body, []byte("backed up line")) {
				t.Error("logs.db backup missing seeded row")
			}
		}
	}
	if !found {
		t.Errorf("no logs.db upload: %v", keysOf(uploads))
	}
	if v, _ := settings.Get(ctx, services.SettingBackupLastResult); v != "ok" {
		t.Errorf("last result = %q, want ok", v)
	}
}

func TestValidateCron(t *testing.T) {
	if err := services.ValidateCron("0 3 * * *"); err != nil {
		t.Errorf("valid cron rejected: %v", err)
	}
	if err := services.ValidateCron("not a cron"); err == nil {
		t.Error("garbage cron accepted")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
