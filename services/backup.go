package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	dbs      map[string]*sql.DB // filename (logs.db etc.) -> open handle
	runMu    sync.Mutex         // one backup at a time (cron + "run now" can overlap)
}

// NewBackupService sources logs.db from the READ pool: the Backup API only
// reads pages, and the write pool has a single connection - holding it for a
// multi-minute copy would stall all ingestion.
func NewBackupService(settings *SettingsService, logsRead, metricsDB, metaDB *sql.DB) *BackupService {
	return &BackupService{
		settings: settings,
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

	tmpDir, err := os.MkdirTemp("", "pooml-backup-*")
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
		f, err := os.Open(filepath.Join(tmpDir, name))
		if err != nil {
			return err
		}
		key := stamp + "/" + name
		if cfg.prefix != "" {
			key = cfg.prefix + "/" + key
		}
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(cfg.bucket),
			Key:    aws.String(key),
			Body:   f,
		})
		f.Close()
		if err != nil {
			return fmt.Errorf("upload %s: %w", key, err)
		}
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

// backupDB copies src into destPath with SQLite's online Backup API:
// incremental page copy, lock released between steps, so writes to the
// source continue with minimal interruption (touched pages are re-copied).
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
			for {
				done, err := bk.Step(256) // pages per step; lock yields between steps
				if err != nil {
					return err
				}
				if done {
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		})
	})
}
