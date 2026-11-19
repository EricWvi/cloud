//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/idaas"
	"github.com/wanglongan587/cloud/v2-server/internal/login"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// envDefault preserves an explicit empty value, which disables optional integrations.
func envDefault(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// loginMethods is the parsed V2_LOGIN: the login methods this process offers, in the order the
// login page shows them.
type loginMethods []string

// has reports whether the method is offered.
func (m loginMethods) has(method string) bool { return slices.Contains(m, method) }

// parseLogin reads V2_LOGIN, a comma-separated list of login methods. It must be set: there is no
// default, so a deployment that forgot it never comes up with the passwordless email login (A6
// invariant 1). An unknown, repeated or empty entry fails as well, all before any database or
// filesystem effect.
func parseLogin(value string, fixtures bool) (loginMethods, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("V2_LOGIN must be set to the login methods to offer, comma-separated: email for development; this build provides %s", strings.Join(server.LoginProviders, ", "))
	}
	var methods loginMethods
	for _, entry := range strings.Split(value, ",") {
		method := strings.TrimSpace(entry)
		switch {
		case method == "":
			return nil, fmt.Errorf("V2_LOGIN %q has an empty entry", value)
		case !slices.Contains(server.LoginProviders, method):
			return nil, fmt.Errorf("V2_LOGIN lists unknown login method %q; this build provides %s", method, strings.Join(server.LoginProviders, ", "))
		case methods.has(method):
			return nil, fmt.Errorf("V2_LOGIN lists %q twice", method)
		}
		methods = append(methods, method)
	}
	// Fixtures seed and drive the development accounts, which exist only where email login does.
	if fixtures && !methods.has(server.LoginProviderEmail) {
		return nil, errors.New("fixture flags require the email login in V2_LOGIN")
	}
	return methods, nil
}

// parsePublicURL reads V2_PUBLIC_URL, the address browsers use for this server. Empty leaves it
// unset. It must be the root of an absolute http or https URL: routes, the frontend's API paths and
// the __Host- cookies (Path=/) all live at the root, so a path prefix could never work.
func parsePublicURL(value string) (*url.URL, error) {
	if value == "" {
		return nil, nil
	}
	u, e := url.Parse(value)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("V2_PUBLIC_URL %q must be an absolute http or https URL without query or fragment", value)
	}
	// Browsers send Origin with a lowercase host and without the scheme's default port, and
	// Origin is compared as a string, so the stored address takes that form too.
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("V2_PUBLIC_URL %q must be a root address without a path: Ora serves its pages, /api and /auth only at the root", value)
	}
	u.Path = ""
	return u, nil
}

// loginRedirects builds the redirect login methods V2_LOGIN offers from their configuration,
// before any database or filesystem effect. Every such method needs V2_PUBLIC_URL, from which its
// callback address is derived; it must be https, or http on a loopback address for local
// development against a stand-in provider.
func loginRedirects(methods loginMethods, publicURL *url.URL, getenv func(string) string) (map[string]login.Provider, error) {
	redirects := map[string]login.Provider{}
	if !methods.has(server.LoginProviderHuaweiIDaaS) {
		return redirects, nil
	}
	if publicURL == nil || (publicURL.Scheme != "https" && !idaas.Loopback(publicURL.Hostname())) {
		return nil, errors.New("V2_LOGIN offers huawei-idaas, which needs an https V2_PUBLIC_URL (http only on a loopback address)")
	}
	var missing []string
	for _, key := range []string{"V2_IDAAS_CLIENT_ID", "V2_IDAAS_CLIENT_SECRET"} {
		if getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("V2_LOGIN offers huawei-idaas but %s is not set", strings.Join(missing, " and "))
	}
	client, e := idaas.New(idaas.Options{
		BaseURL:          getenv("V2_IDAAS_BASE_URL"),
		ClientID:         getenv("V2_IDAAS_CLIENT_ID"),
		ClientSecret:     getenv("V2_IDAAS_CLIENT_SECRET"),
		API:              idaas.API(getenv("V2_IDAAS_API")),
		Scope:            getenv("V2_IDAAS_SCOPE"),
		DisplayNameField: getenv("V2_IDAAS_DISPLAY_NAME_FIELD"),
		HTTP:             idaas.NewHTTPClient(10 * time.Second),
	})
	if e != nil {
		return nil, e
	}
	redirects[server.LoginProviderHuaweiIDaaS] = client
	return redirects, nil
}

// databaseProfile is what a start with these login methods makes of the database: email login
// brings the development accounts and demo data, so only it may start a development database.
func databaseProfile(methods loginMethods) string {
	if methods.has(server.LoginProviderEmail) {
		return store.ProfileDevelopment
	}
	return store.ProfileDeployment
}
