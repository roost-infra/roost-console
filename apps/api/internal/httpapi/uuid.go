package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Billing ids (lease/reservation/org ids) are Postgres uuid columns. A request
// that reaches the database with a non-UUID string raises SQLSTATE 22P02 and
// used to surface as a 500, hiding a client bug behind an "internal error".
// Handlers validate ids at the edge and answer 400 instead; the pg-error
// mapping in fail/relayStoreError is the defense-in-depth path for endpoints
// that have not been annotated yet.

// isUUID reports whether value parses as a UUID (any accepted textual form).
func isUUID(value string) bool {
	_, err := uuid.Parse(strings.TrimSpace(value))
	return err == nil
}

// requireUUIDPath reads a {name} path value and rejects a non-UUID with 400.
// The caller must return when ok is false.
func requireUUIDPath(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := strings.TrimSpace(r.PathValue(name))
	if !isUUID(value) {
		writeError(w, http.StatusBadRequest, "bad_request", name+" must be a UUID")
		return "", false
	}
	return value, true
}

// invalidUUIDField validates a decoded request field and rejects a non-UUID
// with 400. The caller must return when ok is false.
func invalidUUIDField(w http.ResponseWriter, field, value string) bool {
	if isUUID(value) {
		return true
	}
	writeError(w, http.StatusBadRequest, "bad_request", field+" must be a UUID")
	return false
}

// isUUIDParseError recognizes the Postgres "invalid input syntax for type
// uuid" error (SQLSTATE 22P02). Callers map it to 400 so a malformed id cannot
// become a 500.
func isUUIDParseError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		return false
	}
	return strings.Contains(strings.ToLower(pgErr.Message), "uuid")
}
