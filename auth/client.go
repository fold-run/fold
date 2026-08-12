package auth

import (
	"fmt"
	"net/http"
	"time"
)

// TrustAnchorClient returns the HTTP client fold uses to fetch trust-anchor
// documents — issuer JWKS sets and the EMA IdP's JWKS. It matches the
// posture of every other outbound trust-path client (discovery, token
// endpoints, audit webhooks): a bounded timeout so a slow IdP cannot pin
// verification goroutines forever, and redirects refused outright — a
// redirect is never a legitimate step in fetching a key set, and following
// one would let whoever answers the configured URI hand the fetch to a host
// of their choosing.
//
// http.DefaultClient has neither property; never wire it here.
func TrustAnchorClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("jwks: refusing redirect from %q to %q",
				via[0].URL.Redacted(), req.URL.Redacted())
		},
	}
}
