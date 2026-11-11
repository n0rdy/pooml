package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog/log"
)

// Backup settings keys. Credentials are encrypted at rest; see CONTEXT.md >
// Backups for the full design (Backup API, lifecycle-managed retention).
const (
	SettingBackupEnabled   = "backup.enabled"     // "true"/"false"
	SettingBackupSchedule  = "backup.schedule"    // cron expression
	SettingBackupEndpoint  = "backup.s3_endpoint" // empty = AWS
	SettingBackupRegion    = "backup.s3_region"
	SettingBackupBucket    = "backup.s3_bucket"
	SettingBackupPrefix    = "backup.s3_prefix"
	SettingBackupAccessKey = "backup.s3_access_key" // encrypted
	SettingBackupSecretKey = "backup.s3_secret_key" // encrypted

	// last-run state, written by the runner for the Settings UI
	SettingBackupLastRunAt  = "backup.last_run_at" // ms epoch
	SettingBackupLastResult = "backup.last_result" // "ok" or the error text

	BackupDefaultSchedule = "0 3 * * *" // daily 3am UTC
)

// standard 5-field cron
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func ValidateCron(expr string) error {
	_, err := cronParser.Parse(expr)
	return err
}

type BackupService struct {
	settings *SettingsService
	dbDir    string
	dbs      map[string]*sql.DB // filename (logs.db etc.) -> open handle
	runMu    sync.Mutex         // one backup at a time (cron + "run now" can overlap)
}

// NewBackupService sources logs.db from the READ pool: the Backup API only
// reads pages, and the write pool has a single connection - holding it for a
// multi-minute copy would stall all ingestion. Temp copies live under dbDir,
// not the OS temp dir (often tmpfs, and too small for a multi-GB copy);
// leftovers from a SIGKILL are removed here on the next boot.
func NewBackupService(settings *SettingsService, dbDir string, logsRead, metricsDB, metaDB *sql.DB) *BackupService {
	if stale, err := filepath.Glob(filepath.Join(dbDir, ".backup-*")); err == nil {
		for _, dir := range stale {
			if err := os.RemoveAll(dir); err != nil {
				log.Error().Err(err).Str("dir", dir).Msg("removing stale backup temp dir")
			}
		}
	}
	return &BackupService{
		settings: settings,
		dbDir:    dbDir,
		dbs:      map[string]*sql.DB{"logs.db": logsRead, "metrics.db": metricsDB, "meta.db": metaDB},
	}
}

// Run checks once a minute whether the configured cron schedule fires in
// that minute. Re-reading settings each tick means schedule and credential
// changes apply without a restart, and there is no scheduler state to keep
// in sync with the DB.
func (bs *BackupService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			if !bs.enabled(ctx) {
				continue
			}
			expr, err := bs.settings.Get(ctx, SettingBackupSchedule)
			if err != nil || expr == "" {
				expr = BackupDefaultSchedule
			}
			sched, err := cronParser.Parse(expr)
			if err != nil {
				log.Error().Err(err).Str("cron", expr).Msg("invalid backup schedule")
				continue
			}
			minute := now.UTC().Truncate(time.Minute)
			if sched.Next(minute.Add(-time.Second)) == minute {
				bs.RunNow(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (bs *BackupService) enabled(ctx context.Context) bool {
	v, err := bs.settings.Get(ctx, SettingBackupEnabled)
	if err != nil {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// RunNow performs one full backup: online-copy each DB to a temp file, then
// upload all three under one timestamped prefix. Records the outcome for the
// Settings UI either way.
func (bs *BackupService) RunNow(ctx context.Context) error {
	if !bs.runMu.TryLock() {
		return errors.New("a backup is already running")
	}
	defer bs.runMu.Unlock()
	err := bs.runBackup(ctx)
	result := "ok"
	if err != nil {
		result = err.Error()
		BackupRuns.WithLabelValues("error").Inc()
		log.Error().Err(err).Msg("backup failed")
	} else {
		BackupRuns.WithLabelValues("ok").Inc()
		log.Info().Msg("backup completed")
	}
	stateCtx := context.WithoutCancel(ctx) // record the outcome even when aborted
	if serr := bs.settings.Set(stateCtx, SettingBackupLastRunAt, strconv.FormatInt(time.Now().UnixMilli(), 10), false); serr != nil {
		log.Error().Err(serr).Msg("record backup run time")
	}
	if serr := bs.settings.Set(stateCtx, SettingBackupLastResult, result, false); serr != nil {
		log.Error().Err(serr).Msg("record backup result")
	}
	return err
}

func (bs *BackupService) runBackup(ctx context.Context) error {
	cfg, err := bs.s3Config(ctx)
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp(bs.dbDir, ".backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	stamp := time.Now().UTC().Format("2006-01-02T150405Z")
	for name, handle := range bs.dbs {
		if err := backupDB(ctx, handle, filepath.Join(tmpDir, name)); err != nil {
			return fmt.Errorf("backup %s: %w", name, err)
		}
	}

	client := s3.NewFromConfig(aws.Config{
		Region:      cfg.region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.accessKey, cfg.secretKey, ""),
		// default CRC32 checksums break Cloudflare R2 uploads
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}, func(o *s3.Options) {
		if cfg.endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.endpoint)
			o.UsePathStyle = true // MinIO/R2/B2 custom endpoints expect path-style
		}
	})

	for name := range bs.dbs {
		key := stamp + "/" + name
		if cfg.prefix != "" {
			key = cfg.prefix + "/" + key
		}
		if err := uploadFile(ctx, client, cfg.bucket, key, filepath.Join(tmpDir, name)); err != nil {
			return fmt.Errorf("upload %s: %w", key, err)
		}
	}
	return nil
}

// S3-compatible stores refuse a single PUT over 5 GB, which a logs.db with a
// month of retention can exceed, so anything above one part goes multipart.
// Hand-rolled on the client already in use: sequential parts streamed from the
// file, no upload-manager dependency. A var only so tests can shrink it.
var backupPartSize int64 = 64 << 20

const backupMaxParts = 10000 // S3's limit per upload

func uploadFile(ctx context.Context, client *s3.Client, bucket, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size <= backupPartSize {
		_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: f})
		return err
	}

	partSize := max(backupPartSize, (size+backupMaxParts-1)/backupMaxParts)
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		return err
	}
	abort := func() {
		// needs s3:AbortMultipartUpload, which the minimal s3:PutObject policy
		// lacks; refused, only a bucket lifecycle rule clears the orphaned parts
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, aerr := client.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		}); aerr != nil {
			log.Warn().Err(aerr).Str("key", key).
				Msg("could not abort the multipart upload; its parts stay until a lifecycle rule expires incomplete uploads")
		}
	}

	var parts []types.CompletedPart
	for num, off := int32(1), int64(0); off < size; num, off = num+1, off+partSize {
		n := min(partSize, size-off)
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
			PartNumber: aws.Int32(num), Body: io.NewSectionReader(f, off, n), ContentLength: aws.Int64(n),
		})
		if err != nil {
			abort()
			return fmt.Errorf("part %d: %w", num, err)
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		abort()
		return err
	}
	return nil
}

