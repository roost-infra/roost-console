package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Server) relayControlRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/orgs/{orgID}/relay/preferences", s.withUser(s.relayPreferences))
	mux.HandleFunc("PUT /v1/orgs/{orgID}/relay/preferences/{scope}", s.withUser(s.relayPreferences))
	mux.HandleFunc("GET /v1/orgs/{orgID}/relay/memories", s.withUser(s.relayMemories))
	mux.HandleFunc("POST /v1/orgs/{orgID}/relay/memories", s.withUser(s.relayMemories))
	mux.HandleFunc("PUT /v1/orgs/{orgID}/relay/memories/{memoryID}", s.withUser(s.relayMemories))
	mux.HandleFunc("POST /v1/orgs/{orgID}/relay/memories/delete", s.withUser(s.relayDeleteMemories))
	mux.HandleFunc("GET /v1/orgs/{orgID}/relay/memory-events", s.withUser(s.relayMemoryEvents))
	mux.HandleFunc("POST /v1/relay/preferences", s.withService(s.relayRequestPreferences))
	mux.HandleFunc("POST /v1/relay/memory", s.withService(s.relayRequestMemory))
}

func (s *Server) relayPreferences(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgID")
	if !s.requireOrg(w, r, org) {
		return
	}
	ctx, user := r.Context(), userFrom(r).ID
	if r.Method == http.MethodPut {
		var value billing.RelayPreferences
		if !decodeRelayBody(w, r, &value) {
			return
		}
		if value.CompressionPercent != nil && (*value.CompressionPercent < 0 || *value.CompressionPercent > 100) {
			writeError(w, http.StatusBadRequest, "invalid_percent", "compression percent must be between 0 and 100")
			return
		}
		principal := user
		switch r.PathValue("scope") {
		case "personal":
		case "organization":
			role, err := s.membership.Membership(ctx, org, user)
			if err != nil || (role != "owner" && role != "admin") {
				writeError(w, http.StatusForbidden, "forbidden", "organization administrator required")
				return
			}
			principal = ""
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown preference scope")
			return
		}
		if err := s.store.SaveRelayPreferences(ctx, org, principal, value); err != nil {
			s.fail(w, err)
			return
		}
	}
	set, err := s.store.RelayPreferences(ctx, org, user)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) relayRequestPreferences(w http.ResponseWriter, r *http.Request) {
	var input struct {
		OrgID  string `json:"org_id"`
		UserID string `json:"user_id"`
	}
	if !decodeRelayBody(w, r, &input) {
		return
	}
	if !validRelayID(input.OrgID) || (input.UserID != "" && !validRelayID(input.UserID)) {
		writeError(w, http.StatusBadRequest, "invalid_subject", "invalid relay subject")
		return
	}
	set, err := s.store.RelayPreferences(r.Context(), input.OrgID, input.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "relay subject no longer belongs to the organization")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

func (s *Server) relayMemories(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgID")
	if !s.requireOrg(w, r, org) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, user := r.Context(), userFrom(r).ID
	if r.Method == http.MethodGet {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		search := strings.TrimSpace(r.URL.Query().Get("q"))
		if offset < 0 || utf8.RuneCountInString(search) > 500 {
			writeError(w, http.StatusBadRequest, "invalid_query", "invalid memory list query")
			return
		}
		items, err := s.store.RelayMemories(ctx, org, user, search, offset)
		if err != nil {
			s.fail(w, err)
			return
		}
		more := len(items) > 50
		if more {
			items = items[:50]
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "more": more})
		return
	}
	var item billing.RelayMemory
	if !decodeRelayBody(w, r, &item) {
		return
	}
	item.ID = r.PathValue("memoryID")
	item.Body = strings.TrimSpace(item.Body)
	if !validRelayMemoryText(item.Body, 4000) || (r.Method == http.MethodPut && (!validRelayID(item.ID) || item.Version < 1)) {
		writeError(w, http.StatusBadRequest, "invalid_memory", "enter 1–4000 characters and use the current memory version")
		return
	}
	saved, err := s.store.SaveRelayMemory(ctx, org, user, item)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "memory_changed", "memory changed or was deleted; reload and try again")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) relayDeleteMemories(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgID")
	if !s.requireOrg(w, r, org) {
		return
	}
	var input struct {
		IDs []string `json:"ids"`
		All bool     `json:"all"`
	}
	if !decodeRelayBody(w, r, &input) {
		return
	}
	if (input.All && len(input.IDs) > 0) || (!input.All && (len(input.IDs) == 0 || len(input.IDs) > 50)) {
		writeError(w, http.StatusBadRequest, "invalid_selection", "select up to 50 memories or confirm clear all")
		return
	}
	for _, id := range input.IDs {
		if !validRelayID(id) {
			writeError(w, http.StatusBadRequest, "invalid_selection", "invalid memory ID")
			return
		}
	}
	deleted, err := s.store.DeleteRelayMemories(r.Context(), org, userFrom(r).ID, input.IDs, input.All)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

func (s *Server) relayMemoryEvents(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("orgID")
	if !s.requireOrg(w, r, org) {
		return
	}
	var after int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid memory event cursor")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	events, err := s.store.RelayMemoryEvents(r.Context(), org, userFrom(r).ID, after)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) relayRequestMemory(w http.ResponseWriter, r *http.Request) {
	var input billing.RelayMemoryCall
	if !decodeRelayBody(w, r, &input) {
		return
	}
	valid := validRelayID(input.OrgID) && validRelayID(input.UserID) && validRelayMemoryText(input.RequestID, 200)
	switch input.Action {
	case "search":
		valid = valid && validRelayMemoryText(input.Text, 400)
	case "write":
		valid = valid && validRelayMemoryText(input.Text, 4000)
	case "start", "search_error", "write_error":
	default:
		valid = false
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_memory_call", "invalid personal memory call")
		return
	}
	items, err := s.store.RunRelayMemory(r.Context(), input)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, billing.ErrNotFound) {
		writeError(w, http.StatusConflict, "memory_disabled_or_changed", "personal memory settings changed")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func validRelayID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}

func validRelayMemoryText(value string, max int) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, 0) && utf8.RuneCountInString(value) <= max
}
