package httpapi

import (
	"context"
	"net/http"
)

// settingsProvider is the billing-settings slice behind
// GET /internal/v1/billing/settings. Products (dslpha, market) read the flat
// a.b key map to pick up fx/markup/share values instead of keeping local env
// copies.
type settingsProvider interface {
	SettingsFlat(ctx context.Context) (map[string]any, error)
}

func (s *Server) settingsSource() settingsProvider {
	if s.settings != nil {
		return s.settings
	}
	if s.store == nil {
		return nil
	}
	return s.store
}

// internalBillingSettings serves the effective billing settings to trusted
// services. Values are JSON scalars (numbers or strings); `settings_version`
// identifies the immutable snapshot the values came from.
func (s *Server) internalBillingSettings(w http.ResponseWriter, r *http.Request) {
	store := s.settingsSource()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "internal_error", "settings store is not configured")
		return
	}
	settings, err := store.SettingsFlat(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if settings == nil {
		settings = map[string]any{}
	}
	writeJSON(w, http.StatusOK, settings)
}
