package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
)

// RelayMilliPerCredit is the relay's minor unit: 1000 minor units = 1 credit.
// The relay estimates and settles in milli-credits so short requests are not
// rounded to a free call; the console stores whole credits.
const RelayMilliPerCredit = 1000

// relayBilling is the slice of the billing store used by relay handlers. It is
// an interface so handler tests can exercise the API without Postgres.
type relayBilling interface {
	RelayAPIKey(ctx context.Context, token string) (billing.RelayIdentity, error)
	RelayBalance(ctx context.Context, orgID string) (int64, error)
	RelayReserve(context.Context, string, string, int64, string, time.Duration) (billing.RelayReservation, error)
	RelayReserveWithDetails(ctx context.Context, in billing.RelayReserveInput) (billing.RelayReservation, error)
	RelaySettleWithDetails(ctx context.Context, reservationID, settlementID, idempotencyKey string, amount int64, toolCharges []billing.ToolCharge, compression *billing.Compression, usage *billing.UsageEvent) (billing.RelayOperation, error)
	RelaySettleV2(ctx context.Context, in billing.RelaySettleInput) (billing.RelayOperation, error)
	RelayRelease(ctx context.Context, reservationID string) (billing.RelayOperation, error)
	RelayOperation(ctx context.Context, idempotencyKey string) (billing.RelayOperation, error)
	RelayToolCharges(ctx context.Context, reservationID string) ([]billing.ToolCharge, error)
}

func relayCreditsFromMinor(minor int64) int64 {
	if minor <= 0 {
		return 0
	}
	return (minor + RelayMilliPerCredit - 1) / RelayMilliPerCredit
}

// Relay endpoints implement the platform side of the relay-aggregator
// contract: introspect a downstream rk- key, hold a request's estimated spend,
// settle actual usage, release unspent holds and answer idempotency queries.
// Every route is service-authenticated; the same secret may arrive as the
// signed service headers or as the legacy X-Service-Token header.

func (s *Server) relayStore() relayBilling {
	if s.relay != nil {
		return s.relay
	}
	if s.store == nil {
		return nil
	}
	return s.store
}

func (s *Server) relayIntrospect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if !decodeRelayBody(w, r, &req) {
		return
	}
	if req.APIKey == "" {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "api_key is required")
		return
	}
	identity, err := s.relayStore().RelayAPIKey(r.Context(), req.APIKey)
	if err != nil {
		if errors.Is(err, billing.ErrRelayNotFound) {
			writeRelayError(w, http.StatusUnauthorized, "unauthorized", "invalid api key")
			return
		}
		s.fail(w, err)
		return
	}
	balance, err := s.relayStore().RelayBalance(r.Context(), identity.OrgID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// subject_ref stays the org id (the billing tenant) for backward
	// compatibility. user_id is the issuing console user and is "" for legacy
	// tokens issued before user binding; the relay must fall back to
	// org-scoped behavior in that case.
	writeJSON(w, http.StatusOK, map[string]any{
		"subject_ref":   identity.OrgID,
		"user_id":       identity.UserID,
		"balance_minor": balance * RelayMilliPerCredit,
		"currency":      billing.RelayCurrency,
		"as_of":         time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) relayReserve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RequestID      string `json:"request_id"`
		IdempotencyKey string `json:"idempotency_key"`
		SubjectRef     string `json:"subject_ref"`
		AmountMinor    int64  `json:"amount_minor"`
		Currency       string `json:"currency"`
		TTLSeconds     int64  `json:"ttl_seconds"`
		Reason         string `json:"reason"`
		PriceVersion   string `json:"price_version"`
	}
	if !decodeRelayBody(w, r, &req) {
		return
	}
	if req.RequestID == "" || req.SubjectRef == "" {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "request_id and subject_ref are required")
		return
	}
	if !isUUID(req.SubjectRef) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "subject_ref must be a UUID")
		return
	}
	if req.Currency != billing.RelayCurrency {
		writeRelayError(w, http.StatusBadRequest, "currency_mismatch", "unsupported currency")
		return
	}
	reservation, err := s.relayStore().RelayReserveWithDetails(r.Context(), billing.RelayReserveInput{
		OrgID:        req.SubjectRef,
		RequestID:    req.RequestID,
		Amount:       relayCreditsFromMinor(req.AmountMinor),
		Currency:     req.Currency,
		TTL:          time.Duration(req.TTLSeconds) * time.Second,
		PriceVersion: req.PriceVersion,
	})
	if err != nil {
		relayStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reservation_id":   reservation.ID,
		"amount_minor":     reservation.Amount,
		"amount_credits":   reservation.Amount,
		"currency":         billing.RelayCurrency,
		"expires_at":       reservation.ExpiresAt.UTC().Format(time.RFC3339),
		"price_version":    reservation.PriceVersion,
		"settings_version": reservation.SettingsVersion,
	})
}

