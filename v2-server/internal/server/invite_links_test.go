package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

type inviteFixture struct {
	t      *testing.T
	srv    *httptest.Server
	store  *store.Store
	sql    *pgx.Conn
	tenant string
	owner  *http.Cookie
}

func newInviteFixture(t *testing.T, o server.Options) *inviteFixture {
	t.Helper()
	s, sql := openStoreSQL(t)
	token := storetest.SignIn(t, s, "demo", "演示用户", store.RoleOwner)
	_, tenant := storetest.Login(t, s, "demo")
	o.SessionTTL = time.Hour
	srv := httptest.NewServer(server.New(s, o))
	t.Cleanup(srv.Close)
	return &inviteFixture{t: t, srv: srv, store: s, sql: sql, tenant: tenant, owner: &http.Cookie{Name: server.SessionCookie, Value: token}}
}

// outsider signs in an account that belongs to no workspace, like a first login through IDaaS.
func (f *inviteFixture) outsider(name string) (string, *http.Cookie) {
	f.t.Helper()
	var id string
	if e := f.sql.QueryRow(context.Background(), `INSERT INTO users (source, subject, display_name) VALUES ('test', $1, $1) RETURNING id`, name).Scan(&id); e != nil {
		f.t.Fatal(e)
	}
	token, e := f.store.StartSession(context.Background(), id, time.Hour)
	if e != nil {
		f.t.Fatal(e)
	}
	return id, &http.Cookie{Name: server.SessionCookie, Value: token}
}

// membersContract is the candidate contract the invitation routes implement.
var membersContract = sync.OnceValues(func() (routers.Router, error) {
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "members.openapi.json"))
	if e != nil {
		return nil, e
	}
	if e := doc.Validate(context.Background()); e != nil {
		return nil, e
	}
	return legacy.NewRouter(doc)
})

