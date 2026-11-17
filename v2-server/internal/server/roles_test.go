package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

type roleClient struct {
	t      *testing.T
	base   string
	http   *http.Client
	routes routers.Router
}

func newRoleClient(t *testing.T, base string) *roleClient {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "roles.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	routes, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	return &roleClient{t: t, base: base, http: &http.Client{Jar: jar}, routes: routes}
}

func (c *roleClient) login(subject string) {
	c.t.Helper()
	body, _ := json.Marshal(map[string]string{"provider": "email", "email": storetest.Email(subject)})
	res, err := c.http.Post(c.base+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		c.t.Fatalf("login = %d %s", res.StatusCode, raw)
	}
}

func (c *roleClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	routeReq := req.Clone(context.Background())
	routeReq.URL.Scheme, routeReq.URL.Host = "http", "localhost:8080"
	route, params, err := c.routes.FindRoute(routeReq)
	if err != nil {
		c.t.Fatalf("%s %s is not in roles.openapi.json: %v", method, path, err)
	}
	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 res.StatusCode, Header: res.Header,
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	input.SetBodyBytes(raw)
	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		c.t.Fatalf("%s %s does not match roles.openapi.json: %v", method, path, err)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func (c *roleClient) tenant() string {
	c.t.Helper()
	res, err := c.http.Get(c.base + "/api/v1/me/tenants")
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("tenants = %d", res.StatusCode)
	}
	body := map[string]any{}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		c.t.Fatal(err)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		c.t.Fatalf("tenants = %v", body)
	}
	return items[0].(map[string]any)["id"].(string)
}

func TestRolesHTTP(t *testing.T) {
	f := newSquadServer(t)
	storetest.SignIn(t, f.store, "roles-demo", "角色演示", "owner")
	c := newRoleClient(t, f.srv.URL)
	c.login("roles-demo")
	tenant := c.tenant()
	base := "/api/v2/tenants/" + tenant + "/roles"

	// No starter roles: the catalog is empty until someone creates one.
	status, body := c.do("GET", base, nil)
	if status != 200 || len(body["items"].([]any)) != 0 {
		t.Fatalf("initial roles = %d %v", status, body)
	}
	status, body = c.do("POST", base, map[string]any{
		"name": "代码审查", "description": "检查改动", "content": "# 代码审查",
	})
	if status != 201 {
		t.Fatalf("create = %d %v", status, body)
	}
	id := body["id"].(string)
	if body["name"] != "代码审查" || body["version"] != float64(1) {
		t.Fatalf("created = %v", body)
	}
	status, body = c.do("POST", base, map[string]any{
		"name": "代码审查", "content": "# 重复",
	})
	if status != 409 || body["code"] != "role_name_conflict" {
		t.Fatalf("duplicate = %d %v", status, body)
	}
	status, body = c.do("POST", base, map[string]any{
		"name": "a/b", "content": "# 路径分隔符",
	})
	if status != 400 || body["code"] != "role_name_invalid" {
		t.Fatalf("invalid name = %d %v", status, body)
	}
	status, body = c.do("PUT", base+"/"+id, map[string]any{
		"name": "代码审查", "description": "更新说明", "content": "# 代码审查 v2",
	})
	if status != 200 || body["version"] != float64(2) || body["description"] != "更新说明" {
		t.Fatalf("update = %d %v", status, body)
	}
	status, body = c.do("GET", base+"/"+id, nil)
	if status != 200 || body["content"] != "# 代码审查 v2" {
		t.Fatalf("get = %d %v", status, body)
	}
	status, body = c.do("GET", base+"/00000000-0000-4000-8000-000000000000", nil)
	if status != 404 || body["code"] != "role_not_found" {
		t.Fatalf("unknown role = %d %v", status, body)
	}

	imported := "---\nname: 技术写作\ndescription: 技术写作说明\n---\n\n# 技术写作\n"
	status, body = c.do("POST", base+"/import", map[string]any{
		"fileName": "writer.md", "content": imported,
	})
	if status != 201 || body["name"] != "技术写作" {
		t.Fatalf("import = %d %v", status, body)
	}
	status, body = c.do("POST", base+"/import", map[string]any{
		"fileName": "writer.md", "content": imported,
	})
	if status != 409 || body["code"] != "role_name_conflict" {
		t.Fatalf("import conflict = %d %v", status, body)
	}
	status, body = c.do("POST", base+"/import", map[string]any{
		"fileName": "writer.txt", "content": imported,
	})
	if status != 400 || body["code"] != "role_file_type" {
		t.Fatalf("wrong file type = %d %v", status, body)
	}
	status, body = c.do("POST", base+"/import", map[string]any{
		"fileName": "writer.md", "content": "# 没有 front matter",
	})
	if status != 400 || body["code"] != "role_front_matter_missing" {
		t.Fatalf("invalid import = %d %v", status, body)
	}

	// Every member of the workspace reads, edits and imports the same catalog.
	member := newRoleClient(t, f.srv.URL)
	storetest.SignIn(t, f.store, "roles-http-member", "member", "member")
	member.login("roles-http-member")
	status, body = member.do("GET", base, nil)
	if status != 200 || len(body["items"].([]any)) != 2 {
		t.Fatalf("member roles = %d %v", status, body)
	}
	status, body = member.do("PUT", base+"/"+id, map[string]any{
		"name": "代码审查", "description": "成员更新", "content": "# 成员改的内容",
	})
	if status != 200 || body["description"] != "成员更新" {
		t.Fatalf("member update = %d %v", status, body)
	}

	status, _ = c.do("DELETE", base+"/"+id, nil)
	if status != 204 {
		t.Fatalf("delete = %d", status)
	}
	status, body = c.do("GET", base+"/"+id, nil)
	if status != 404 || body["code"] != "role_not_found" {
		t.Fatalf("deleted role = %d %v", status, body)
	}
}