func (s *Server) relaySettle(w http.ResponseWriter, r *http.Request) {
	reservationID := strings.TrimSpace(r.PathValue("reservationID"))
	if !isUUID(reservationID) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "reservation_id must be a UUID")
		return
	}
	var req struct {
		SettlementID   string               `json:"settlement_id"`
		IdempotencyKey string               `json:"idempotency_key"`
		AmountMinor    *int64               `json:"amount_minor"`
		Currency       string               `json:"currency"`
		UsageStatus    string               `json:"usage_status"`
		ToolCharges    []billing.ToolCharge `json:"tool_charges"`
		Compression    *billing.Compression `json:"compression"`
		Usage          json.RawMessage      `json:"usage"`
		UsageStats     *billing.UsageEvent  `json:"usage_stats"`
		// Settle V2 fields.
		Model        string                    `json:"model"`
		UpstreamID   string                    `json:"upstream_id"`
		StartedAt    string                    `json:"started_at"`
		PriceVersion string                    `json:"price_version"`
		Basis        *billing.CompressionBasis `json:"basis"`
	}
	if !decodeRelayBody(w, r, &req) {
		return
	}
	if req.IdempotencyKey == "" {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "idempotency_key is required")
		return
	}
	if req.Currency != "" && req.Currency != billing.RelayCurrency {
		writeRelayError(w, http.StatusBadRequest, "currency_mismatch", "unsupported currency")
		return
	}
	// Usage accepts both shapes during migration: the V2 token object
	// ({input,output,cache_read,cache_write}) and the old usage-stats object
	// ({model,requests,input_tokens,...,upstream_cost_minor}). The v2 shape is
	// only present under `usage` when token keys are used; a stats object keeps
	// its historical meaning.
	tokenUsage, stats, err := parseRelayUsage(req.Usage)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_usage", err.Error())
		return
	}
	if stats == nil {
		stats = req.UsageStats
	} else if req.UsageStats != nil {
		// Both were sent (new relay): the stats object is the reporting detail,
		// the token object is the pricing input.
		stats = req.UsageStats
	}
	// Validate the complete breakdown before touching the store: a malformed
	// entry must not book a settlement (or partially persist tool charges).
	charges, err := billing.NormalizeToolCharges(req.ToolCharges)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_tool_charges", err.Error())
		return
	}
	// Compression is optional. A malformed object is rejected before the store
	// for the same reason; absent or all-zero savings produce no row.
	compression, err := billing.NormalizeCompression(req.Compression)
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_compression", err.Error())
		return
	}
	var startedAt time.Time
	if strings.TrimSpace(req.StartedAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(req.StartedAt))
		if err != nil {
			writeRelayError(w, http.StatusBadRequest, "invalid_started_at", "started_at must be RFC3339")
			return
		}
		startedAt = parsed
	}
	isV2 := strings.TrimSpace(req.Model) != "" ||
		strings.TrimSpace(req.PriceVersion) != "" ||
		strings.TrimSpace(req.StartedAt) != "" ||
		req.Basis != nil ||
		tokenUsage != nil
	if !isV2 {
		// Legacy settlement: the relay computes the amount and the console
		// books it. usage_stats keeps the reporting row for pre-v2 clients.
		usageEvent, err := billing.NormalizeUsage(stats)
		if err != nil {
			writeRelayError(w, http.StatusBadRequest, "invalid_usage", err.Error())
			return
		}
		var amount int64
		if req.AmountMinor != nil {
			amount = relayCreditsFromMinor(*req.AmountMinor)
		}
		op, err := s.relayStore().RelaySettleWithDetails(r.Context(), reservationID, req.SettlementID, req.IdempotencyKey, amount, charges, compression, usageEvent)
		if err != nil {
			relayStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, relayOperationBody(op))
		return
	}
	if tokenUsage == nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_usage", "usage is required for a v2 settlement")
		return
	}
	op, err := s.relayStore().RelaySettleV2(r.Context(), billing.RelaySettleInput{
		ReservationID:  reservationID,
		SettlementID:   req.SettlementID,
		IdempotencyKey: req.IdempotencyKey,
		Model:          req.Model,
		UpstreamID:     req.UpstreamID,
		StartedAt:      startedAt,
		PriceVersion:   req.PriceVersion,
		Usage:          tokenUsage,
		Basis:          req.Basis,
		ToolCharges:    charges,
		AmountMinor:    req.AmountMinor,
		Stats:          stats,
	})
	if err != nil {
		relayStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, relayOperationBody(op))
}

