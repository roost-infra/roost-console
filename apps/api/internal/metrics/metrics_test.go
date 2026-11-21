package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestObserveHTTPExposition(t *testing.T) {
	r := NewRegistry()
	r.AddInFlight(2)
	r.ObserveHTTP("GET", "GET /healthz", 200, 0.01)
	r.ObserveHTTP("GET", "GET /healthz", 200, 0.2)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`roost_console_http_requests_total{method="GET",route="GET /healthz",status="200"} 2`,
		`roost_console_http_request_duration_seconds_count{method="GET",route="GET /healthz"} 2`,
		`roost_console_http_in_flight 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q\n%s", want, body)
		}
	}
}
