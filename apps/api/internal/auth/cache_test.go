package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func identityServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		_ = json.NewEncoder(w).Encode(User{ID: "user-1", Email: "a@example.com"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestIdentityIsCachedBetweenRequests(t *testing.T) {
	var hits int32
	srv := identityServer(t, &hits)
	v := NewVerifier(srv.URL, "anon", srv.Client())

	for i := 0; i < 5; i++ {
		u, err := v.User(context.Background(), "token-a")
		if err != nil || u.ID != "user-1" {
			t.Fatalf("call %d: %+v %v", i, u, err)
		}
	}
	if hits != 1 {
		t.Fatalf("provider was asked %d times, want 1", hits)
	}
}

func TestCacheTTLIsConfigurable(t *testing.T) {
	var hits int32
	srv := identityServer(t, &hits)
	v := NewVerifier(srv.URL, "anon", srv.Client())
	v.SetCacheTTL(20 * time.Millisecond)

	if _, err := v.User(context.Background(), "token-a"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := v.User(context.Background(), "token-a"); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("expired entry was not refreshed: %d provider calls", hits)
	}
}

func TestCacheTTLRejectsNonsense(t *testing.T) {
	var hits int32
	srv := identityServer(t, &hits)
	v := NewVerifier(srv.URL, "anon", srv.Client())
	v.SetCacheTTL(-time.Second) // falls back to the default, never to "no cache"

	if _, err := v.User(context.Background(), "token-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.User(context.Background(), "token-a"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("negative TTL disabled the cache: %d provider calls", hits)
	}
}