// parseRelayUsage classifies the optional `usage` object. It returns exactly
// one of a V2 token usage or a legacy usage-stats object; sending both key
// families in one object is accepted as a V2 token usage plus stats.
func parseRelayUsage(raw json.RawMessage) (*billing.TokenUsage, *billing.UsageEvent, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, errors.New("usage must be a JSON object")
	}
	tokenKeys := []string{"input", "output", "cache_read", "cache_write"}
	statsKeys := []string{"model", "requests", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "session_id"}
	hasToken, hasStats := false, false
	for _, key := range tokenKeys {
		if _, ok := fields[key]; ok {
			hasToken = true
		}
	}
	for _, key := range statsKeys {
		if _, ok := fields[key]; ok {
			hasStats = true
		}
	}
	var tokens *billing.TokenUsage
	if hasToken {
		var usage billing.TokenUsage
		if err := json.Unmarshal(raw, &usage); err != nil {
			return nil, nil, errors.New("usage token counters must be integers")
		}
		if err := usage.Validate(); err != nil {
			return nil, nil, err
		}
		tokens = &usage
	}
	var stats *billing.UsageEvent
	if hasStats {
		var event billing.UsageEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, nil, errors.New("usage stats counters must be integers")
		}
		stats = &event
	}
	if !hasToken && !hasStats {
		return nil, nil, errors.New("usage must carry token counters or usage stats")
	}
	return tokens, stats, nil
}

func (s *Server) relayRelease(w http.ResponseWriter, r *http.Request) {
	reservationID := strings.TrimSpace(r.PathValue("reservationID"))
	if !isUUID(reservationID) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "reservation_id must be a UUID")
		return
	}
	op, err := s.relayStore().RelayRelease(r.Context(), reservationID)
	if err != nil {
		relayStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, relayOperationBody(op))
}

func (s *Server) relayOperation(w http.ResponseWriter, r *http.Request) {
	op, err := s.relayStore().RelayOperation(r.Context(), r.PathValue("idempotencyKey"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !op.Found {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	writeJSON(w, http.StatusOK, relayOperationBody(op))
}

// relayReservationToolCharges exposes a reservation's persisted breakdown
// directly, for reporting without replaying the settle idempotency key.
func (s *Server) relayReservationToolCharges(w http.ResponseWriter, r *http.Request) {
	reservationID := strings.TrimSpace(r.PathValue("reservationID"))
	if !isUUID(reservationID) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "reservation_id must be a UUID")
		return
	}
	charges, err := s.relayStore().RelayToolCharges(r.Context(), reservationID)
	if err != nil {
		relayStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, relayToolChargesBody(reservationID, charges))
}

func relayToolChargesBody(reservationID string, charges []billing.ToolCharge) map[string]any {
	if charges == nil {
		charges = []billing.ToolCharge{}
	}
	totals := billing.SumToolCharges(charges)
	return map[string]any{
		"reservation_id":                   reservationID,
		"tool_charges":                     charges,
		"tool_charges_total":               totals,
		"tool_charges_amount_minor":        totals.AmountMinor,
		"tool_charges_provider_cost_micro": totals.ProviderCostMicro,
	}
}

func relayOperationBody(op billing.RelayOperation) map[string]any {
	charges := op.ToolCharges
	if charges == nil {
		charges = []billing.ToolCharge{}
	}
	totals := billing.SumToolCharges(charges)
	amountMinor := op.AmountMinor
	if amountMinor == 0 {
		amountMinor = op.Amount * RelayMilliPerCredit
	}
	debts := op.Debts
	if debts == nil {
		debts = []billing.Debt{}
	}
	body := map[string]any{
		"found":                            op.Found,
		"operation":                        op.Operation,
		"status":                           op.Status,
		"reservation_id":                   op.ReservationID,
		"amount_minor":                     amountMinor,
		"amount_credits":                   op.Amount,
		"currency":                         billing.RelayCurrency,
		"error_code":                       op.ErrorCode,
		"policy_version":                   op.PolicyVersion,
		"settings_version":                 op.SettingsVersion,
		"upstream_id":                      op.UpstreamID,
		"tool_charges":                     charges,
		"tool_charges_total":               totals,
		"tool_charges_amount_minor":        totals.AmountMinor,
		"tool_charges_provider_cost_micro": totals.ProviderCostMicro,
		"compression":                      op.Compression,
		"usage":                            op.Usage,
		"debts":                            debts,
	}
	if op.Pricing != nil && op.Pricing.Compression != nil {
		compression := op.Pricing.Compression
		body["compression"] = map[string]any{
			"saved_tokens":      compression.SavedTokens,
			"user_share_tokens": compression.UserShareTokens,
			"net_gain_minor":    compression.NetGainMinor,
		}
	}
	return body
}

func decodeRelayBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRelayError(w, http.StatusBadRequest, "bad_request", "could not read body")
		return false
	}
	if err := json.Unmarshal(body, dst); err != nil {
		writeRelayError(w, http.StatusBadRequest, "bad_request", "invalid json")
		return false
	}
	return true
}

func relayStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrInsufficientCredits):
		writeRelayError(w, http.StatusPaymentRequired, "insufficient_balance", "insufficient balance")
	case errors.Is(err, billing.ErrRelayConflict):
		writeRelayError(w, http.StatusConflict, "conflict", "idempotency conflict")
	case errors.Is(err, billing.ErrRelayNotFound):
		writeRelayError(w, http.StatusNotFound, "not_found", "reservation not found")
	case errors.Is(err, billing.ErrRelayReleased):
		writeRelayError(w, http.StatusConflict, "lease_expired", "reservation already released")
	case errors.Is(err, billing.ErrPriceVersionMismatch):
		writeRelayError(w, http.StatusConflict, "price_version_mismatch", "price version does not match the reservation")
	case errors.Is(err, billing.ErrAmountMismatch):
		writeRelayError(w, http.StatusConflict, "amount_mismatch", "recomputed amount does not match the reported amount")
	case errors.Is(err, billing.ErrUnknownModel), errors.Is(err, billing.ErrPriceTierNotFound):
		writeRelayError(w, http.StatusUnprocessableEntity, "unknown_model", "model is not in the price book")
	case errors.Is(err, billing.ErrInvalidPricing):
		writeRelayError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, billing.ErrInvalidToolCharges):
		writeRelayError(w, http.StatusBadRequest, "invalid_tool_charges", err.Error())
	case errors.Is(err, billing.ErrInvalidCompression):
		writeRelayError(w, http.StatusBadRequest, "invalid_compression", err.Error())
	case errors.Is(err, billing.ErrInvalidUsage):
		writeRelayError(w, http.StatusBadRequest, "invalid_usage", err.Error())
	case isUUIDParseError(err):
		writeRelayError(w, http.StatusBadRequest, "invalid_request", "reservation_id must be a UUID")
	default:
		writeRelayError(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func writeRelayError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// withRelayService authenticates the relay service itself. Unlike the dslpha
// compatibility middleware it carries no user identity: the relay presents a
// downstream API key, not a user session, and every relay endpoint resolves
// its subject from that key or the reservation.
func (s *Server) withRelayService(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeRelayError(w, http.StatusBadRequest, "bad_request", "could not read body")
			return
		}
		service := "relay"
		verified := s.verifyService(service, r, body)
		if !verified {
			legacy := r.Header.Get("X-Service-Token")
			if s.verifyLegacyServiceToken(service, legacy) {
				verified = true
			}
		}
		if !verified {
			writeRelayError(w, http.StatusUnauthorized, "unauthorized", "bad service signature")
			return
		}
		if !serviceAllows(service, r.Method, r.URL.Path) {
			writeRelayError(w, http.StatusForbidden, "forbidden", "service scope does not allow this endpoint")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next(w, r)
	}
}
