package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func uuidTestServer() *Server {
	return &Server{
		relay: &fakeRelayBilling{},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	body := decodeRelayResponse(t, rec)
	errBody, _ := body["error"].(map[string]any)
	code, _ := errBody["code"].(string)
	return code
}

// A non-UUID path segment used to reach the store and fail the Postgres uuid
// cast as a 500. These handlers must answer 400 before touching the store.
func TestLeaseHandlersRejectNonUUIDIDs(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
	}{
		{"settle", uuidTestServer().settle, "/internal/v1/usage/leases/not-a-uuid/settle", `{"actual_credits":1}`},
		{"release", uuidTestServer().release, "/internal/v1/usage/leases/not-a-uuid/release", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.SetPathValue("leaseID", "not-a-uuid")
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "bad_request" {
				t.Fatalf("code=%q body=%s", code, rec.Body.String())
			}
		})
	}
}

func TestRunHandlersRejectNonUUIDOrgID(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		body    string
	}{
		{"reserve", uuidTestServer().reserve, `{"org_id":"org-1","product":"relay","run_ref":"run-1","estimated_credits":1}`},
		{"settle by run", uuidTestServer().settleByRun, `{"org_id":"org-1","product":"dsalpha","run_ref":"run-1","actual_credits":1}`},
		{"release by run", uuidTestServer().releaseByRun, `{"org_id":"org-1","product":"dsalpha","run_ref":"run-1"}`},
		{"draw by run", uuidTestServer().drawByRun, `{"org_id":"org-1","product":"market","run_ref":"run-1","ref":"draw-1","credits":1}`},
		{"charge", uuidTestServer().charge, `{"org_id":"org-1","product":"market","ref":"ref-1","credits":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://example/internal/v1/usage", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "bad_request" {
				t.Fatalf("code=%q body=%s", code, rec.Body.String())
			}
		})
	}
}

func TestRelayHandlersRejectNonUUIDReservation(t *testing.T) {
	f := &fakeRelayBilling{}
	s := relayTestServer(f)

	settle := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://example/v1/relay/reservations/res-1/settle", strings.NewReader(`{"idempotency_key":"settle:r1","amount_minor":1}`))
		req.SetPathValue("reservationID", "res-1")
		rec := httptest.NewRecorder()
		s.relaySettle(rec, req)
		return rec
	}
	release := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://example/v1/relay/reservations/res-1/release", nil)
		req.SetPathValue("reservationID", "res-1")
		rec := httptest.NewRecorder()
		s.relayRelease(rec, req)
		return rec
	}
	charges := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://example/v1/relay/reservations/res-1/tool-charges", nil)
		req.SetPathValue("reservationID", "res-1")
		rec := httptest.NewRecorder()
		s.relayReservationToolCharges(rec, req)
		return rec
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"settle": settle(), "release": release(), "tool charges": charges(),
	} {
		t.Run(name, func(t *testing.T) {
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "invalid_request" {
				t.Fatalf("code=%q body=%s", code, rec.Body.String())
			}
		})
	}
	if len(f.settleCalls) != 0 || len(f.settleV2Calls) != 0 {
		t.Fatalf("invalid reservation reached the store: settle=%+v v2=%+v", f.settleCalls, f.settleV2Calls)
	}
}

func TestRelayReserveRejectsNonUUIDSubjectRef(t *testing.T) {
	f := &fakeRelayBilling{}
	rec := postRelayReserve(t, relayTestServer(f), `{
		"request_id":"req-1","subject_ref":"org-1","amount_minor":100,"currency":"CREDIT"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "invalid_request" {
		t.Fatalf("code=%q body=%s", code, rec.Body.String())
	}
	if len(f.reserveCalls) != 0 {
		t.Fatalf("invalid subject_ref reached the store: %+v", f.reserveCalls)
	}
}

// Defense in depth: any id that still reaches Postgres must map the uuid cast
// error to 400, not 500. The handlers above pre-validate; this covers routes
// that have not been annotated yet.
func TestUUIDParseErrorMapsToClientError(t *testing.T) {
	err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type uuid: "res-1"`}

	s := uuidTestServer()
	rec := httptest.NewRecorder()
	s.fail(rec, err)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("fail status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	relayStoreError(rec, err)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("relay status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUUIDParseErrorIgnoresOtherCastErrors(t *testing.T) {
	err := &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "x"`}
	if isUUIDParseError(err) {
		t.Fatal("non-uuid cast error was classified as an invalid UUID")
	}
	if isUUIDParseError(io.ErrUnexpectedEOF) {
		t.Fatal("non-Postgres error was classified as an invalid UUID")
	}
}
