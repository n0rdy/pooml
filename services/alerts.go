package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/n0rdy/pooml/query"
)

const MinCheckIntervalMs = 30_000 // protects the read pool; UI-validated and re-checked here

type Alert struct {
	ID              int64
	Name            string
	Query           string
	CheckIntervalMs int64
	CooldownMs      int64
	Target          string // raw JSON, validated via ParseTarget
	Enabled         bool

	LastCheckedAt   sql.NullInt64
	LastFiredAt     sql.NullInt64
	CurrentlyFiring bool
	LastError       sql.NullString
	LastErrorAt     sql.NullInt64
	CreatedAt       int64
}

type AlertsService struct {
	metaDB *sql.DB
}

func NewAlertsService(metaDB *sql.DB) *AlertsService {
	return &AlertsService{metaDB: metaDB}
}

func validateAlert(a *Alert) error {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" || len(a.Name) > 100 {
		return errors.New("name must be 1-100 characters")
	}
	if _, err := query.Validate(a.Query); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if a.CheckIntervalMs < MinCheckIntervalMs {
		return fmt.Errorf("check interval must be at least %d seconds", MinCheckIntervalMs/1000)
	}
	if a.CooldownMs < 0 {
		return errors.New("cooldown cannot be negative")
	}
	if _, err := ParseTarget(a.Target); err != nil {
		return err
	}
	return nil
}

func (s *AlertsService) Create(ctx context.Context, a Alert) error {
	if err := validateAlert(&a); err != nil {
		return err
	}
	_, err := s.metaDB.ExecContext(ctx,
		`INSERT INTO alerts (name, query, check_interval_ms, cooldown_ms, target, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.Query, a.CheckIntervalMs, a.CooldownMs, a.Target, a.Enabled, time.Now().UnixMilli())
	return err
}

func (s *AlertsService) Update(ctx context.Context, a Alert) error {
	if err := validateAlert(&a); err != nil {
		return err
	}
	res, err := s.metaDB.ExecContext(ctx,
		`UPDATE alerts SET name = ?, query = ?, check_interval_ms = ?, cooldown_ms = ?, target = ?, enabled = ?
		 WHERE id = ?`,
		a.Name, a.Query, a.CheckIntervalMs, a.CooldownMs, a.Target, a.Enabled, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("alert not found")
	}
	return nil
}

func (s *AlertsService) Delete(ctx context.Context, id int64) error {
	_, err := s.metaDB.ExecContext(ctx, "DELETE FROM alerts WHERE id = ?", id)
	return err
}

const alertColumns = `id, name, query, check_interval_ms, cooldown_ms, target, enabled,
	last_checked_at, last_fired_at, currently_firing, last_error, last_error_at, created_at`

func scanAlert(row interface{ Scan(...any) error }) (Alert, error) {
	var a Alert
	err := row.Scan(&a.ID, &a.Name, &a.Query, &a.CheckIntervalMs, &a.CooldownMs, &a.Target, &a.Enabled,
		&a.LastCheckedAt, &a.LastFiredAt, &a.CurrentlyFiring, &a.LastError, &a.LastErrorAt, &a.CreatedAt)
	return a, err
}

func (s *AlertsService) Get(ctx context.Context, id int64) (Alert, error) {
	return scanAlert(s.metaDB.QueryRowContext(ctx,
		"SELECT "+alertColumns+" FROM alerts WHERE id = ?", id))
}

func (s *AlertsService) list(ctx context.Context, where string) ([]Alert, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT "+alertColumns+" FROM alerts "+where+" ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *AlertsService) List(ctx context.Context) ([]Alert, error) {
	return s.list(ctx, "")
}

func (s *AlertsService) ListEnabled(ctx context.Context) ([]Alert, error) {
	return s.list(ctx, "WHERE enabled = 1")
}

// UpdateEval records one evaluation outcome. firedAt nil leaves last_fired_at
// untouched (cooldown suppressed or delivery failed - see CONTEXT.md >
// Notification Delivery); evalErr nil clears last_error.
func (s *AlertsService) UpdateEval(ctx context.Context, id int64, firing bool, firedAt *int64, evalErr *string, checkedAt int64) error {
	var errAt *int64
	if evalErr != nil {
		now := time.Now().UnixMilli()
		errAt = &now
	}
	_, err := s.metaDB.ExecContext(ctx,
		`UPDATE alerts SET currently_firing = ?, last_checked_at = ?,
		 last_fired_at = COALESCE(?, last_fired_at), last_error = ?, last_error_at = ? WHERE id = ?`,
		firing, checkedAt, firedAt, evalErr, errAt, id)
	return err
}

func (s *AlertsService) RecordFiring(ctx context.Context, alertID, firedAt int64, matchedRows string, notificationSent bool) error {
	_, err := s.metaDB.ExecContext(ctx,
		"INSERT INTO alert_firings (alert_id, fired_at, matched_rows, notification_sent) VALUES (?, ?, ?, ?)",
		alertID, firedAt, matchedRows, notificationSent)
	return err
}
