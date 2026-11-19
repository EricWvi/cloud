// Package login is the contract between the HTTP login flow and the external login methods it
// redirects to. A method sends the browser to its own sign-in page and later turns the code it
// returned into an identity; it never sees Cloud's sessions, accounts or memberships.
package login

import (
	"context"
	"errors"
)

// ErrUnavailable marks a failure on the provider's side (network, timeout, rate limit, server
// error) that the user can retry later. Any other Exchange error means the sign-in was refused.
var ErrUnavailable = errors.New("login provider unavailable")

// Identity is who a provider verified: Source names the provider's identity namespace and Subject
// is its stable, opaque key for the person. Together they find or create one account.
type Identity struct {
	Source      string
	Subject     string
	DisplayName string
}

// Provider is one redirect-based login method (an OAuth authorization code flow).
type Provider interface {
	// AuthorizeURL is where the browser signs in; the provider returns it to redirectURI with a
	// code and this state.
	AuthorizeURL(state, redirectURI string) string
	// Exchange turns the code into the verified identity. redirectURI is the one AuthorizeURL used.
	Exchange(ctx context.Context, code, redirectURI string) (Identity, error)
}
