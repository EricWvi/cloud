// Package idaas signs people in with Huawei IDaaS through the OAuth authorization code flow, as a
// confidential client (client_secret_post, no PKCE). It turns a code into the corporate account's
// uuid and display name; IDaaS tokens and profile documents never leave this package.
package idaas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/login"
)

const (
	// Source is the account namespace of Huawei corporate identities (A6 D3).
	Source = "huawei-corp"
	// DefaultBaseURL is the production IDaaS origin.
	DefaultBaseURL = "https://uniportal.huawei.com"
	// DefaultScope is the minimal profile scope, used for authorization and userinfo alike.
	DefaultScope = "base.profile"
	// DefaultDisplayNameField is the userinfo field that holds the display name. IDaaS returns it
	// only when the application was granted that attribute; without it accounts show their uuid.
	DefaultDisplayNameField = "displayName"
	maxResponse             = 64 << 10
)

// API selects which of IDaaS's two endpoint sets to call.
type API string

const (
	// APIOAuth2 is the set IDaaS's integration guide and official sample clients use: authorize
	// with display=page, then JSON requests to accesstoken and userinfo, which report failures as
	// an errorCode in an HTTP 200 body.
	APIOAuth2 API = "oauth2"
	// APIOAuth2V1 is the older /oauth2/v1/ set with form requests, which the previous Gateway
	// targeted. It is kept selectable until a real login settles which set the deployment uses.
	APIOAuth2V1 API = "oauth2-v1"
)

// Options configures one IDaaS application.
type Options struct {
	BaseURL          string
	ClientID         string
	ClientSecret     string
	API              API
	Scope            string
	DisplayNameField string
	// HTTP must have a timeout and should not follow redirects (see NewHTTPClient).
	HTTP *http.Client
}

// Client is a configured IDaaS application. It implements login.Provider.
type Client struct {
	base                          *url.URL
	clientID, clientSecret, scope string
	api                           API
	displayNameField              string
	http                          *http.Client
}

var _ login.Provider = (*Client)(nil)

// New validates the options. The base URL must be an https origin; plain http is accepted only
// for a loopback address, where a local stand-in serves development and tests.
func New(o Options) (*Client, error) {
	if o.ClientID == "" || o.ClientSecret == "" {
		return nil, errors.New("IDaaS client ID and secret are required")
	}
	if o.HTTP == nil || o.HTTP.Timeout <= 0 {
		return nil, errors.New("IDaaS HTTP client with a timeout is required")
	}
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	base, e := url.Parse(o.BaseURL)
	if e != nil || base.Host == "" || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.Fragment != "" || base.User != nil ||
		(base.Scheme != "https" && !(base.Scheme == "http" && Loopback(base.Hostname()))) {
		return nil, fmt.Errorf("IDaaS base URL %q must be an https origin (http only on a loopback address)", o.BaseURL)
	}
	base.Path = ""
	switch o.API {
	case "":
		o.API = APIOAuth2
	case APIOAuth2, APIOAuth2V1:
	default:
		return nil, fmt.Errorf("IDaaS API %q must be %s or %s", o.API, APIOAuth2, APIOAuth2V1)
	}
	if o.Scope == "" {
		o.Scope = DefaultScope
	}
	if o.DisplayNameField == "" {
		o.DisplayNameField = DefaultDisplayNameField
	}
	return &Client{base: base, clientID: o.ClientID, clientSecret: o.ClientSecret, scope: o.Scope, api: o.API, displayNameField: o.DisplayNameField, http: o.HTTP}, nil
}

// Loopback reports whether host is a loopback name or address.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewHTTPClient returns a client bounded by timeout that does not follow redirects: only the
// browser follows the authorize redirect chain.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c *Client) endpoint(name string) string {
	u := *c.base
	if c.api == APIOAuth2V1 {
		u.Path = "/saaslogin1/oauth2/v1/" + name
	} else {
		u.Path = "/saaslogin1/oauth2/" + name
	}
	return u.String()
}

// Endpoints lists the addresses this client calls, for the startup log.
func (c *Client) Endpoints() (authorize, token, userinfo string) {
	if c.api == APIOAuth2V1 {
		return c.endpoint("authorize"), c.endpoint("token"), c.endpoint("userinfo")
	}
	return c.endpoint("authorize"), c.endpoint("accesstoken"), c.endpoint("userinfo")
}

// AuthorizeURL is the IDaaS sign-in page for this application.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	authorize, _, _ := c.Endpoints()
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", c.scope)
	q.Set("state", state)
	if c.api == APIOAuth2 {
		q.Set("display", "page")
	}
	return authorize + "?" + q.Encode()
}

