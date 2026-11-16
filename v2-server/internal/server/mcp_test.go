package server_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestMCPInstallConfigureAndRemoveHTTP(t *testing.T) {
	t.Setenv("V2_MARKET_CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	// Match the first official Tavily package: Windows ZIP paths and an
	// Authorization reference with no explicit secret metadata.
	for name, value := range map[string]string{
		"orax.toml":          "identifier = \"fixture.search\"\nkind = \"mcp\"\nversion = \"1.0.0\"\n",
		`assets\config.json`: `{"schemaVersion":1,"settings":{"apiKey":{"type":"string","title":"API key","description":"Authentication","required":true}},"transport":{"type":"http","url":"https://example.test/mcp","headers":{"Authorization":{"setting":"apiKey","prefix":"Bearer "}}}}`,
	} {
		f, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = f.Write([]byte(value))
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(archive.Bytes())
	digest := hex.EncodeToString(sum[:])
	f := newSquadServerWith(t, server.Options{SessionTTL: time.Hour, DownloadPluginPackage: func(ctx context.Context, locator, sha string, r pluginmarket.Retrieval, c pluginmarket.Credentials) (string, error) {
		if sha != digest || locator != "https://example.test/search.orax" {
			t.Fatal("wrong package source")
		}
		file, e := os.CreateTemp(t.TempDir(), "mcp-*.orax")
		if e != nil {
			return "", e
		}
		_, e = file.Write(archive.Bytes())
		_ = file.Close()
		return file.Name(), e
	}})
	entry := pluginmarket.Entry{Namespace: "official", Identifier: "fixture.search", Title: "Search", Kind: "mcp", Version: "1.0.0", URL: "https://example.test/search.orax", SHA256: digest, MarketplaceVisible: true}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "mcp.openapi.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(f.ctx); e != nil {
		t.Fatal(e)
	}
	routes, e := legacy.NewRouter(doc)
	if e != nil {
		t.Fatal(e)
	}
	jar, _ := cookiejar.New(nil)
	c := &skillClient{t: t, base: f.srv.URL, http: &http.Client{Jar: jar}, routes: routes}
	c.login("squad-demo")
	u, e := f.store.UserByEmail(f.ctx, storetest.Email("squad-demo"))
	if e != nil {
		t.Fatal(e)
	}
	tenants, e := f.store.Tenants(f.ctx, u.ID)
	if e != nil {
		t.Fatal(e)
	}
	tenant := tenants[0].ID
	a, e := f.store.TenantAccess(f.ctx, tenant, u.ID)
	if e != nil {
		t.Fatal(e)
	}
	spaces, e := f.store.TenantSpaces(f.ctx, a)
	if e != nil {
		t.Fatal(e)
	}
	list := "/api/v1/tenants/" + tenant + "/mcp"
	base := "/api/v1/tenants/" + tenant + "/spaces/" + spaces[0].ID + "/mcp"
	status, body := c.do("POST", base, map[string]any{"identifier": "official/fixture.search", "pluginVersion": "1.0.0"})
	if status != 200 {
		t.Fatal(status, body)
	}
	resource := body["resource"].(map[string]any)
	if resource["complete"] != false {
		t.Fatal("missing config reported complete")
	}
	settings := resource["settings"].([]any)
	if len(settings) != 1 || settings[0].(map[string]any)["secret"] != true {
		t.Fatal("legacy authentication setting not secret")
	}
	const secret = "http-only-fixture-secret"
	status, body = c.do("PUT", base+"/settings", map[string]any{"identifier": "official/fixture.search", "revision": resource["revision"], "values": map[string]any{"apiKey": secret}})
	if status != 200 {
		t.Fatal(status, body)
	}
	resource = body["resource"].(map[string]any)
	if resource["complete"] != true {
		t.Fatal(body)
	}
	serialized, _ := json.Marshal(body)
	configured := resource["configuredSecrets"].([]any)
	if bytes.Contains(serialized, []byte(secret)) || len(configured) != 1 || configured[0] != "apiKey" {
		t.Fatal("write response exposed authentication setting")
	}
	status, body = c.do("GET", list, nil)
	serialized, _ = json.Marshal(body)
	if status != 200 || bytes.Contains(serialized, []byte(secret)) {
		t.Fatal("read secret", status)
	}
	status, body = c.do("PUT", base+"/settings", map[string]any{"identifier": "official/fixture.search", "revision": 1, "values": map[string]any{}})
	if status != 409 {
		t.Fatal(status, body)
	}
	storetest.SignIn(t, f.store, "mcp-member", "member", "member")
	c.login("mcp-member")
	status, body = c.do("GET", list, nil)
	serialized, _ = json.Marshal(body)
	if status != 200 || bytes.Contains(serialized, []byte(secret)) {
		t.Fatal(status, body)
	}
	for _, method := range []string{"POST", "DELETE"} {
		status, body = c.do(method, base, map[string]any{"identifier": "official/fixture.search", "revision": resource["revision"]})
		if status != 403 {
			t.Fatal(method, status, body)
		}
	}
	c.login("squad-demo")
	status, body = c.do("DELETE", base, map[string]any{"identifier": "official/fixture.search", "revision": resource["revision"]})
	if status != 204 {
		t.Fatal(status, body)
	}
	status, body = c.do("GET", list, nil)
	if status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatal(status, body)
	}
}
