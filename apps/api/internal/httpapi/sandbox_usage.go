package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
)

// sandboxUsageRecorder is the billing slice behind
// POST /internal/v1/usage/sandbox. It is an interface so handler tests can
// exercise validation, auth and idempotency without Postgres.
type sandboxUsageRecorder interface {
	RecordSandboxUsage(ctx context.Context, in billing.SandboxUsageInput) (billing.SandboxChargeResult, error)
}

func (s *Server) sandboxUsageStore() sandboxUsageRecorder {
	if s.sandboxUsage != nil {
		return s.sandboxUsage
	}
	if s.store == nil {
		return nil
	}
	return s.store
}

// recordSandboxUsage serves POST /internal/v1/usage/sandbox. The service
// signature and scope are checked by withService before this handler runs. A
// fresh report answers 201; a replay of the same idempotency key answers 200
// with the original row and no second debit.
func (s *Server) recordSandboxUsage(w http.ResponseWriter, r *http.Request) {
	store := s.sandboxUsageStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "internal_error", "sandbox usage store is not configured")
		return
	}
	var in billing.SandboxUsageInput
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_usage", "invalid sandbox usage body")
		return
	}
	result, err := store.RecordSandboxUsage(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"id":              result.Usage.ID,
		"charged_credits": result.ChargedCredits,
		"balance_credits": result.BalanceCredits,
		"credits_minor":   result.Usage.CreditsMinor,
		"gpu_unpriced":    result.Usage.GPUUnpriced,
		"replayed":        result.Replayed,
	})
}
