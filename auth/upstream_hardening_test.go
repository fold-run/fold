package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

// TestTokenExchangeSingleFlight proves a burst of first-time callers for one
// principal produces one token-endpoint call, not one per request: each of
// those calls carries the client secret and the caller's own bearer token.
func TestTokenExchangeSingleFlight(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release // hold the first call open so the others must queue
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"exchanged","expires_in":3600}`)
	}))
	t.Cleanup(ts.Close)

	t.Setenv("CS", "shhh")
	creds := NewUpstreamCredentials(&config.UpstreamAuth{
		Strategy:      "token-exchange",
		TokenEndpoint: ts.URL,
		ClientID:      "fold",
		Audience:      "https://upstream.example",
		ClientAuth:    &config.ClientAuth{Type: "client_secret_post", SecretRef: "CS"},
	}, nil)

	ctx := WithPrincipal(context.Background(), &Principal{
		Subject: "alice", Issuer: "https://idp.example", Token: "caller-token",
	})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			hdr := http.Header{}
			errs[i] = creds.Apply(ctx, hdr)
			if got := hdr.Get("Authorization"); errs[i] == nil && got != "Bearer exchanged" {
				errs[i] = fmt.Errorf("unexpected Authorization %q", got)
			}
		})
	}
	time.Sleep(50 * time.Millisecond) // let the fleet pile up on the fetch lock
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d concurrent callers produced %d token-endpoint calls, want 1", callers, n)
	}
}

// TestTokenResponseIsBounded proves a hostile token endpoint cannot stream
// an unbounded body into gateway memory.
func TestTokenResponseIsBounded(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A never-terminating JSON string: without a cap this reads forever.
		_, _ = w.Write([]byte(`{"access_token":"`))
		chunk := strings.Repeat("A", 64<<10)
		for range 64 { // 4 MiB, well past the 1 MiB cap
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)

	t.Setenv("CS", "shhh")
	creds := NewUpstreamCredentials(&config.UpstreamAuth{
		Strategy:      "client-credentials",
		TokenEndpoint: ts.URL,
		ClientID:      "fold",
		ClientAuth:    &config.ClientAuth{Type: "client_secret_post", SecretRef: "CS"},
	}, nil)

	if err := creds.Apply(context.Background(), http.Header{}); err == nil {
		t.Fatal("an oversized token response should fail, not be accepted")
	}
}

func TestTokenCachePruning(t *testing.T) {
	c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "none"}, nil)
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	for i := range maxCachedTokens + 500 {
		key := fmt.Sprintf("iss\x00sub%d", i)
		exp := future
		if i%2 == 0 {
			exp = past
		}
		c.tokens[key] = &cachedToken{value: "t", expires: exp}
	}
	c.mu.Lock()
	c.pruneLocked()
	c.mu.Unlock()

	if n := len(c.tokens); n > maxCachedTokens {
		t.Fatalf("token cache holds %d entries, cap is %d", n, maxCachedTokens)
	}
	for k, tok := range c.tokens {
		if !time.Now().Before(tok.expires) {
			t.Fatalf("expired entry %q survived pruning", k)
		}
	}
}

// TestFetchGatesDoNotLeak proves the per-key fetch gates are bounded by the
// callers in flight rather than by the callers ever seen. The failing
// exchange is the case that matters: a token that is never minted never
// reaches the token cache, so it was never dropped alongside one — and under
// token-exchange the key is (issuer, subject), which the gateway does not
// choose. A user who has not connected the upstream fails on every request,
// so a gate that outlived its fetch would grow with the user population.
func TestFetchGatesDoNotLeak(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	t.Cleanup(ts.Close)

	t.Setenv("CS", "shhh")
	creds := NewUpstreamCredentials(&config.UpstreamAuth{
		Strategy:      "token-exchange",
		TokenEndpoint: ts.URL,
		ClientID:      "fold",
		Audience:      "https://upstream.example",
		ClientAuth:    &config.ClientAuth{Type: "client_secret_post", SecretRef: "CS"},
	}, nil)

	const principals = 500
	for i := range principals {
		ctx := WithPrincipal(context.Background(), &Principal{
			Subject: fmt.Sprintf("user-%d", i),
			Issuer:  "https://idp.example",
			Token:   "caller-token",
		})
		if err := creds.Apply(ctx, http.Header{}); err == nil {
			t.Fatalf("principal %d: a rejected exchange should surface an error", i)
		}
	}

	creds.mu.Lock()
	gates, tokens := len(creds.fetchMu), len(creds.tokens)
	creds.mu.Unlock()

	if gates != 0 {
		t.Fatalf("%d fetch gates survived %d failed exchanges, want 0", gates, principals)
	}
	if tokens != 0 {
		t.Fatalf("a failed exchange cached %d tokens, want 0", tokens)
	}
}
