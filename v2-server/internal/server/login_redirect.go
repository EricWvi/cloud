package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/login"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// LoginAttemptCookie carries one redirect login's key from POST /auth/login to the callback. It is
// sent only to the callback path and lasts as long as the attempt.
const LoginAttemptCookie = "ora_login"

const (
	loginAttemptTTL  = 10 * time.Minute
	maxLoginAttempts = 10000
	// loginExchangeTimeout bounds the provider calls a callback makes.
	loginExchangeTimeout = 20 * time.Second
)

// loginAttempt is one redirect login in flight. It lives in memory only: v2-server is one process,
// so the callback always reaches the process that started the attempt, and a restart only makes
// people click the login button again (A6 D2).
type loginAttempt struct {
	provider, state, returnTo string
	expires                   time.Time
}

type loginAttempts struct {
	mu    sync.Mutex
	items map[string]loginAttempt
}

func newLoginAttempts() *loginAttempts { return &loginAttempts{items: map[string]loginAttempt{}} }

// add stores an attempt under a new random key, or reports false when too many are in flight.
func (a *loginAttempts) add(at loginAttempt, now time.Time) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.items) >= maxLoginAttempts {
		for k, v := range a.items {
			if !now.Before(v.expires) {
				delete(a.items, k)
			}
		}
		if len(a.items) >= maxLoginAttempts {
			return "", false
		}
	}
	key := randomToken()
	a.items[key] = at
	return key, true
}

// take removes and returns the attempt, so each one is used at most once.
func (a *loginAttempts) take(key string, now time.Time) (loginAttempt, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.items[key]
	delete(a.items, key)
	return at, ok && now.Before(at.expires)
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// publicPath is the path prefix of V2_PUBLIC_URL, "" at the root.
func (h *handler) publicPath() string {
	if h.opts.PublicURL == nil {
		return ""
	}
	return h.opts.PublicURL.Path
}

func (h *handler) callbackPath(provider string) string {
	return h.publicPath() + "/auth/callback/" + provider
}

// callbackURL is the redirect URI a provider is registered with. It comes from V2_PUBLIC_URL only,
// never from request headers.
func (h *handler) callbackURL(provider string) string {
	p := h.opts.PublicURL
	return p.Scheme + "://" + p.Host + h.callbackPath(provider)
}

// redirectProvider is the offered redirect login method by that name.
func (h *handler) redirectProvider(name string) (login.Provider, bool) {
	p, ok := h.opts.LoginRedirects[name]
	if !ok || h.opts.PublicURL == nil {
		return nil, false
	}
	for _, offered := range h.loginProviders() {
		if offered == name {
			return p, true
		}
	}
	return nil, false
}

// startRedirectLogin answers POST /auth/login for a redirect method: it records the attempt, sets
// the attempt cookie for the callback path and returns the provider's sign-in address. returnTo
// stays here and never appears in that address.
func (h *handler) startRedirectLogin(w http.ResponseWriter, name string, p login.Provider, returnTo string) {
	state := randomToken()
	key, ok := h.attempts.add(loginAttempt{provider: name, state: state, returnTo: localPath(returnTo), expires: time.Now().Add(loginAttemptTTL)}, time.Now())
	if !ok {
		fault(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: LoginAttemptCookie, Value: key, Path: h.callbackPath(name), HttpOnly: true, Secure: h.secure(),
		SameSite: http.SameSiteLaxMode, MaxAge: int(loginAttemptTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]string{"authorizationUrl": p.AuthorizeURL(state, h.callbackURL(name))})
}

// callback is where a redirect provider sends the browser back. It always answers with a redirect:
// on success to the attempt's returnTo with a session, otherwise to the login page with a failure
// category (failed, unavailable or disabled) the page explains. The log records the step and the
// provider's error kind, never the code, the state or a token.
func (h *handler) callback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	p, ok := h.redirectProvider(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The attempt is single-use whatever happens next.
	http.SetCookie(w, &http.Cookie{Name: LoginAttemptCookie, Path: h.callbackPath(name), HttpOnly: true, Secure: h.secure(), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	fail := func(category, step string, err error) {
		slog.Warn("login callback", "provider", name, "step", step, "error", err)
		http.Redirect(w, r, h.publicPath()+"/?login_error="+category, http.StatusSeeOther)
	}
	c, e := r.Cookie(LoginAttemptCookie)
	if e != nil {
		fail("failed", "attempt", errors.New("no login attempt cookie"))
		return
	}
	at, ok := h.attempts.take(c.Value, time.Now())
	q := r.URL.Query()
	if !ok || at.provider != name || subtle.ConstantTimeCompare([]byte(at.state), []byte(q.Get("state"))) != 1 {
		fail("failed", "state", errors.New("unknown, expired or mismatched login attempt"))
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		fail("failed", "authorize", errors.New("provider returned no code: "+url.QueryEscape(q.Get("error"))))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginExchangeTimeout)
	defer cancel()
	id, e := p.Exchange(ctx, q.Get("code"), h.callbackURL(name))
	if e != nil {
		category := "failed"
		if errors.Is(e, login.ErrUnavailable) {
			category = "unavailable"
		}
		fail(category, "exchange", e)
		return
	}
	u, e := h.store.SignInExternal(r.Context(), id.Source, id.Subject, id.DisplayName)
	if errors.Is(e, store.ErrAccountDisabled) {
		fail("disabled", "account", e)
		return
	}
	if e != nil {
		fail("unavailable", "account", e)
		return
	}
	token, e := h.store.StartSession(r.Context(), u.ID, h.opts.SessionTTL)
	if e != nil {
		fail("unavailable", "session", e)
		return
	}
	h.setSession(w, token, int(h.opts.SessionTTL/time.Second))
	http.Redirect(w, r, at.returnTo, http.StatusSeeOther)
}
