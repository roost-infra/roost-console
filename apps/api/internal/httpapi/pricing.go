package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/JingxuanC/roost-console/apps/api/internal/billing"
)

// The public price book is the single price source for the website, the relay
// config generator and product clients. Both endpoints serve the same content
// hash: the public projection omits cost, the internal projection (service
// auth) carries it, and the hash is computed over the public projection so CI
// can compare page hash = relay config hash = API hash.
const pricingBookCacheTTL = 60 * time.Second

// priceBookStore is the pricing slice of the billing store. It is an interface
// so handler tests can exercise ETag/caching without Postgres.
type priceBookStore interface {
	PriceBook(ctx context.Context, at time.Time) (billing.PriceBook, error)
}

func (s *Server) pricingStore() priceBookStore {
	if s.pricing != nil {
		return s.pricing
	}
	if s.store == nil {
		return nil
	}
	return s.store
}

// publicPricingBook serves GET /v1/pricing/book: no auth, no cost, cached for
// 60 seconds, ETag = content hash.
func (s *Server) publicPricingBook(w http.ResponseWriter, r *http.Request) {
	s.servePricingBook(w, r, false)
}

// internalPricingBook serves GET /internal/v1/pricing/book for service callers
// (relay config generation, margin accounting): same book plus per-component
// cost and the settings version.
func (s *Server) internalPricingBook(w http.ResponseWriter, r *http.Request) {
	s.servePricingBook(w, r, true)
}

func (s *Server) servePricingBook(w http.ResponseWriter, r *http.Request, internal bool) {
	store := s.pricingStore()
	if store == nil {
		writeError(w, http.StatusServiceUnavailable, "internal_error", "pricing store is not configured")
		return
	}
	query := r.URL.Query()
	if currency := strings.ToUpper(strings.TrimSpace(query.Get("currency"))); currency != "" && currency != billing.RelayCurrency {
		writeError(w, http.StatusBadRequest, "currency_mismatch", "unsupported currency")
		return
	}
	atRaw := strings.TrimSpace(query.Get("at"))
	var at time.Time
	if atRaw != "" {
		parsed, err := time.Parse(time.RFC3339, atRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_at", "at must be an RFC3339 timestamp")
			return
		}
		at = parsed
	}
	book, err := s.priceBook(r.Context(), at, atRaw != "")
	if err != nil {
		if errors.Is(err, billing.ErrPriceBookNotFound) {
			writeError(w, http.StatusNotFound, "price_book_not_found", "no price book is effective at the requested time")
			return
		}
		s.fail(w, err)
		return
	}
	etag := `"` + book.Hash + `"`
	w.Header().Set("ETag", etag)
	if internal {
		w.Header().Set("Cache-Control", "private, max-age=60")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=60")
	}
	if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match != "" && (match == etag || match == book.Hash) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if internal {
		writeJSON(w, http.StatusOK, book.InternalView())
		return
	}
	writeJSON(w, http.StatusOK, book.PublicView())
}

// priceBook caches the current book for 60s. An explicit `at` (historical
// lookup) bypasses the cache so a backfill can read any immutable version.
func (s *Server) priceBook(ctx context.Context, at time.Time, explicit bool) (billing.PriceBook, error) {
	store := s.pricingStore()
	if explicit {
		return store.PriceBook(ctx, at)
	}
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	if s.pricingCacheValid && time.Since(s.pricingCachedAt) < pricingBookCacheTTL {
		return s.pricingCached, nil
	}
	book, err := store.PriceBook(ctx, time.Time{})
	if err != nil {
		return billing.PriceBook{}, err
	}
	s.pricingCached = book
	s.pricingCachedAt = time.Now()
	s.pricingCacheValid = true
	return book, nil
}
