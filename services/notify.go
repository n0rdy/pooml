package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/n0rdy/pooml/query"
)

// Target is the per-alert routing config stored in alerts.target.
type Target struct {
	Type     string `json:"type"` // "pushover" | "campfire"
	Device   string `json:"device,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Sound    string `json:"sound,omitempty"`
	RoomID   int64  `json:"room_id,omitempty"`
}

func ParseTarget(raw string) (Target, error) {
	var t Target
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		return t, fmt.Errorf("target config: %w", err)
	}
	switch t.Type {
	case "pushover":
		if t.Priority < -2 || t.Priority > 2 {
			return t, errors.New("pushover priority must be between -2 and 2")
		}
	case "campfire":
		if t.RoomID <= 0 {
			return t, errors.New("campfire target needs a room_id")
		}
	default:
		return t, fmt.Errorf("unknown target type %q", t.Type)
	}
	return t, nil
}

// NotificationService delivers alert notifications. Base URLs are fields so
// tests point them at fakes.
type NotificationService struct {
	settings        *SettingsService
	httpClient      *http.Client
	PushoverBaseURL string
}

func NewNotificationService(settings *SettingsService) *NotificationService {
	return &NotificationService{
		settings:        settings,
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		PushoverBaseURL: "https://api.pushover.net",
	}
}

// SendAlert routes by the alert's target. Message content is built from the
// matched rows: plain text for Pushover, escaped HTML for Campfire (log
// content is untrusted text).
func (n *NotificationService) SendAlert(ctx context.Context, name, targetRaw string, res *query.Result) error {
	t, err := ParseTarget(targetRaw)
	if err != nil {
		return err
	}
	switch t.Type {
	case "pushover":
		return n.sendPushover(ctx, t, "🔥 "+name, plainRows(res))
	case "campfire":
		return n.sendCampfire(ctx, t.RoomID, campfireHTML(name, res))
	}
	return fmt.Errorf("unknown target type %q", t.Type)
}

func (n *NotificationService) TestPushover(ctx context.Context) error {
	return n.sendPushover(ctx, Target{}, "pooml test", "Don't panic - this is only a test.")
}

func (n *NotificationService) TestCampfire(ctx context.Context, roomID int64) error {
	return n.sendCampfire(ctx, roomID, "<b>pooml test</b> - don't panic, this is only a test.")
}

func (n *NotificationService) sendPushover(ctx context.Context, t Target, title, message string) error {
	token, err := n.settings.Get(ctx, SettingPushoverAppToken)
	if err != nil {
		return err
	}
	user, err := n.settings.Get(ctx, SettingPushoverUserKey)
	if err != nil {
		return err
	}
	if token == "" || user == "" {
		return errors.New("pushover is not configured (Settings > Pushover)")
	}

	form := url.Values{
		"token":   {token},
		"user":    {user},
		"title":   {title},
		"message": {message},
	}
	if t.Device != "" {
		form.Set("device", t.Device)
	}
	if t.Priority != 0 {
		form.Set("priority", fmt.Sprint(t.Priority))
	}
	if t.Sound != "" {
		form.Set("sound", t.Sound)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.PushoverBaseURL+"/1/messages.json", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushover: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	var parsed struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(body, &parsed)
	if resp.StatusCode != http.StatusOK || parsed.Status != 1 {
		if len(parsed.Errors) > 0 {
			return fmt.Errorf("pushover rejected the message: %s", strings.Join(parsed.Errors, "; "))
		}
		return fmt.Errorf("pushover rejected the message: HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendCampfire posts to Once Campfire. Contract per CONTEXT.md > Campfire
// Integration: the bot key is a path segment, so errors must never include
// the URL; success is 201 and 2xx must be asserted explicitly (a swallowed
// 3xx would count a lost notification as delivered).
func (n *NotificationService) sendCampfire(ctx context.Context, roomID int64, messageHTML string) error {
	base, err := n.settings.Get(ctx, SettingCampfireBaseURL)
	if err != nil {
		return err
	}
	botKey, err := n.settings.Get(ctx, SettingCampfireBotKey)
	if err != nil {
		return err
	}
	if base == "" || botKey == "" {
		return errors.New("campfire is not configured (Settings > Campfire)")
	}

	endpoint := fmt.Sprintf("%s/rooms/%d/%s/messages", strings.TrimSuffix(base, "/"), roomID, botKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(messageHTML))
	if err != nil {
		return errors.New("campfire: building request failed")
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")

	resp, err := n.httpClient.Do(req)
	if err != nil {
		// url.Error would embed the endpoint (and the bot key); redact
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("campfire: %s request failed: %w", uerr.Op, uerr.Err)
		}
		return errors.New("campfire: request failed")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("campfire rejected the message: HTTP %d", resp.StatusCode)
	}
	return nil
}

const notifyMaxRows = 5

// plainRows renders matched rows as compact plain text for Pushover.
func plainRows(res *query.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d matching row(s)\n", len(res.Rows))
	for i, row := range res.Rows {
		if i >= notifyMaxRows {
			fmt.Fprintf(&b, "… and %d more", len(res.Rows)-notifyMaxRows)
			break
		}
		b.WriteString(compactRow(res.Columns, row, 200))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// campfireHTML renders the alert as Campfire's sanitized HTML subset; every
// interpolated value is escaped because log content is untrusted.
func campfireHTML(name string, res *query.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>🔥 %s</b><br>%d matching row(s)", html.EscapeString(name), len(res.Rows))
	for i, row := range res.Rows {
		if i >= notifyMaxRows {
			fmt.Fprintf(&b, "<br>… and %d more", len(res.Rows)-notifyMaxRows)
			break
		}
		fmt.Fprintf(&b, "<br><code>%s</code>", html.EscapeString(compactRow(res.Columns, row, 200)))
	}
	return b.String()
}

func compactRow(cols []string, row []any, maxLen int) string {
	parts := make([]string, 0, len(cols))
	for i, c := range cols {
		if i >= len(row) || row[i] == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", c, row[i]))
	}
	s := strings.Join(parts, " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}
