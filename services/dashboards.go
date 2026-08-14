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

var chartTypes = map[string]bool{"": true, "line": true, "bar": true, "stat": true}

type Dashboard struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   int64
}

type Panel struct {
	ID          int64
	DashboardID int64
	Title       string
	Query       string
	ChartType   string // "" = auto-detect from result shape
	Position    int64
	Width       int64 // 1-3 grid columns
}

type DashboardsService struct {
	metaDB *sql.DB
}

func NewDashboardsService(metaDB *sql.DB) *DashboardsService {
	return &DashboardsService{metaDB: metaDB}
}

func (s *DashboardsService) CreateDashboard(ctx context.Context, name, description string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return 0, errors.New("name must be 1-100 characters")
	}
	res, err := s.metaDB.ExecContext(ctx,
		"INSERT INTO dashboards (name, description, created_at) VALUES (?, ?, ?)",
		name, strings.TrimSpace(description), time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *DashboardsService) ListDashboards(ctx context.Context) ([]Dashboard, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT id, name, COALESCE(description, ''), created_at FROM dashboards ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Dashboard
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *DashboardsService) GetDashboard(ctx context.Context, id int64) (Dashboard, error) {
	var d Dashboard
	err := s.metaDB.QueryRowContext(ctx,
		"SELECT id, name, COALESCE(description, ''), created_at FROM dashboards WHERE id = ?", id).
		Scan(&d.ID, &d.Name, &d.Description, &d.CreatedAt)
	return d, err
}

// DeleteDashboard relies on ON DELETE CASCADE for the panels (foreign_keys=ON
// is a meta-pool pragma).
func (s *DashboardsService) DeleteDashboard(ctx context.Context, id int64) error {
	_, err := s.metaDB.ExecContext(ctx, "DELETE FROM dashboards WHERE id = ?", id)
	return err
}

func validatePanel(p *Panel) error {
	p.Title = strings.TrimSpace(p.Title)
	p.Query = strings.TrimSpace(p.Query)
	if p.Title == "" || len(p.Title) > 100 {
		return errors.New("title must be 1-100 characters")
	}
	if _, err := query.Validate(p.Query); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if !chartTypes[p.ChartType] {
		return errors.New("chart type must be line, bar, stat, or auto")
	}
	if p.Width < 1 || p.Width > 3 {
		return errors.New("width must be 1-3 columns")
	}
	return nil
}

func (s *DashboardsService) CreatePanel(ctx context.Context, p Panel) error {
	if err := validatePanel(&p); err != nil {
		return err
	}
	_, err := s.metaDB.ExecContext(ctx,
		`INSERT INTO dashboard_panels (dashboard_id, title, query, chart_type, position, width)
		 VALUES (?, ?, ?, ?, COALESCE((SELECT MAX(position) + 1 FROM dashboard_panels WHERE dashboard_id = ?), 0), ?)`,
		p.DashboardID, p.Title, p.Query, nullIfEmpty(p.ChartType), p.DashboardID, p.Width)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY") {
		return errors.New("dashboard not found")
	}
	return err
}

func (s *DashboardsService) UpdatePanel(ctx context.Context, p Panel) error {
	if err := validatePanel(&p); err != nil {
		return err
	}
	res, err := s.metaDB.ExecContext(ctx,
		"UPDATE dashboard_panels SET title = ?, query = ?, chart_type = ?, width = ? WHERE id = ? AND dashboard_id = ?",
		p.Title, p.Query, nullIfEmpty(p.ChartType), p.Width, p.ID, p.DashboardID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("panel not found")
	}
	return nil
}

func (s *DashboardsService) DeletePanel(ctx context.Context, dashboardID, panelID int64) error {
	_, err := s.metaDB.ExecContext(ctx,
		"DELETE FROM dashboard_panels WHERE id = ? AND dashboard_id = ?", panelID, dashboardID)
	return err
}

const panelColumns = "id, dashboard_id, title, query, COALESCE(chart_type, ''), position, width"

func scanPanel(row interface{ Scan(...any) error }) (Panel, error) {
	var p Panel
	err := row.Scan(&p.ID, &p.DashboardID, &p.Title, &p.Query, &p.ChartType, &p.Position, &p.Width)
	return p, err
}

func (s *DashboardsService) GetPanel(ctx context.Context, dashboardID, panelID int64) (Panel, error) {
	return scanPanel(s.metaDB.QueryRowContext(ctx,
		"SELECT "+panelColumns+" FROM dashboard_panels WHERE id = ? AND dashboard_id = ?", panelID, dashboardID))
}

func (s *DashboardsService) ListPanels(ctx context.Context, dashboardID int64) ([]Panel, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT "+panelColumns+" FROM dashboard_panels WHERE dashboard_id = ? ORDER BY position, id", dashboardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Panel
	for rows.Next() {
		p, err := scanPanel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
