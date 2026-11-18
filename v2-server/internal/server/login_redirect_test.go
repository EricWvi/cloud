package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/login"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// fakeProvider is a redirect login method whose sign-in page the test plays itself.
type fakeProvider struct {
	identity  login.Identity
	err       error
	redirects []string
	codes     []string
}

func (p *fakeProvider) AuthorizeURL(state, redirectURI string) string {
	return "https://idaas.example/authorize?" + url.Values{"state": {state}, "redirect_uri": {redirectURI}}.Encode()
}

func (p *fakeProvider) Exchange(_ context.Context, code, redirectURI string) (login.Identity, error) {
	p.codes = append(p.codes, code)
	p.redirects = append(p.redirects, redirectURI)
	return p.identity, p.err
}

type redirectFixture struct {
	t        *testing.T
	srv      *httptest.Server
	provider *fakeProvider
	store    *store.Store
	sql      *pgx.Conn
	origin   string
}

func newRedirectFixture(t *testing.T, public string) *redirectFixture {
	t.Helper()
	s, sql := openStoreSQL(t)
	storetest.SignIn(t, s, "demo", "演示用户", store.RoleOwner)
	p := &fakeProvider{identity: login.Identity{Source: "huawei-corp", Subject: "uuid~abc", DisplayName: "张三"}}
	u, _ := url.Parse(public)
	srv := httptest.NewServer(server.New(s, server.Options{
		SessionTTL: time.Hour, PublicURL: u,
		LoginProviders: []string{"huawei-idaas"},
		LoginRedirects: map[string]login.Provider{"huawei-idaas": p},
	}))
	t.Cleanup(srv.Close)
	return &redirectFixture{t: t, srv: srv, provider: p, store: s, sql: sql, origin: u.Scheme + "://" + u.Host}
}

// start posts the login and returns the provider's state and the attempt cookie.
func (f *redirectFixture) start(returnTo string) (state string, attempt *http.Cookie) {
	f.t.Helper()
	res, body := rawCall(f.t, "POST", f.srv.URL+"/auth/login", f.origin, nil, map[string]string{"provider": "huawei-idaas", "returnTo": returnTo})
	if res.StatusCode != 200 {
		f.t.Fatalf("start = %d %v", res.StatusCode, body)
	}
	target, _ := url.Parse(body["authorizationUrl"].(string))
	for _, c := range res.Cookies() {
		if c.Name == server.LoginAttemptCookie || c.Name == server.SecureLoginAttemptCookie {
			attempt = c
		}
	}
	if attempt == nil {
		f.t.Fatal("no attempt cookie")
	}
	if strings.Contains(target.String(), url.QueryEscape(returnTo)) && returnTo != "/" {
		f.t.Fatalf("returnTo leaked into the authorize address: %s", target)
	}
	return target.Query().Get("state"), attempt
}

// back plays the provider's redirect to the callback.
func (f *redirectFixture) back(query url.Values, attempt *http.Cookie) *http.Response {
	f.t.Helper()
	res, _ := rawCall(f.t, "GET", f.srv.URL+"/auth/callback/huawei-idaas?"+query.Encode(), "", attempt, nil)
	return res
}

