// Package metrics is the console API's minimal Prometheus surface: request
// counts by matched route/status, a latency histogram, and in-flight gauge.
// Route is the ServeMux pattern (r.Pattern), never the raw path, so IDs and
// query strings cannot blow up label cardinality.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Default is the process-wide registry used by the HTTP middleware and the
// loopback /metrics listener.
var Default = NewRegistry()

// Buckets are fixed so the exposition format stays stable.
var Buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type requestKey struct {
	method string
	route  string
	status int
}

type latencyKey struct {
	method string
	route  string
}

type histogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

// Registry is safe for concurrent use.
type Registry struct {
	mu       sync.Mutex
	requests map[requestKey]uint64
	latency  map[latencyKey]*histogram
	inFlight int64
}

func NewRegistry() *Registry {
	return &Registry{
		requests: make(map[requestKey]uint64),
		latency:  make(map[latencyKey]*histogram),
	}
}

// AddInFlight adjusts the in-flight request gauge by delta.
func (r *Registry) AddInFlight(delta int64) {
	r.mu.Lock()
	r.inFlight += delta
	r.mu.Unlock()
}

// ObserveHTTP records one completed request.
func (r *Registry) ObserveHTTP(method, route string, status int, seconds float64) {
	method = sanitize(method)
	route = sanitize(route)
	r.mu.Lock()
	r.requests[requestKey{method: method, route: route, status: status}]++
	h := r.latency[latencyKey{method: method, route: route}]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(Buckets))}
		r.latency[latencyKey{method: method, route: route}] = h
	}
	idx := -1
	for i, bound := range Buckets {
		if seconds <= bound {
			idx = i
			break
		}
	}
	if idx >= 0 {
		h.counts[idx]++
	}
	h.sum += seconds
	h.count++
	r.mu.Unlock()
}

func sanitize(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "other"
	}
	if len(v) > 128 {
		return v[:128]
	}
	return v
}

// Handler serves the Prometheus text exposition.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.mu.Lock()
		requests := make([]requestKey, 0, len(r.requests))
		for k := range r.requests {
			requests = append(requests, k)
		}
		sort.Slice(requests, func(i, j int) bool {
			a, b := requests[i], requests[j]
			if a.route != b.route {
				return a.route < b.route
			}
			if a.method != b.method {
				return a.method < b.method
			}
			return a.status < b.status
		})
		latency := make([]latencyKey, 0, len(r.latency))
		for k := range r.latency {
			latency = append(latency, k)
		}
		sort.Slice(latency, func(i, j int) bool {
			if latency[i].route != latency[j].route {
				return latency[i].route < latency[j].route
			}
			return latency[i].method < latency[j].method
		})

		var b strings.Builder
		b.WriteString("# HELP roost_console_http_requests_total Completed HTTP requests by method, matched route and status.\n")
		b.WriteString("# TYPE roost_console_http_requests_total counter\n")
		for _, k := range requests {
			fmt.Fprintf(&b, "roost_console_http_requests_total{method=%q,route=%q,status=%q} %d\n",
				k.method, k.route, strconv.Itoa(k.status), r.requests[k])
		}
		b.WriteString("# HELP roost_console_http_request_duration_seconds HTTP request latency by method and matched route.\n")
		b.WriteString("# TYPE roost_console_http_request_duration_seconds histogram\n")
		for _, k := range latency {
			h := r.latency[k]
			var cumulative uint64
			for i, bound := range Buckets {
				cumulative += h.counts[i]
				fmt.Fprintf(&b, "roost_console_http_request_duration_seconds_bucket{method=%q,route=%q,le=%q} %d\n",
					k.method, k.route, formatFloat(bound), cumulative)
			}
			fmt.Fprintf(&b, "roost_console_http_request_duration_seconds_bucket{method=%q,route=%q,le=\"+Inf\"} %d\n",
				k.method, k.route, h.count)
			fmt.Fprintf(&b, "roost_console_http_request_duration_seconds_sum{method=%q,route=%q} %s\n",
				k.method, k.route, formatFloat(h.sum))
			fmt.Fprintf(&b, "roost_console_http_request_duration_seconds_count{method=%q,route=%q} %d\n",
				k.method, k.route, h.count)
		}
		b.WriteString("# HELP roost_console_http_in_flight HTTP requests currently being served.\n")
		b.WriteString("# TYPE roost_console_http_in_flight gauge\n")
		fmt.Fprintf(&b, "roost_console_http_in_flight %d\n", r.inFlight)
		r.mu.Unlock()
		_, _ = w.Write([]byte(b.String()))
	})
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