// Exchange redeems the code with client_secret_post, reads the profile, and drops the token.
func (c *Client) Exchange(ctx context.Context, code, redirectURI string) (login.Identity, error) {
	if code == "" {
		return login.Identity{}, errors.New("IDaaS returned no code")
	}
	_, tokenURL, userinfoURL := c.Endpoints()
	var token map[string]json.RawMessage
	if e := c.post(ctx, "token", tokenURL, map[string]string{
		"grant_type": "authorization_code", "client_id": c.clientID, "client_secret": c.clientSecret,
		"code": code, "redirect_uri": redirectURI,
	}, &token); e != nil {
		return login.Identity{}, e
	}
	access := text(token["access_token"])
	if access == "" {
		return login.Identity{}, errors.New("IDaaS token response has no access_token")
	}
	if kind := text(token["token_type"]); kind != "" && !strings.EqualFold(kind, "bearer") {
		return login.Identity{}, fmt.Errorf("IDaaS token type %q is not bearer", kind)
	}
	userinfo := map[string]string{"access_token": access}
	if c.api == APIOAuth2 {
		userinfo["client_id"], userinfo["scope"] = c.clientID, c.scope
	}
	var profile map[string]json.RawMessage
	if e := c.post(ctx, "userinfo", userinfoURL, userinfo, &profile); e != nil {
		return login.Identity{}, e
	}
	// IDaaS pads uuid with whitespace in some responses; the padding is not part of the key.
	uuid := strings.TrimSpace(text(profile["uuid"]))
	if uuid == "" {
		return login.Identity{}, errors.New("IDaaS userinfo has no uuid")
	}
	name := DisplayName(text(profile[c.displayNameField]))
	if name == "" {
		name = uuid
	}
	return login.Identity{Source: Source, Subject: uuid, DisplayName: name}, nil
}

// DisplayName reads IDaaS's display name, which for intranet accounts combines both languages as
// "cn=张三, en=Zhang San": the Chinese name wins, then the English one, and any other form is used
// as it is.
func DisplayName(raw string) string {
	raw = strings.TrimSpace(raw)
	names, structured := map[string]string{}, false
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(part, "=")
		if key = strings.ToLower(strings.TrimSpace(key)); ok && (key == "cn" || key == "en") {
			structured, names[key] = true, strings.TrimSpace(value)
		}
	}
	if !structured {
		return raw
	}
	if names["cn"] != "" {
		return names["cn"]
	}
	return names["en"]
}

// post sends one request and decodes a JSON object. The official API takes a JSON body; the v1 API
// a form. Either way credentials stay in the body, never the URL. A failure says which step and,
// when IDaaS gave one, its errorCode, and never echoes the code, token or secret.
func (c *Client) post(ctx context.Context, step, target string, fields map[string]string, out *map[string]json.RawMessage) error {
	var body io.Reader
	contentType := "application/json"
	if c.api == APIOAuth2V1 {
		form := url.Values{}
		for k, v := range fields {
			form.Set(k, v)
		}
		body, contentType = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	} else {
		raw, _ := json.Marshal(fields)
		body = bytes.NewReader(raw)
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if e != nil {
		return fmt.Errorf("IDaaS %s request: %w", step, e)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	res, e := c.http.Do(req)
	if e != nil {
		// Only the kind of failure: the URL in a transport error carries no secret, but keep it terse.
		return fmt.Errorf("%w: IDaaS %s: %s", login.ErrUnavailable, step, transportReason(e))
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if e != nil {
		return fmt.Errorf("%w: IDaaS %s: reading response", login.ErrUnavailable, step)
	}
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: IDaaS %s: HTTP %d", login.ErrUnavailable, step, res.StatusCode)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("IDaaS %s response exceeds %d bytes", step, maxResponse)
	}
	if e := json.Unmarshal(raw, out); e != nil || *out == nil {
		return fmt.Errorf("IDaaS %s: HTTP %d without a JSON object", step, res.StatusCode)
	}
	// IDaaS reports failures as an errorCode, usually with HTTP 200; E_30001 is its own system error.
	if code := text((*out)["errorCode"]); code != "" {
		if code == "E_30001" {
			return fmt.Errorf("%w: IDaaS %s: errorCode %s", login.ErrUnavailable, step, code)
		}
		return fmt.Errorf("IDaaS %s refused: errorCode %s", step, code)
	}
	if oauth := text((*out)["error"]); oauth != "" {
		return fmt.Errorf("IDaaS %s refused: error %s", step, oauth)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("IDaaS %s refused: HTTP %d", step, res.StatusCode)
	}
	return nil
}

func transportReason(e error) string {
	var timeout interface{ Timeout() bool }
	if errors.As(e, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	if errors.Is(e, context.Canceled) {
		return "canceled"
	}
	return "connection failed"
}

// text reads a JSON string, or a number as its literal; anything else is empty.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