func TestRedirectLoginSignsInAndCreatesTheAccount(t *testing.T) {
	f := newRedirectFixture(t, "https://ora.example.com/ora")
	state, attempt := f.start("/workflows?x=1")
	// At an https address the attempt cookie is host-only and __Host- prefixed, so a sibling
	// subdomain cannot plant one.
	if attempt.Name != server.SecureLoginAttemptCookie || attempt.Path != "/" || attempt.Domain != "" || !attempt.HttpOnly || !attempt.Secure || attempt.MaxAge != 600 {
		t.Fatalf("attempt cookie = %+v", attempt)
	}
	res := f.back(url.Values{"code": {"c1"}, "state": {state}}, attempt)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/workflows?x=1" {
		t.Fatalf("callback = %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if fmt.Sprint(f.provider.redirects) != "[https://ora.example.com/ora/auth/callback/huawei-idaas]" || fmt.Sprint(f.provider.codes) != "[c1]" {
		t.Fatalf("exchange got %v %v", f.provider.codes, f.provider.redirects)
	}
	session := sessionCookie(t, res)
	if session.Name != server.SecureSessionCookie {
		t.Fatalf("session cookie = %+v", session)
	}
	res, me := rawCall(t, "GET", f.srv.URL+"/api/v1/me", "", session, nil)
	if res.StatusCode != 200 || me["displayName"] != "张三" {
		t.Fatalf("me = %d %v", res.StatusCode, me)
	}
	// Signing in does not join a workspace.
	if _, tenants := rawCall(t, "GET", f.srv.URL+"/api/v1/me/tenants", "", session, nil); len(tenants["items"].([]any)) != 0 {
		t.Fatalf("tenants = %v", tenants)
	}
	// The second login finds the same account and refreshes its name.
	f.provider.identity.DisplayName = "张三丰"
	state, attempt = f.start("/")
	res = f.back(url.Values{"code": {"c2"}, "state": {state}}, attempt)
	_, again := rawCall(t, "GET", f.srv.URL+"/api/v1/me", "", sessionCookie(t, res), nil)
	if again["id"] != me["id"] || again["displayName"] != "张三丰" {
		t.Fatalf("second login = %v, first %v", again, me)
	}
}

func TestRedirectLoginRefusesBadAttempts(t *testing.T) {
	f := newRedirectFixture(t, "https://ora.example.com")
	expectFailure := func(name string, res *http.Response, category string) {
		t.Helper()
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/?login_error="+category {
			t.Fatalf("%s: %d %s", name, res.StatusCode, res.Header.Get("Location"))
		}
		for _, c := range res.Cookies() {
			if c.Name == server.SecureSessionCookie && c.MaxAge >= 0 {
				t.Fatalf("%s: signed in", name)
			}
		}
	}
	state, attempt := f.start("/")
	expectFailure("wrong state", f.back(url.Values{"code": {"c"}, "state": {"forged"}}, attempt), "failed")
	// The attempt is gone after one callback, even a failed one.
	expectFailure("reused attempt", f.back(url.Values{"code": {"c"}, "state": {state}}, attempt), "failed")
	state, _ = f.start("/")
	expectFailure("no attempt cookie", f.back(url.Values{"code": {"c"}, "state": {state}}, nil), "failed")
	state, attempt = f.start("/")
	expectFailure("provider error", f.back(url.Values{"error": {"access_denied"}, "state": {state}}, attempt), "failed")
	if len(f.provider.codes) != 0 {
		t.Fatalf("exchanged after a refused attempt: %v", f.provider.codes)
	}

	f.provider.err = errors.New("refused")
	state, attempt = f.start("/")
	expectFailure("exchange refused", f.back(url.Values{"code": {"c"}, "state": {state}}, attempt), "failed")
	f.provider.err = fmt.Errorf("%w: timeout", login.ErrUnavailable)
	state, attempt = f.start("/")
	expectFailure("provider down", f.back(url.Values{"code": {"c"}, "state": {state}}, attempt), "unavailable")
}

func TestRedirectLoginRefusesADisabledAccount(t *testing.T) {
	f := newRedirectFixture(t, "https://ora.example.com")
	state, attempt := f.start("/")
	f.back(url.Values{"code": {"c"}, "state": {state}}, attempt)
	if _, e := f.sql.Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE source = 'huawei-corp' AND subject = 'uuid~abc'`); e != nil {
		t.Fatal(e)
	}
	state, attempt = f.start("/")
	if res := f.back(url.Values{"code": {"c"}, "state": {state}}, attempt); res.Header.Get("Location") != "/?login_error=disabled" {
		t.Fatalf("disabled login = %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestRedirectLoginOnlyForOfferedMethods(t *testing.T) {
	f := newRedirectFixture(t, "https://ora.example.com")
	if res, body := rawCall(t, "POST", f.srv.URL+"/auth/login", f.origin, nil, map[string]string{"provider": "github"}); res.StatusCode != 400 || body["code"] != "unknown_provider" {
		t.Fatalf("unknown method = %d %v", res.StatusCode, body)
	}
	if res, _ := rawCall(t, "GET", f.srv.URL+"/auth/callback/github?code=c&state=s", "", nil, nil); res.StatusCode != 404 {
		t.Fatalf("callback of an unknown method = %d", res.StatusCode)
	}
	// Starting a redirect login is a write too: another site's page cannot start one.
	if res, _ := rawCall(t, "POST", f.srv.URL+"/auth/login", "https://evil.example", nil, map[string]string{"provider": "huawei-idaas"}); res.StatusCode != 403 {
		t.Fatalf("cross-site start = %d", res.StatusCode)
	}
}

// Over plain http (a local stand-in) the attempt cookie keeps its plain name, scoped to the callback.
func TestRedirectLoginAttemptCookieOverHTTP(t *testing.T) {
	f := newRedirectFixture(t, "http://127.0.0.1:8090/ora")
	state, attempt := f.start("/")
	if attempt.Name != server.LoginAttemptCookie || attempt.Path != "/ora/auth/callback/huawei-idaas" || attempt.Secure {
		t.Fatalf("attempt cookie = %+v", attempt)
	}
	if res := f.back(url.Values{"code": {"c"}, "state": {state}}, attempt); res.Header.Get("Location") != "/" {
		t.Fatalf("callback = %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// A plain-named cookie is not read at an https address, so a planted one is ignored there.
}

// returnTo goes into a server-side Location, so it must stay on this origin even with control
// characters a browser would strip.
func TestRedirectLoginKeepsReturnToOnThisOrigin(t *testing.T) {
	f := newRedirectFixture(t, "https://ora.example.com")
	for _, target := range []string{"/\t/evil.example", "/\n/evil.example", "//evil.example", "https://evil.example/"} {
		state, attempt := f.start(target)
		if res := f.back(url.Values{"code": {"c"}, "state": {state}}, attempt); res.Header.Get("Location") != "/" {
			t.Fatalf("returnTo %q redirected to %q", target, res.Header.Get("Location"))
		}
	}
}
