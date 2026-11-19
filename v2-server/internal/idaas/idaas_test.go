package idaas_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/idaas"
	"github.com/wanglongan587/cloud/v2-server/internal/login"
)

const (
	secret   = "s3cret-value"
	code     = "code-123"
	access   = "access-xyz"
	callback = "https://ora.example.com/auth/callback/huawei-idaas"
)

// stub is a stand-in IDaaS: it records what each endpoint received and answers with the handler's
// response.
type stub struct {
	t        *testing.T
	token    func(w http.ResponseWriter)
	userinfo func(w http.ResponseWriter)
	seen     map[string]map[string]string
	types    map[string]string
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	fields := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		_ = json.Unmarshal(raw, &fields)
	} else {
		form, _ := url.ParseQuery(string(raw))
		for k := range form {
			fields[k] = form.Get(k)
		}
	}
	if r.URL.RawQuery != "" {
		s.t.Errorf("%s carries a query: %s", r.URL.Path, r.URL.RawQuery)
	}
	s.seen[r.URL.Path], s.types[r.URL.Path] = fields, r.Header.Get("Content-Type")
	switch {
	case strings.HasSuffix(r.URL.Path, "/accesstoken"), strings.HasSuffix(r.URL.Path, "/token"):
		s.token(w)
	case strings.HasSuffix(r.URL.Path, "/userinfo"):
		s.userinfo(w)
	default:
		http.NotFound(w, r)
	}
}

