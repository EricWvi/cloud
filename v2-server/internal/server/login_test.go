package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// rawCall sends one request with explicit headers and cookies, bypassing the jar so tests can
// play a foreign page or a Secure cookie over the test server's plain http.
func rawCall(t *testing.T, method, target, origin string, cookie *http.Cookie, body any) (*http.Response, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, target, reader)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, e := http.DefaultTransport.RoundTrip(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res, out
}

// sessionCookie is the session cookie a login response set.
func sessionCookie(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == server.SessionCookie || c.Name == server.SecureSessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie in %v", res.Header["Set-Cookie"])
	return nil
}

func loginServer(t *testing.T, o server.Options) (*httptest.Server, *store.Store) {
	t.Helper()
	s := openStore(t)
	storetest.SignIn(t, s, "demo", "演示用户", store.RoleOwner)
	o.SessionTTL = time.Hour
	srv := httptest.NewServer(server.New(s, o))
	t.Cleanup(srv.Close)
	return srv, s
}

func TestProvidersFollowV2Login(t *testing.T) {
	for _, tc := range []struct {
		configured []string
		want       string
	}{
		{nil, "[email]"},
		{[]string{"email"}, "[email]"},
		// The server lists what main configured, in order; main rejects names this build lacks.
		{[]string{"huawei-idaas", "email"}, "[huawei-idaas email]"},
	} {
		srv, _ := loginServer(t, server.Options{LoginProviders: tc.configured})
		res, body := rawCall(t, "GET", srv.URL+"/auth/providers", "", nil, nil)
		if res.StatusCode != 200 || fmt.Sprint(body["providers"]) != tc.want || body["default"] != nil {
			t.Fatalf("providers for %v = %d %v", tc.configured, res.StatusCode, body)
		}
	}
}

// Without email in V2_LOGIN there is no way to an email session, even for a preset address.
func TestEmailLoginOffWhenNotConfigured(t *testing.T) {
	srv, _ := loginServer(t, server.Options{LoginProviders: []string{"huawei-idaas"}})
	for _, provider := range []string{"email", ""} {
		res, body := rawCall(t, "POST", srv.URL+"/auth/login", "", nil, map[string]string{"provider": provider, "email": demoEmail})
		if res.StatusCode != 400 || body["code"] != "unknown_provider" || len(res.Cookies()) != 0 {
			t.Fatalf("email login with provider %q = %d %v", provider, res.StatusCode, body)
		}
	}
}

func TestCrossOriginWritesAreRefused(t *testing.T) {
	srv, s := loginServer(t, server.Options{})
	_, tenant := storetest.Login(t, s, "demo")
	foreign := "https://evil.example"
	login := map[string]string{"provider": "email", "email": demoEmail}

	res, body := rawCall(t, "POST", srv.URL+"/auth/login", foreign, nil, login)
	if res.StatusCode != 403 || body["code"] != "origin_mismatch" || len(res.Cookies()) != 0 {
		t.Fatalf("foreign login = %d %v", res.StatusCode, body)
	}
	// The page's own origin (the request's Host, without V2_PUBLIC_URL) signs in.
	res, body = rawCall(t, "POST", srv.URL+"/auth/login", srv.URL, nil, login)
	if res.StatusCode != 200 {
		t.Fatalf("same-origin login = %d %v", res.StatusCode, body)
	}
	session := sessionCookie(t, res)

	// Reads stay open to any origin; writes carrying the cookie must come from this origin.
	if res, body := rawCall(t, "GET", srv.URL+"/api/v1/me", foreign, session, nil); res.StatusCode != 200 {
		t.Fatalf("foreign read = %d %v", res.StatusCode, body)
	}
	write := srv.URL + "/api/v2/tenants/" + tenant + "/skills"
	// Without V2_PUBLIC_URL only the host is compared: behind a TLS proxy the server cannot tell
	// which scheme the page used.
	for _, origin := range []string{foreign, "null", srv.URL + "/path", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		if res, body := rawCall(t, "POST", write, origin, session, map[string]string{}); res.StatusCode != 403 || body["code"] != "origin_mismatch" {
			t.Fatalf("write from %q = %d %v", origin, res.StatusCode, body)
		}
	}
	if res, body := rawCall(t, "POST", write, srv.URL, session, map[string]string{}); res.StatusCode == 403 || res.StatusCode == 401 {
		t.Fatalf("same-origin write = %d %v", res.StatusCode, body)
	}
	// A foreign page cannot end the session either.
	if res, _ := rawCall(t, "POST", srv.URL+"/auth/logout", foreign, session, nil); res.StatusCode != 403 {
		t.Fatalf("foreign logout = %d", res.StatusCode)
	}
	if res, _ := rawCall(t, "GET", srv.URL+"/api/v1/me", "", session, nil); res.StatusCode != 200 {
		t.Fatalf("session after foreign logout = %d", res.StatusCode)
	}
}

func TestHTTPSPublicURLUsesHostPrefixedSecureCookie(t *testing.T) {
	public, _ := url.Parse("https://ora.example.com")
	srv, _ := loginServer(t, server.Options{PublicURL: public})
	login := map[string]string{"provider": "email", "email": demoEmail}

	// With V2_PUBLIC_URL set, only that origin counts, not whatever Host the request names.
	if res, _ := rawCall(t, "POST", srv.URL+"/auth/login", srv.URL, nil, login); res.StatusCode != 403 {
		t.Fatalf("login from the raw host = %d", res.StatusCode)
	}
	res, body := rawCall(t, "POST", srv.URL+"/auth/login", "https://ora.example.com", nil, login)
	if res.StatusCode != 200 {
		t.Fatalf("login = %d %v", res.StatusCode, body)
	}
	c := sessionCookie(t, res)
	if c.Name != "__Host-ora_session" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatalf("session cookie = %+v", c)
	}
	if res, _ := rawCall(t, "GET", srv.URL+"/api/v1/me", "", c, nil); res.StatusCode != 200 {
		t.Fatalf("me with secure cookie = %d", res.StatusCode)
	}
	// The plain-http cookie name is not read at an https address.
	plain := &http.Cookie{Name: server.SessionCookie, Value: c.Value}
	if res, _ := rawCall(t, "GET", srv.URL+"/api/v1/me", "", plain, nil); res.StatusCode != 401 {
		t.Fatalf("me with plain cookie name = %d", res.StatusCode)
	}
	res, _ = rawCall(t, "POST", srv.URL+"/auth/logout", "https://ora.example.com", c, nil)
	if cleared := sessionCookie(t, res); res.StatusCode != 204 || cleared.Name != "__Host-ora_session" || cleared.MaxAge >= 0 || !cleared.Secure {
		t.Fatalf("logout = %d %+v", res.StatusCode, cleared)
	}
}

func TestPlainHTTPKeepsDevelopmentCookie(t *testing.T) {
	public, _ := url.Parse("http://127.0.0.1:8090")
	srv, _ := loginServer(t, server.Options{PublicURL: public})
	res, _ := rawCall(t, "POST", srv.URL+"/auth/login", "http://127.0.0.1:8090", nil, map[string]string{"provider": "email", "email": demoEmail})
	if c := sessionCookie(t, res); res.StatusCode != 200 || c.Name != "ora_session" || c.Secure {
		t.Fatalf("login = %d %+v", res.StatusCode, c)
	}
}
