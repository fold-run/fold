package auth

import (
	"fmt"
	"net/http"
	"time"
)

// TrustAnchorClient returns the HTTP client fold uses to talk to trust
// anchors — issuer JWKS sets, the EMA IdP's JWKS, and (via
// NewUpstreamCredentials) upstream token endpoints. It matches the posture of
// every other outbound trust-path client (discovery, audit webhooks): a
// bounded timeout so a slow IdP cannot pin verification goroutines or a
// single-flighted token fetch forever, and redirects refused outright — a
// redirect is never a legitimate step in fetching a key set or a token, and
// following one would let whoever answers the configured URI hand the
// request to a host of their choosing.
//
// http.DefaultClient has neither property; never wire it into any of these.
func TrustAnchorClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return fmt.Errorf("jwks: refusing redirect from %q to %q",
				via[0].URL.Redacted(), req.URL.Redacted())
		},
	}
}