func reply(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func newStub(t *testing.T) (*stub, *httptest.Server) {
	s := &stub{t: t, seen: map[string]map[string]string{}, types: map[string]string{},
		token:    reply(`{"access_token":"` + access + `","token_type":"Bearer","expires_in":3600}`),
		userinfo: reply(`{"uuid":"uuid~abc  ","displayName":"cn=张三, en=Zhang San","tenantId":"t"}`),
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, srv
}

func client(t *testing.T, base string, api idaas.API) *idaas.Client {
	t.Helper()
	c, e := idaas.New(idaas.Options{BaseURL: base, ClientID: "ora-app", ClientSecret: secret, API: api, HTTP: idaas.NewHTTPClient(2 * time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	return c
}

func TestOfficialAPIExchange(t *testing.T) {
	s, srv := newStub(t)
	c := client(t, srv.URL, "")
	u, _ := url.Parse(c.AuthorizeURL("st", callback))
	q := u.Query()
	if u.Path != "/saaslogin1/oauth2/authorize" || q.Get("client_id") != "ora-app" || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != callback || q.Get("scope") != "base.profile" || q.Get("state") != "st" || q.Get("display") != "page" ||
		q.Has("code_challenge") || strings.Contains(u.String(), secret) {
		t.Fatalf("authorize = %s", u)
	}
	id, e := c.Exchange(context.Background(), code, callback)
	if e != nil || id != (login.Identity{Source: "huawei-corp", Subject: "uuid~abc", DisplayName: "张三"}) {
		t.Fatalf("identity = %+v %v", id, e)
	}
	token := s.seen["/saaslogin1/oauth2/accesstoken"]
	if s.types["/saaslogin1/oauth2/accesstoken"] != "application/json" || token["grant_type"] != "authorization_code" ||
		token["client_id"] != "ora-app" || token["client_secret"] != secret || token["code"] != code || token["redirect_uri"] != callback {
		t.Fatalf("token request = %v (%s)", token, s.types["/saaslogin1/oauth2/accesstoken"])
	}
	info := s.seen["/saaslogin1/oauth2/userinfo"]
	if info["access_token"] != access || info["client_id"] != "ora-app" || info["scope"] != "base.profile" {
		t.Fatalf("userinfo request = %v", info)
	}
}

func TestV1APIExchange(t *testing.T) {
	s, srv := newStub(t)
	c := client(t, srv.URL, idaas.APIOAuth2V1)
	u, _ := url.Parse(c.AuthorizeURL("st", callback))
	if u.Path != "/saaslogin1/oauth2/v1/authorize" || u.Query().Has("display") {
		t.Fatalf("authorize = %s", u)
	}
	if _, e := c.Exchange(context.Background(), code, callback); e != nil {
		t.Fatal(e)
	}
	if s.types["/saaslogin1/oauth2/v1/token"] != "application/x-www-form-urlencoded" || s.seen["/saaslogin1/oauth2/v1/token"]["client_secret"] != secret {
		t.Fatalf("v1 token request = %v", s.seen)
	}
	if info := s.seen["/saaslogin1/oauth2/v1/userinfo"]; info["access_token"] != access || info["client_id"] != "" {
		t.Fatalf("v1 userinfo request = %v", info)
	}
}

func TestExchangeFailuresAreClassified(t *testing.T) {
	slow := func(w http.ResponseWriter) { time.Sleep(3 * time.Second) }
	for name, tc := range map[string]struct {
		token, userinfo func(http.ResponseWriter)
		unavailable     bool
	}{
		"errorCode in a 200":        {token: reply(`{"errorCode":"E_10009","errorDesc":"code invalid ` + code + `"}`)},
		"system errorCode":          {token: reply(`{"errorCode":"E_30001"}`), unavailable: true},
		"oauth error":               {token: reply(`{"error":"invalid_grant"}`)},
		"no access token":           {token: reply(`{"token_type":"Bearer"}`)},
		"not bearer":                {token: reply(`{"access_token":"x","token_type":"mac"}`)},
		"rate limited":              {token: func(w http.ResponseWriter) { w.WriteHeader(429) }, unavailable: true},
		"server error":              {token: func(w http.ResponseWriter) { w.WriteHeader(502) }, unavailable: true},
		"timeout":                   {token: slow, unavailable: true},
		"not json":                  {token: reply(`<html>login</html>`)},
		"oversized":                 {token: reply(`{"access_token":"` + strings.Repeat("a", 70<<10) + `"}`)},
		"userinfo errorCode":        {userinfo: reply(`{"errorCode":"E_10001"}`)},
		"userinfo without uuid":     {userinfo: reply(`{"displayName":"cn=张三"}`)},
		"userinfo blank uuid":       {userinfo: reply(`{"uuid":"   "}`)},
		"userinfo system error":     {userinfo: func(w http.ResponseWriter) { w.WriteHeader(503) }, unavailable: true},
		"userinfo error with a 4xx": {userinfo: func(w http.ResponseWriter) { w.WriteHeader(401) }},
	} {
		t.Run(name, func(t *testing.T) {
			s, srv := newStub(t)
			if tc.token != nil {
				s.token = tc.token
			}
			if tc.userinfo != nil {
				s.userinfo = tc.userinfo
			}
			_, e := client(t, srv.URL, "").Exchange(context.Background(), code, callback)
			if e == nil || errors.Is(e, login.ErrUnavailable) != tc.unavailable {
				t.Fatalf("error = %v, unavailable want %v", e, tc.unavailable)
			}
			for _, leaked := range []string{secret, code, access} {
				if strings.Contains(e.Error(), leaked) {
					t.Fatalf("error %q leaks %q", e, leaked)
				}
			}
		})
	}
}

func TestDisplayNameFallsBackToUUID(t *testing.T) {
	s, srv := newStub(t)
	s.userinfo = reply(`{"uuid":"uuid~abc"}`)
	if id, e := client(t, srv.URL, "").Exchange(context.Background(), code, callback); e != nil || id.DisplayName != "uuid~abc" {
		t.Fatalf("identity = %+v %v", id, e)
	}
	for raw, want := range map[string]string{
		"cn=张三, en=Zhang San": "张三",
		"en=Zhang San":        "Zhang San",
		"cn=, en=Li Si":       "Li Si",
		" 王五 ":                "王五",
		"cn=":                 "",
		"":                    "",
	} {
		if got := idaas.DisplayName(raw); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestOptionsAreValidated(t *testing.T) {
	h := idaas.NewHTTPClient(time.Second)
	for name, o := range map[string]idaas.Options{
		"no secret":   {ClientID: "a", HTTP: h},
		"no timeout":  {ClientID: "a", ClientSecret: "b", HTTP: &http.Client{}},
		"plain http":  {BaseURL: "http://idaas.example.com", ClientID: "a", ClientSecret: "b", HTTP: h},
		"path":        {BaseURL: "https://idaas.example.com/x", ClientID: "a", ClientSecret: "b", HTTP: h},
		"unknown API": {ClientID: "a", ClientSecret: "b", API: "v3", HTTP: h},
	} {
		if _, e := idaas.New(o); e == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, base := range []string{"", "https://uniportal-beta.huawei.com", "http://127.0.0.1:9000", "http://localhost:9000/"} {
		if _, e := idaas.New(idaas.Options{BaseURL: base, ClientID: "a", ClientSecret: "b", HTTP: h}); e != nil {
			t.Errorf("base %q: %v", base, e)
		}
	}
}
