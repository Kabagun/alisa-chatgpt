package app

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"smart-home-plugin/internal/oauth"
	"smart-home-plugin/internal/store"
)

// CleanupAuthorization is a temporary, exact-client exception for installing a
// disposable plugin before deletion. The resulting identity has no provider.
// Deployment-only bindings must never be hardcoded in the source package.
type CleanupAuthorization struct {
	ClientID    string    `json:"client_id"`
	RedirectURI string    `json:"redirect_uri"`
	ExpiresAt   time.Time `json:"expires_at"`
	// AutoActivate skips the synthetic form only for this exact, live binding.
	AutoActivate bool `json:"auto_activate"`
}

const cleanupProviderID = "plugin-cleanup"

var cleanupClientID = regexp.MustCompile(`^client_[a-f0-9]{48}$`)

func prepareCleanupAuthorization(c *CleanupAuthorization, now time.Time) (*CleanupAuthorization, error) {
	if c == nil {
		return nil, nil
	}
	u, err := url.Parse(c.RedirectURI)
	if !cleanupClientID.MatchString(c.ClientID) || err != nil || !oauth.RedirectValid(c.RedirectURI) || u.Scheme != "https" || u.RawQuery != "" || c.ExpiresAt.IsZero() || c.ExpiresAt.After(now.Add(2*time.Hour)) {
		return nil, errors.New("invalid_cleanup_authorization")
	}
	copy := *c
	return &copy, nil
}

func (h *Handler) cleanupAuthorizationAllowed(f store.Flow) bool {
	c := h.cleanupAuthorization
	return c != nil && h.now().Before(c.ExpiresAt) && f.Client == c.ClientID && f.Redirect == c.RedirectURI
}

func (h *Handler) cleanupAutoActivationAllowed(f store.Flow) bool {
	return h.cleanupAuthorizationAllowed(f) && h.cleanupAuthorization.AutoActivate
}

// This path grants only an inert cleanup identity. The freshly created flow is
// still consumed atomically; normal code exchange retains every PKCE binding.
func (h *Handler) authorizeCleanupAutomatically(w http.ResponseWriter, r *http.Request, x oauth.Started) {
	f, err := h.oauth.TakeFlow(x.ID, x.Cookie, x.CSRF)
	if err != nil || !h.cleanupAutoActivationAllowed(f) {
		h.authDenied(w, "cleanup_not_allowed", false)
		return
	}
	select {
	case h.authSlots <- struct{}{}:
		defer func() { <-h.authSlots }()
	default:
		h.failure(w, 429, "temporarily_unavailable")
		return
	}
	id, _, err := h.store.NewAccount()
	if err != nil {
		h.failure(w, 503, "temporarily_unavailable")
		return
	}
	linked := false
	defer func() {
		if !linked {
			_ = h.store.RemoveUnlinkedAccount(id)
		}
	}()
	code, err := h.oauth.Complete(f, id, cleanupProviderID)
	if err != nil {
		h.failure(w, 503, "temporarily_unavailable")
		return
	}
	linked = true
	redirectAuthorization(w, r, f, code)
}
