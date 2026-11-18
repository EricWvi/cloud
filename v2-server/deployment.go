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

	"github.com/wanglongan587/cloud/v2-server/internal/server"
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

// parseLogin reads V2_LOGIN, a comma-separated list of login methods. Unset or empty means email, so
// a development checkout starts without configuration; a deployment names its methods explicitly.
// An unknown, repeated or empty entry fails before any database or filesystem effect.
func parseLogin(value string, fixtures bool) (loginMethods, error) {
	if strings.TrimSpace(value) == "" {
		value = server.LoginProviderEmail
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
// unset. It must be an absolute http or https URL without query or fragment; a path prefix is kept
// for addresses derived from it later.
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
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}
