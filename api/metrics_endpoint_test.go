package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/n0rdy/pooml/services"
)

func TestMetricsEndpoint(t *testing.T) {
	_, srv, _ := setup(t)

	// no key: 401
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", resp.StatusCode)
	}

	// wrong key: 401
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("X-API-Key", "nope")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", resp.StatusCode)
	}

	// right key: Prometheus exposition incl. pooml's own counters
	services.RetentionDeleted.WithLabelValues("logs").Add(0)
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("X-API-Key", "metrics-test-secret-0123456789012345")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with key = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"go_goroutines", "pooml_retention_deleted_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}
