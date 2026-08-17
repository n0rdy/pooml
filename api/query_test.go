package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/api"
	"github.com/n0rdy/pooml/services"
)

const queryTestSecret = "query-test-secret-01234567890123456"

func postQuery(t *testing.T, srv *httptest.Server, path, secret, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-API-Key", secret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestQueryAPI(t *testing.T) {
	pools, srv, ingestKey := setup(t)

	for i := range 5 {
		if _, err := pools.LogsWrite.Exec(
			"INSERT INTO logs(timestamp, ingested_at, level, service, host, raw) VALUES (?,?,4,'pay-svc','h1','boom')",
			1723600000000+int64(i), 1723600000000); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pools.Metrics.Exec(
		"INSERT INTO metrics(timestamp, name, type, value, service, host) VALUES (1723600000000,'queue_depth',1,7,'shop','h1')"); err != nil {
		t.Fatal(err)
	}

	// no secret: 401
	if code, _ := postQuery(t, srv, "/api/v1/query/logs", "", `{"sql":"SELECT 1 FROM logs"}`); code != http.StatusUnauthorized {
		t.Fatalf("no secret = %d, want 401", code)
	}
	// the INGEST key must NOT work here: reads use a different credential
	if code, _ := postQuery(t, srv, "/api/v1/query/logs", ingestKey, `{"sql":"SELECT 1 FROM logs"}`); code != http.StatusUnauthorized {
		t.Fatalf("ingest key accepted on query API = %d, want 401", code)
	}

	// happy path
	code, body := postQuery(t, srv, "/api/v1/query/logs", queryTestSecret,
		`{"sql":"SELECT service, raw FROM logs WHERE level >= 4 ORDER BY id"}`)
	if code != http.StatusOK {
		t.Fatalf("logs query = %d: %s", code, body)
	}
	var res struct {
		Columns   []string `json:"columns"`
		Rows      [][]any  `json:"rows"`
		RowCount  int      `json:"row_count"`
		Truncated bool     `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 5 || res.Truncated || res.Columns[0] != "service" || res.Rows[0][1] != "boom" {
		t.Fatalf("unexpected result: %+v", res)
	}

	// max_rows truncates and says so
	code, body = postQuery(t, srv, "/api/v1/query/logs", queryTestSecret,
		`{"sql":"SELECT id FROM logs ORDER BY id","max_rows":2}`)
	if code != http.StatusOK {
		t.Fatalf("max_rows query = %d", code)
	}
	_ = json.Unmarshal([]byte(body), &res)
	if res.RowCount != 2 || !res.Truncated {
		t.Fatalf("max_rows: count=%d truncated=%v", res.RowCount, res.Truncated)
	}

	// scope isolation: metrics table via the logs endpoint
	code, body = postQuery(t, srv, "/api/v1/query/logs", queryTestSecret, `{"sql":"SELECT * FROM metrics"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "message") {
		t.Fatalf("scope violation = %d: %s", code, body)
	}
	// writes rejected by the validator
	if code, _ = postQuery(t, srv, "/api/v1/query/logs", queryTestSecret, `{"sql":"DELETE FROM logs"}`); code != http.StatusBadRequest {
		t.Fatalf("DELETE = %d, want 400", code)
	}
	// validation errors carry an actionable message
	code, body = postQuery(t, srv, "/api/v1/query/logs", queryTestSecret, `{"sql":"SELECT * FROM api_keys"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "not allowed") {
		t.Fatalf("forbidden table = %d: %s", code, body)
	}

	// metrics endpoint + catalog
	code, body = postQuery(t, srv, "/api/v1/query/metrics", queryTestSecret,
		`{"sql":"SELECT name, value FROM metrics"}`)
	if code != http.StatusOK || !strings.Contains(body, "queue_depth") {
		t.Fatalf("metrics query = %d: %s", code, body)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/query/catalog", nil)
	req.Header.Set("X-API-Key", queryTestSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	catBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(catBody), `"gauge"`) {
		t.Fatalf("catalog = %d: %s", resp.StatusCode, catBody)
	}
}

func TestQueryAPIDisabled(t *testing.T) {
	pools, _, _ := setup(t)
	router := api.NewRouter(
		services.NewMonitoringService(pools),
		services.NewThrottlingService(),
		services.NewApiKeysService(pools.Meta), nil, nil, "local", false, "",
		api.QueryAPI{}, // no secret = the routes must not exist
	)
	srv := httptest.NewServer(router.NewRouter())
	defer srv.Close()
	if code, _ := postQuery(t, srv, "/api/v1/query/logs", queryTestSecret, `{"sql":"SELECT 1 FROM logs"}`); code != http.StatusNotFound && code != http.StatusUnauthorized {
		t.Fatalf("disabled query API = %d, want 404/401", code)
	}
}
