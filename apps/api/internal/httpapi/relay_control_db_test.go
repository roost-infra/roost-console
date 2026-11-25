package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/auth"
	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/JingxuanC/roost-console/apps/api/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRelayControlCORSAllowsPut(t *testing.T) {
	const origin = "https://console.example"
	handler := New(config.Config{ConsoleOrigin: origin}, nil, nil, nil, nil, nil).Handler()
	request := httptest.NewRequest(http.MethodOptions, "/v1/orgs/org/relay/preferences/personal", nil)
	request.Header.Set("Origin", origin)
	request.Header.Set("Access-Control-Request-Method", http.MethodPut)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !strings.Contains(response.Header().Get("Access-Control-Allow-Methods"), http.MethodPut) {
		t.Fatalf("preflight = %d; methods = %s", response.Code, response.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestRelayControlSignedRoutesDB(t *testing.T) {
	dsn := os.Getenv("ROOST_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ROOST_TEST_DATABASE_URL to run the relay control integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := billing.New(pool)
	org, user := uuid.NewString(), uuid.NewString()
	if _, err = pool.Exec(ctx, `insert into auth.users (id,email) values ($1,$2)`, user, user+"@test.local"); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(ctx, `delete from auth.users where id=$1`, user)
	if _, err = pool.Exec(ctx, `insert into public.organizations(id,name,slug,owner_user_id) values ($1,'relay',$2,$3)`, org, org, user); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `insert into public.organization_members(org_id,user_id,role) values ($1,$2,'owner')`, org, user); err != nil {
		t.Fatal(err)
	}

	const secret = "local-relay-test"
	handler := New(config.Config{PublicBaseURL: "https://console.example/console-api",
		ServiceSecrets: map[string][]string{"relay": {secret}, "causal": {secret}},
	}, store, nil, nil, nil, nil).Handler()
	now := time.Now()
	post := func(service, path string, data any, want int) []byte {
		t.Helper()
		body, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		now = now.Add(time.Millisecond)
		if service != "" {
			ts, nonce, signature := auth.SignService(service, request.Method, "/console-api"+path, body, secret, now)
			request.Header.Set("X-Roost-Service", service)
			request.Header.Set("X-Roost-Timestamp", ts)
			request.Header.Set("X-Roost-Nonce", nonce)
			request.Header.Set("X-Roost-Signature", signature)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("%s %s: status %d, want %d: %s", service, path, response.Code, want, response.Body)
		}
		return response.Body.Bytes()
	}

	subject := map[string]string{"org_id": org, "user_id": user}
	post("", "/v1/relay/preferences", subject, http.StatusUnauthorized)
	post("causal", "/v1/relay/preferences", subject, http.StatusForbidden)
	var prefs billing.RelayPreferenceSet
	read := func() {
		t.Helper()
		if err := json.Unmarshal(post("relay", "/v1/relay/preferences", subject, http.StatusOK), &prefs); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if prefs.Effective.MemoryEnabled != nil {
		t.Fatalf("unexpected default: %+v", prefs)
	}
	on, percent := true, 30
	if err := store.SaveRelayPreferences(ctx, org, user, billing.RelayPreferences{MemoryEnabled: &on, CompressionPercent: &percent}); err != nil {
		t.Fatal(err)
	}
	read()
	if prefs.Effective.CompressionPercent == nil || *prefs.Effective.CompressionPercent != 30 || prefs.UserRevision == 0 {
		t.Fatalf("preference update not visible: %+v", prefs)
	}
	call := billing.RelayMemoryCall{OrgID: org, UserID: user, RequestID: "signed-request",
		Action: "write", Text: "session memory", OrgRevision: prefs.OrgRevision, UserRevision: prefs.UserRevision}
	post("relay", "/v1/relay/memory", call, http.StatusOK)
	call.Action, call.Text = "search", "session"
	var matches []billing.RelayMemory
	if err := json.Unmarshal(post("relay", "/v1/relay/memory", call, http.StatusOK), &matches); err != nil || len(matches) != 1 {
		t.Fatalf("matches = %+v; err = %v", matches, err)
	}
}
