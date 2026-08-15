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

// Metrics panels pick a chart shape; logs panels pick a KIND - stream is the
// primary logs use case ("show me my errors"), chart covers aggregations,
// number a single count.
var (
	metricsPanelKinds = map[string]bool{"": true, "line": true, "bar": true, "stat": true}
	logsPanelKinds    = map[string]bool{"stream": true, "chart": true, "number": true}
)

// Dashboard is TYPED: it shows one signal ("logs" or "metrics") and every
// panel in it validates against that signal's scope. Decided after the
// untyped design kept forcing the UI to ask "which signal?" per panel -
// architectural answer over UI patches; cross-signal screens are the future
// War Room's job.
type Dashboard struct {
	ID          int64
	Name        string
	Type        string
	Description string
	CreatedAt   int64
}

var dashboardTypes = map[string]bool{"logs": true, "metrics": true}

type Panel struct {
	ID          int64
	DashboardID int64
	Title       string
	Query       string
	ChartType   string // "" = auto-detect from result shape
	Position    int64
	Width       int64 // 1-3 grid columns

	// assisted inputs, remembered for editing parity with the explorer/logs
	// page. Query stays canonical: DSL compiles into it at save (metrics),
	// FTS combines into it at render (logs).
	DSL string
	FTS string
	Op  string // "and" (default) or "or": how FTS combines with the SQL
}

type DashboardsService struct {
	metaDB *sql.DB
}

func NewDashboardsService(metaDB *sql.DB) *DashboardsService {
	return &DashboardsService{metaDB: metaDB}
}

func (s *DashboardsService) CreateDashboard(ctx context.Context, name, dtype, description string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return 0, errors.New("name must be 1-100 characters")
	}
	if !dashboardTypes[dtype] {
		return 0, errors.New("a dashboard shows logs or metrics; pick one")
	}
	res, err := s.metaDB.ExecContext(ctx,
		"INSERT INTO dashboards (name, type, description, created_at) VALUES (?, ?, ?, ?)",
		name, dtype, strings.TrimSpace(description), time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *DashboardsService) ListDashboards(ctx context.Context) ([]Dashboard, error) {
	rows, err := s.metaDB.QueryContext(ctx,
		"SELECT id, name, type, COALESCE(description, ''), created_at FROM dashboards ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Dashboard
	for rows.Next() {
		var d Dashboard
		if err := rows.Scan(&d.ID, &d.Name, &d.Type, &d.Description, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *DashboardsService) GetDashboard(ctx context.Context, id int64) (Dashboard, error) {
	var d Dashboard
	err := s.metaDB.QueryRowContext(ctx,
		"SELECT id, name, type, COALESCE(description, ''), created_at FROM dashboards WHERE id = ?", id).
		Scan(&d.ID, &d.Name, &d.Type, &d.Description, &d.CreatedAt)
	return d, err
}

// DeleteDashboard relies on ON DELETE CASCADE for the panels (foreign_keys=ON
// is a meta-pool pragma).
func (s *DashboardsService) DeleteDashboard(ctx context.Context, id int64) error {
	_, err := s.metaDB.ExecContext(ctx, "DELETE FROM dashboards WHERE id = ?", id)
	return err
}

// PanelScope maps a dashboard's type to the validation scope its panels get.
func PanelScope(dashboardType string) query.Scope {
	if dashboardType == "logs" {
		return query.ScopeLogs
	}
	return query.ScopeMetrics
}

func (s *DashboardsService) validatePanel(ctx context.Context, p *Panel) error {
	p.Title = strings.TrimSpace(p.Title)
	p.Query = strings.TrimSpace(p.Query)
	if p.Title == "" || len(p.Title) > 100 {
		return errors.New("title must be 1-100 characters")
	}
	d, err := s.GetDashboard(ctx, p.DashboardID)
	if err != nil {
		return errors.New("dashboard not found")
	}
	v, err := query.ValidateIn(p.Query, PanelScope(d.Type))
	if err != nil {
		return fmt.Errorf("query (this is a %s dashboard): %w", d.Type, err)
	}
	if d.Type == "logs" {
		if p.ChartType == "" {
			p.ChartType = "stream"
		}
		if p.Op != "or" {
			p.Op = "and"
		}
		if !logsPanelKinds[p.ChartType] {
			return errors.New("a logs panel shows a stream, a chart, or a number")
		}
		if p.ChartType == "stream" && v.Shape != query.ShapeLogViewer {
			return errors.New("a stream panel lists raw log lines - aggregations belong in a chart or number panel")
		}
	} else if !metricsPanelKinds[p.ChartType] {
		return errors.New("chart type must be line, bar, stat, or auto")
	}
	if p.Width < 1 || p.Width > 3 {
		return errors.New("width must be 1-3 columns")
	}
	return nil
}

func (s *DashboardsService) CreatePanel(ctx context.Context, p Panel) error {
	if err := s.validatePanel(ctx, &p); err != nil {
		return err
	}
	_, err := s.metaDB.ExecContext(ctx,
		`INSERT INTO dashboard_panels (dashboard_id, title, query, chart_type, position, width, dsl, fts, op)
		 VALUES (?, ?, ?, ?, COALESCE((SELECT MAX(position) + 1 FROM dashboard_panels WHERE dashboard_id = ?), 0), ?, ?, ?, ?)`,
		p.DashboardID, p.Title, p.Query, nullIfEmpty(p.ChartType), p.DashboardID, p.Width, nullIfEmpty(p.DSL), nullIfEmpty(p.FTS), nullIfEmpty(p.Op))
	return err
}

func (s *DashboardsService) UpdatePanel(ctx context.Context, p Panel) error {
	if err := s.validatePanel(ctx, &p); err != nil {
		return err
	}
	res, err := s.metaDB.ExecContext(ctx,
		"UPDATE dashboard_panels SET title = ?, query = ?, chart_type = ?, width = ?, dsl = ?, fts = ?, op = ? WHERE id = ? AND dashboard_id = ?",
		p.Title, p.Query, nullIfEmpty(p.ChartType), p.Width, nullIfEmpty(p.DSL), nullIfEmpty(p.FTS), nullIfEmpty(p.Op), p.ID, p.DashboardID)
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

const panelColumns = "id, dashboard_id, title, query, COALESCE(chart_type, ''), position, width, COALESCE(dsl, ''), COALESCE(fts, ''), COALESCE(op, '')"

func scanPanel(row interface{ Scan(...any) error }) (Panel, error) {
	var p Panel
	err := row.Scan(&p.ID, &p.DashboardID, &p.Title, &p.Query, &p.ChartType, &p.Position, &p.Width, &p.DSL, &p.FTS, &p.Op)
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
