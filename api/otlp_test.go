package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n0rdy/pooml/api"
	"github.com/n0rdy/pooml/db"
	"github.com/n0rdy/pooml/ingestion"
	"github.com/n0rdy/pooml/metrics"
	"github.com/n0rdy/pooml/services"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func setup(t *testing.T) (*db.Pools, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	db.MigrateAll(dir)
	pools, err := db.OpenPools(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pools.Close)

	broadcaster := ingestion.NewBroadcaster()
	pipeline := ingestion.NewPipeline(pools.LogsWrite, broadcaster)
	pipeline.Start()
	t.Cleanup(pipeline.Shutdown)

	metricsPipeline := metrics.NewPipeline(pools.Metrics)
	metricsPipeline.Start()
	t.Cleanup(metricsPipeline.Shutdown)

	apiKeys := services.NewApiKeysService(pools.Meta)
	key, err := apiKeys.Create(context.Background(), "otlp-test")
	if err != nil {
		t.Fatal(err)
	}

	router := api.NewRouter(
		services.NewMonitoringService(pools),
		services.NewThrottlingService(),
		apiKeys, pipeline, metricsPipeline, "local", false,
		"metrics-test-secret-0123456789012345",
		api.QueryAPI{Secret: "query-test-secret-01234567890123456", LogsRead: pools.LogsRead, Metrics: pools.Metrics},
	)
	srv := httptest.NewServer(router.NewRouter())
	t.Cleanup(srv.Close)
	return pools, srv, key
}

func postOTLP(t *testing.T, srv *httptest.Server, contentType, apiKey string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/otlp/v1/metrics", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func waitForMetricRows(t *testing.T, mdb *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if err := mdb.QueryRow("SELECT COUNT(*) FROM metrics").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d metric rows, got %d", want, got)
}

func TestOTLPPushJSON(t *testing.T) {
	pools, srv, key := setup(t)

	body := `{
		"resourceMetrics": [{
			"resource": {"attributes": [
				{"key": "service.name", "value": {"stringValue": "shop"}},
				{"key": "host.name", "value": {"stringValue": "h1"}}
			]},
			"scopeMetrics": [{"metrics": [
				{"name": "orders_total", "sum": {"isMonotonic": true, "aggregationTemporality": 2, "dataPoints": [
					{"timeUnixNano": "1723600000000000000", "asInt": "42",
					 "attributes": [{"key": "status", "value": {"stringValue": "ok"}}]}
				]}},
				{"name": "queue_depth", "gauge": {"dataPoints": [
					{"timeUnixNano": "1723600000000000000", "asDouble": 7.5}
				]}},
				{"name": "req_latency", "histogram": {"dataPoints": [
					{"timeUnixNano": "1723600000000000000", "count": "10", "sum": 123.4}
				]}}
			]}]
		}]
	}`
	resp := postOTLP(t, srv, "application/json", key, []byte(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != "{}" {
		t.Fatalf("expected {} response body, got %q", respBody)
	}

	// gauge + counter + histogram downcast (_sum, _count) = 4 rows
	waitForMetricRows(t, pools.Metrics, 4)

	var typ int
	var value float64
	var service, host string
	var labels sql.NullString
	err := pools.Metrics.QueryRow(
		"SELECT type, value, service, host, labels FROM metrics WHERE name = 'orders_total'",
	).Scan(&typ, &value, &service, &host, &labels)
	if err != nil {
		t.Fatal(err)
	}
	if typ != metrics.TypeCounter || value != 42 || service != "shop" || host != "h1" {
		t.Fatalf("counter row wrong: type=%d value=%v service=%s host=%s", typ, value, service, host)
	}
	if !labels.Valid || labels.String != `{"status":"ok"}` {
		t.Fatalf("labels = %v", labels)
	}

	var ts int64
	if err := pools.Metrics.QueryRow("SELECT timestamp FROM metrics WHERE name = 'queue_depth'").Scan(&ts); err != nil {
		t.Fatal(err)
	}
	if ts != 1723600000000 {
		t.Fatalf("timestamp not converted nano->ms: %d", ts)
	}

	var n int
	if err := pools.Metrics.QueryRow(
		"SELECT COUNT(*) FROM metrics WHERE name IN ('req_latency_sum', 'req_latency_count') AND type = ?",
		metrics.TypeCounter, // downcast rows are cumulative, typed as counters
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected histogram downcast to 2 counter rows, got %d", n)
	}
}

func TestOTLPPushProtobuf(t *testing.T) {
	pools, srv, key := setup(t)

	data := &metricspb.MetricsData{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "cpu_load",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: 1723600000000000000,
							Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.42},
						}},
					}},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}

	resp := postOTLP(t, srv, "application/x-protobuf", key, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-protobuf" {
		t.Fatalf("response content type = %q", ct)
	}
	waitForMetricRows(t, pools.Metrics, 1)

	// no resource attrs: identity falls back to unknown/unknown
	var service, host string
	if err := pools.Metrics.QueryRow("SELECT service, host FROM metrics WHERE name = 'cpu_load'").Scan(&service, &host); err != nil {
		t.Fatal(err)
	}
	if service != "unknown" || host != "unknown" {
		t.Fatalf("fallback identity wrong: %s/%s", service, host)
	}
}

func TestOTLPSkipsNoRecordedValuePoints(t *testing.T) {
	pools, srv, key := setup(t)

	data := &metricspb.MetricsData{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "flaky_gauge",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{
							{
								TimeUnixNano: 1723600000000000000,
								Flags:        uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK),
								Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 1},
							},
							{
								TimeUnixNano: 1723600001000000000,
								Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 2},
							},
						},
					}},
				}},
			}},
		}},
	}
	body, err := proto.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	resp := postOTLP(t, srv, "application/x-protobuf", key, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitForMetricRows(t, pools.Metrics, 1)

	var value float64
	if err := pools.Metrics.QueryRow("SELECT value FROM metrics WHERE name = 'flaky_gauge'").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != 2 {
		t.Fatalf("kept the wrong point: %v", value)
	}
}

func TestOTLPRejections(t *testing.T) {
	_, srv, key := setup(t)

	if resp := postOTLP(t, srv, "application/json", "", []byte("{}")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key: status = %d", resp.StatusCode)
	}
	if resp := postOTLP(t, srv, "text/plain", key, []byte("hello")); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type: status = %d", resp.StatusCode)
	}
	if resp := postOTLP(t, srv, "application/json", key, []byte("not json")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad body: status = %d", resp.StatusCode)
	}
}