type s3Settings struct {
	endpoint, region, bucket, prefix, accessKey, secretKey string
}

func (bs *BackupService) s3Config(ctx context.Context) (s3Settings, error) {
	get := func(key string) string {
		v, err := bs.settings.Get(ctx, key)
		if err != nil {
			log.Error().Err(err).Str("key", key).Msg("read backup setting")
		}
		return v
	}
	cfg := s3Settings{
		endpoint:  get(SettingBackupEndpoint),
		region:    get(SettingBackupRegion),
		bucket:    get(SettingBackupBucket),
		prefix:    get(SettingBackupPrefix),
		accessKey: get(SettingBackupAccessKey),
		secretKey: get(SettingBackupSecretKey),
	}
	if cfg.bucket == "" || cfg.accessKey == "" || cfg.secretKey == "" {
		return cfg, errors.New("backup is not fully configured: bucket and credentials are required")
	}
	if cfg.region == "" {
		cfg.region = "auto"
	}
	return cfg, nil
}

const (
	backupBusyRetries = 50
	backupBusyWait    = 100 * time.Millisecond
)

// backupDB copies src into destPath with SQLite's online Backup API in ONE
// step. Stepping in chunks restarts the copy whenever another connection
// writes to the source, so under steady ingest it never finished. A single
// step reads one WAL snapshot; writers are not blocked.
func backupDB(ctx context.Context, src *sql.DB, destPath string) error {
	destDB, err := sql.Open("sqlite3", destPath)
	if err != nil {
		return err
	}
	defer destDB.Close()

	srcConn, err := src.Conn(ctx)
	if err != nil {
		return err
	}
	defer srcConn.Close()
	destConn, err := destDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer destConn.Close()

	return destConn.Raw(func(destDriver any) error {
		return srcConn.Raw(func(srcDriver any) error {
			srcC, ok := srcDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("source is not a sqlite3 connection")
			}
			destC := destDriver.(*sqlite3.SQLiteConn)
			bk, err := destC.Backup("main", srcC, "main")
			if err != nil {
				return err
			}
			defer bk.Finish()
			// the driver reports BUSY/LOCKED as (false, nil); the next step
			// resumes the copy, so a brief lock must not fail the whole run
			for attempt := 0; ; attempt++ {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				done, err := bk.Step(-1)
				if err != nil {
					return err
				}
				if done {
					return nil
				}
				if attempt >= backupBusyRetries {
					return errors.New("source database stayed locked; backup not completed")
				}
				time.Sleep(backupBusyWait)
			}
		})
	})
}
