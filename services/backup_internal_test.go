package services

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n0rdy/pooml/db"
)

// The chunked copy restarted on every write to the source, so with steady
// ingest a backup never completed. It must finish while writes keep landing.
func TestBackupCompletesUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	const insert = "INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (1,1,2,'svc','h',?)"
	filler := strings.Repeat("backup filler ", 1000)
	for range 1500 { // ~20 MB: far more pages than one chunk of the old loop
		if _, err := pools.LogsWrite.Exec(insert, filler); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				pools.LogsWrite.Exec(insert, "live line")
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "logs-copy.db")
	err = backupDB(ctx, pools.LogsRead, dest)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("backup under concurrent writes: %v", err)
	}

	copyDB, err := sql.Open("sqlite3", "file:"+dest+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var n int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil || n < 1500 {
		t.Fatalf("copy holds %d rows (err %v), want at least the 1500 seeded", n, err)
	}
	var check string
	if err := copyDB.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q (err %v)", check, err)
	}
}

// fakeS3 speaks just enough of the S3 API for single and multipart uploads.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte         // completed objects by path
	parts     map[string]map[int][]byte // upload id -> part number -> bytes
	puts      int                       // single-PUT objects
	aborted   []string
	failPart  int // fail this part number (0 = never)
	nextID    int
	completed []string
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.nextID++
		id := fmt.Sprintf("upload-%d", s.nextID)
		s.parts[id] = map[int][]byte{}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
	case r.Method == http.MethodPut && uploadID != "":
		num, _ := strconv.Atoi(q.Get("partNumber"))
		if num == s.failPart {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>no</Message></Error>`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		s.parts[uploadID][num] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, num))
	case r.Method == http.MethodPost && uploadID != "":
		var whole []byte
		for num := 1; num <= len(s.parts[uploadID]); num++ {
			whole = append(whole, s.parts[uploadID][num]...)
		}
		s.objects[r.URL.Path] = whole
		s.completed = append(s.completed, uploadID)
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><CompleteMultipartUploadResult><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && uploadID != "":
		s.aborted = append(s.aborted, uploadID)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		s.objects[r.URL.Path] = body
		s.puts++
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func multipartFixture(t *testing.T, store *fakeS3) (*BackupService, *SettingsService, *db.Pools) {
	t.Helper()
	srv := httptest.NewServer(store)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)
	enc, err := NewEncryptionService("0123456789abcdef0123456789abcdef", pools.Meta)
	if err != nil {
		t.Fatal(err)
	}
	settings := NewSettingsService(pools.Meta, enc)
	ctx := context.Background()
	for key, val := range map[string]string{SettingBackupEndpoint: srv.URL, SettingBackupBucket: "bkt"} {
		if err := settings.Set(ctx, key, val, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{SettingBackupAccessKey, SettingBackupSecretKey} {
		if err := settings.Set(ctx, key, "secret", true); err != nil {
			t.Fatal(err)
		}
	}
	// ~600 KB of logs: several parts once the part size is shrunk
	filler := strings.Repeat("multipart filler ", 600)
	for range 60 {
		if _, err := pools.LogsWrite.Exec(
			"INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (1,1,2,'svc','h',?)", filler); err != nil {
			t.Fatal(err)
		}
	}
	old := backupPartSize
	backupPartSize = 128 << 10
	t.Cleanup(func() { backupPartSize = old })
	return NewBackupService(settings, dir, pools.LogsRead, pools.Metrics, pools.Meta), settings, pools
}

// A single PUT stops at 5 GB on S3-compatible stores, so a large database goes
// up in parts. The reassembled object must be the database, byte for byte.
func TestBackupUploadsLargeFilesInParts(t *testing.T) {
	store := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
	bs, _, _ := multipartFixture(t, store)

	if err := bs.RunNow(context.Background()); err != nil {
		t.Fatalf("backup: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 3 || len(store.completed) == 0 || store.puts == 0 {
		t.Fatalf("objects=%d multipart=%d single=%d, want 3 objects with both kinds", len(store.objects), len(store.completed), store.puts)
	}
	var logsCopy []byte
	for path, body := range store.objects {
		if strings.HasSuffix(path, "/logs.db") {
			logsCopy = body
		}
	}
	nParts := len(store.parts[store.completed[0]])
	if nParts < 3 || int64(len(logsCopy)) <= backupPartSize {
		t.Fatalf("logs.db went up as %d part(s), %d bytes: the test did not exercise multipart", nParts, len(logsCopy))
	}

	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(restored, logsCopy, 0o600); err != nil {
		t.Fatal(err)
	}
	copyDB, err := sql.Open("sqlite3", "file:"+restored+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var n int
	var check string
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM logs").Scan(&n); err != nil || n != 60 {
		t.Fatalf("restored copy holds %d rows (err %v), want 60", n, err)
	}
	if err := copyDB.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity_check = %q (err %v)", check, err)
	}
}

// A failed part must fail the backup loudly and abort the upload, not leave a
// half-written object that looks like a backup.
func TestBackupMultipartFailureAborts(t *testing.T) {
	store := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}, failPart: 2}
	bs, settings, _ := multipartFixture(t, store)
	ctx := context.Background()

	if err := bs.RunNow(ctx); err == nil {
		t.Fatal("backup succeeded although a part was refused")
	}
	if v, _ := settings.Get(ctx, SettingBackupLastResult); v == "" || v == "ok" {
		t.Errorf("failure not recorded: %q", v)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.aborted) != 1 || len(store.completed) != 0 {
		t.Errorf("aborted=%v completed=%v, want one abort and no completion", store.aborted, store.completed)
	}
	for path := range store.objects {
		if strings.HasSuffix(path, "/logs.db") {
			t.Error("a logs.db object exists despite the failed upload")
		}
	}
}