// call sends one request and checks the response against members.openapi.json when the route is
// one it describes.
func (f *inviteFixture) call(method, path string, c *http.Cookie, body any) (int, map[string]any) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, f.srv.URL+path, reader)
	if e != nil {
		f.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if c != nil {
		req.AddCookie(c)
	}
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	routes, e := membersContract()
	if e != nil {
		f.t.Fatal(e)
	}
	if route, params, e := routes.FindRoute(req); e == nil {
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 res.StatusCode, Header: res.Header,
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		in.SetBodyBytes(raw)
		if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
			f.t.Fatalf("%s %s does not match members.openapi.json: %v", method, path, e)
		}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (f *inviteFixture) tenantsOf(c *http.Cookie) int {
	f.t.Helper()
	_, body := f.call("GET", "/api/v1/me/tenants", c, nil)
	items, _ := body["items"].([]any)
	return len(items)
}

func TestInviteLinkFlowOverHTTP(t *testing.T) {
	f := newInviteFixture(t, server.Options{})
	links := "/api/v2/tenants/" + f.tenant + "/invite-links"

	status, created := f.call("POST", links, f.owner, map[string]string{})
	link, _ := created["link"].(map[string]any)
	rawURL, _ := created["url"].(string)
	if status != 201 || link == nil || !strings.HasPrefix(rawURL, "/invite/") {
		t.Fatalf("create = %d %v", status, created)
	}
	token := strings.TrimPrefix(rawURL, "/invite/")
	status, listed := f.call("GET", links, f.owner, nil)
	if items, _ := listed["items"].([]any); status != 200 || len(items) != 1 || strings.Contains(stringify(listed), token) {
		t.Fatalf("list = %d %v", status, listed)
	}

	alice, aliceCookie := f.outsider("alice")
	_ = alice
	if f.tenantsOf(aliceCookie) != 0 {
		t.Fatal("a fresh account already sees a workspace")
	}
	// A member cannot generate links; a visitor without a session cannot preview.
	if status, _ := f.call("POST", links, aliceCookie, map[string]string{}); status != 404 {
		t.Fatalf("outsider create = %d", status)
	}
	if status, body := f.call("POST", "/api/v1/invites/preview", nil, map[string]string{"token": token}); status != 401 {
		t.Fatalf("anonymous preview = %d %v", status, body)
	}
	status, preview := f.call("POST", "/api/v1/invites/preview", aliceCookie, map[string]string{"token": token})
	if status != 200 || preview["state"] != "active" || preview["alreadyMember"] != false || preview["tenant"].(map[string]any)["id"] != f.tenant {
		t.Fatalf("preview = %d %v", status, preview)
	}
	// Joining is a write: another site's page cannot make the visitor join.
	if res, body := rawCall(t, "POST", f.srv.URL+"/api/v1/invites/accept", "https://evil.example", aliceCookie, map[string]string{"token": token}); res.StatusCode != 403 || body["code"] != "origin_mismatch" {
		t.Fatalf("cross-site accept = %d %v", res.StatusCode, body)
	}
	if status, body := f.call("POST", "/api/v1/invites/accept", aliceCookie, map[string]string{"token": token}); status != 200 || body["tenantId"] != f.tenant {
		t.Fatalf("accept = %d %v", status, body)
	}
	// The next request sees the workspace without signing in again.
	if f.tenantsOf(aliceCookie) != 1 {
		t.Fatal("joined member does not see the workspace")
	}

	_, bobCookie := f.outsider("bob")
	status, body := f.call("POST", "/api/v1/invites/accept", bobCookie, map[string]string{"token": token})
	if params, _ := body["params"].(map[string]any); status != 410 || body["code"] != "invite_unavailable" || params["reason"] != "used" {
		t.Fatalf("reuse = %d %v", status, body)
	}
	if status, body := f.call("POST", "/api/v1/invites/preview", bobCookie, map[string]string{"token": "unknown"}); status != 404 || body["code"] != "invite_not_found" {
		t.Fatalf("unknown token = %d %v", status, body)
	}
	id, _ := link["id"].(string)
	if status, body := f.call("DELETE", links+"/"+id, f.owner, nil); status != 409 || body["code"] != "invite_already_used" {
		t.Fatalf("revoke used = %d %v", status, body)
	}
}

func TestRevokingALinkOverHTTP(t *testing.T) {
	f := newInviteFixture(t, server.Options{})
	links := "/api/v2/tenants/" + f.tenant + "/invite-links"
	_, created := f.call("POST", links, f.owner, map[string]string{})
	id := created["link"].(map[string]any)["id"].(string)
	token := strings.TrimPrefix(created["url"].(string), "/invite/")
	if status, _ := f.call("DELETE", links+"/"+id, f.owner, nil); status != 204 {
		t.Fatalf("revoke = %d", status)
	}
	_, aliceCookie := f.outsider("alice")
	if status, body := f.call("POST", "/api/v1/invites/accept", aliceCookie, map[string]string{"token": token}); status != 410 || body["params"].(map[string]any)["reason"] != "revoked" {
		t.Fatalf("accept revoked = %d %v", status, body)
	}
	if status, _ := f.call("DELETE", links+"/not-a-uuid", f.owner, nil); status != 404 {
		t.Fatalf("revoke malformed = %d", status)
	}
	if status, body := f.call("POST", links, f.owner, map[string]string{"squadId": "00000000-0000-0000-0000-000000000000"}); status != 404 || body["code"] != "squad_not_found" {
		t.Fatalf("unknown squad = %d %v", status, body)
	}
}

func TestInviteURLUsesThePublicAddress(t *testing.T) {
	public, _ := url.Parse("https://ora.example.com")
	f := newInviteFixture(t, server.Options{PublicURL: public})
	f.owner.Name = server.SecureSessionCookie
	_, created := f.call("POST", "/api/v2/tenants/"+f.tenant+"/invite-links", f.owner, map[string]string{})
	if u, _ := created["url"].(string); !strings.HasPrefix(u, "https://ora.example.com/invite/") {
		t.Fatalf("url = %v", created)
	}
}

func TestRemoveMemberOverHTTP(t *testing.T) {
	f := newInviteFixture(t, server.Options{})
	_, created := f.call("POST", "/api/v2/tenants/"+f.tenant+"/invite-links", f.owner, map[string]string{})
	token := strings.TrimPrefix(created["url"].(string), "/invite/")
	alice, aliceCookie := f.outsider("alice")
	f.call("POST", "/api/v1/invites/accept", aliceCookie, map[string]string{"token": token})
	members := "/api/v2/tenants/" + f.tenant + "/members/"

	// A member cannot remove anyone.
	var owner string
	if e := f.sql.QueryRow(context.Background(), `SELECT user_id FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, f.tenant).Scan(&owner); e != nil {
		t.Fatal(e)
	}
	if status, _ := f.call("DELETE", members+owner, aliceCookie, nil); status != 403 {
		t.Fatalf("member removes owner = %d", status)
	}
	if status, body := f.call("DELETE", members+owner, f.owner, nil); status != 409 || body["code"] != "last_owner" {
		t.Fatalf("remove last owner = %d %v", status, body)
	}
	if status, _ := f.call("DELETE", members+alice, f.owner, nil); status != 204 {
		t.Fatalf("remove = %d", status)
	}
	// Alice still signs in but no longer sees the workspace, and its routes hide it.
	if status, _ := f.call("GET", "/api/v1/me", aliceCookie, nil); status != 200 {
		t.Fatalf("removed member me = %d", status)
	}
	if f.tenantsOf(aliceCookie) != 0 {
		t.Fatal("removed member still sees the workspace")
	}
	if status, _ := f.call("GET", "/api/v1/tenants/"+f.tenant+"/members", aliceCookie, nil); status != 404 {
		t.Fatalf("removed member reads the roster = %d", status)
	}
	if status, body := f.call("DELETE", members+alice, f.owner, nil); status != 404 || body["code"] != "member_not_found" {
		t.Fatalf("remove twice = %d %v", status, body)
	}
}

// Without email login, members join by link only: inviting an address is not offered.
func TestEmailInviteOnlyWithEmailLogin(t *testing.T) {
	f := newInviteFixture(t, server.Options{LoginProviders: []string{"huawei-idaas"}})
	path := "/api/v2/tenants/" + f.tenant + "/members"
	if status, _ := f.call("POST", path, f.owner, map[string]string{"email": "new@demo.test"}); status != 404 {
		t.Fatalf("email invite without email login = %d", status)
	}
	g := newInviteFixture(t, server.Options{})
	if status, body := g.call("POST", "/api/v2/tenants/"+g.tenant+"/members", g.owner, map[string]string{"email": "new@demo.test"}); status != 201 {
		t.Fatalf("email invite with email login = %d %v", status, body)
	}
}

func stringify(v map[string]any) string {
	var b strings.Builder
	for k, x := range v {
		b.WriteString(k)
		b.WriteString("=")
		switch y := x.(type) {
		case []any:
			for _, z := range y {
				if m, ok := z.(map[string]any); ok {
					b.WriteString(stringify(m))
				}
			}
		case map[string]any:
			b.WriteString(stringify(y))
		case string:
			b.WriteString(y)
		}
		b.WriteString(";")
	}
	return b.String()
}

// A removed member keeps the account: with email login the address still signs in, and the page
// then asks for an invitation instead of refusing the login.
func TestRemovedMemberStillSignsInByEmail(t *testing.T) {
	f := newInviteFixture(t, server.Options{})
	status, invited := f.call("POST", "/api/v2/tenants/"+f.tenant+"/members", f.owner, map[string]string{"email": "alice@demo.test"})
	if status != 201 {
		t.Fatalf("invite = %d %v", status, invited)
	}
	if status, _ := f.call("DELETE", "/api/v2/tenants/"+f.tenant+"/members/"+invited["userId"].(string), f.owner, nil); status != 204 {
		t.Fatalf("remove = %d", status)
	}
	res, body := rawCall(t, "POST", f.srv.URL+"/auth/login", "", nil, map[string]string{"provider": "email", "email": "alice@demo.test"})
	if res.StatusCode != 200 {
		t.Fatalf("login after removal = %d %v", res.StatusCode, body)
	}
	if f.tenantsOf(sessionCookie(t, res)) != 0 {
		t.Fatal("removed member still sees the workspace")
	}
	// An address without an account is still refused.
	if res, body := rawCall(t, "POST", f.srv.URL+"/auth/login", "", nil, map[string]string{"provider": "email", "email": "nobody@demo.test"}); res.StatusCode != 403 || body["code"] != "email_not_invited" {
		t.Fatalf("unknown address = %d %v", res.StatusCode, body)
	}
}
