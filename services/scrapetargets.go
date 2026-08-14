package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Scrape interval bounds: see CONTEXT.md > Metrics. Sub-30s intervals are
// false precision for a single-instance tool whose deploys cause gaps at the
// same scale.
const (
	MinScrapeIntervalMs = 30_000
	MaxScrapeIntervalMs = 3_600_000
)

type ScrapeTarget struct {
	ID               int64
	Service          string
	Host             string
	URL              string
	AuthHeader       string // decrypted; empty when unset
	ScrapeIntervalMs int64
	Enabled          bool

	LastScrapedAt sql.NullInt64
	LastError     sql.NullString
	LastErrorAt   sql.NullInt64
	CreatedAt     int64
}

type ScrapeTargetsService struct {
	metaDB *sql.DB
	enc    *EncryptionService
}

func NewScrapeTargetsService(metaDB *sql.DB, enc *EncryptionService) *ScrapeTargetsService {
	return &ScrapeTargetsService{metaDB: metaDB, enc: enc}
}

func validateScrapeTarget(t *ScrapeTarget) error {
	t.Service = strings.TrimSpace(t.Service)
	t.Host = strings.TrimSpace(t.Host)
	t.URL = strings.TrimSpace(t.URL)
	t.AuthHeader = strings.TrimSpace(t.AuthHeader)

	if t.Service == "" || len(t.Service) > 200 {
		return errors.New("service must be 1-200 characters")
	}
	if t.Host == "" || len(t.Host) > 200 {
		return errors.New("host must be 1-200 characters")
	}
	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be a valid http(s) URL")
	}
	if t.AuthHeader != "" && !strings.Contains(t.AuthHeader, ":") {
		return errors.New(`auth header must look like "Header-Name: value"`)
	}
	if t.ScrapeIntervalMs < MinScrapeIntervalMs {
		return fmt.Errorf("scrape interval must be at least %d seconds", MinScrapeIntervalMs/1000)
	}
	if t.ScrapeIntervalMs > MaxScrapeIntervalMs {
		return errors.New("scrape interval cannot exceed 1 hour")
	}
	return nil
}

func (s *ScrapeTargetsService) Create(ctx context.Context, t ScrapeTarget) error {
	if err := validateScrapeTarget(&t); err != nil {
		return err
	}
	authStored, encrypted := "", false
	if t.AuthHeader != "" {
		var err error
		if authStored, err = s.enc.Encrypt(t.AuthHeader); err != nil {
			return err
		}
		encrypted = true
	}
	var auth any
	if authStored != "" {
		auth = authStored
	}
	_, err := s.metaDB.ExecContext(ctx,
		`INSERT INTO scrape_targets (service, host, url, auth_header, is_auth_encrypted, scrape_interval_ms, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Service, t.Host, t.URL, auth, encrypted, t.ScrapeIntervalMs, t.Enabled, time.Now().UnixMilli())
	return err
}

func (s *ScrapeTargetsService) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := s.metaDB.ExecContext(ctx, "UPDATE scrape_targets SET enabled = ? WHERE id = ?", enabled, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("scrape target not found")
	}
	return nil
}

func (s *ScrapeTargetsService) Delete(ctx context.Context, id int64) error {
	_, err := s.metaDB.ExecContext(ctx, "DELETE FROM scrape_targets WHERE id = ?", id)
	return err
}

const scrapeTargetColumns = `id, service, host, url, auth_header, is_auth_encrypted,
	scrape_interval_ms, enabled, last_scraped_at, last_error, last_error_at, created_at`

func (s *ScrapeTargetsService) scanTarget(row interface{ Scan(...any) error }) (ScrapeTarget, error) {
	var t ScrapeTarget
	var auth sql.NullString
	var encrypted bool
	err := row.Scan(&t.ID, &t.Service, &t.Host, &t.URL, &auth, &encrypted,
		&t.ScrapeIntervalMs, &t.Enabled, &t.LastScrapedAt, &t.LastError, &t.LastErrorAt, &t.CreatedAt)
	if err != nil {
		return t, err
	}
	if auth.Valid && auth.String != "" {
		if encrypted {
			if t.AuthHeader, err = s.enc.Decrypt(auth.String); err != nil {
				return t, fmt.Errorf("decrypt auth header for target %d: %w", t.ID, err)
			}
		} else {
			t.AuthHeader = auth.String
		}
	}
	return t, nil
}

func (s *ScrapeTargetsService) list(ctx context.Context, where string) ([]ScrapeTarget, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT "+scrapeTargetColumns+" FROM scrape_targets "+where+" ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ScrapeTarget
	for rows.Next() {
		t, err := s.scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *ScrapeTargetsService) List(ctx context.Context) ([]ScrapeTarget, error) {
	return s.list(ctx, "")
}

func (s *ScrapeTargetsService) ListEnabled(ctx context.Context) ([]ScrapeTarget, error) {
	return s.list(ctx, "WHERE enabled = 1")
}

// RecordScrape stores one scrape outcome; errMsg nil clears the error state.
func (s *ScrapeTargetsService) RecordScrape(ctx context.Context, id int64, scrapedAt int64, errMsg *string) error {
	var errAt *int64
	if errMsg != nil {
		errAt = &scrapedAt
	}
	_, err := s.metaDB.ExecContext(ctx,
		"UPDATE scrape_targets SET last_scraped_at = ?, last_error = ?, last_error_at = ? WHERE id = ?",
		scrapedAt, errMsg, errAt, id)
	return err
}

// EnsureSelfScrape registers pooml's own /metrics endpoint as a scrape target
// once (matched by URL). The endpoint itself lands in M10; until then the
// caller gates this on POOML_METRICS_ENABLED.
func (s *ScrapeTargetsService) EnsureSelfScrape(ctx context.Context, apiAddr string) error {
	selfURL := fmt.Sprintf("http://%s/metrics", apiAddr)
	var n int
	if err := s.metaDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM scrape_targets WHERE url = ?", selfURL).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return s.Create(ctx, ScrapeTarget{
		Service:          "pooml",
		Host:             "self",
		URL:              selfURL,
		ScrapeIntervalMs: MinScrapeIntervalMs,
		Enabled:          true,
	})
}
