// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/auth"
)

// TestUnknownKidTriggersOneBoundedRefresh pins the JWKS rotation behavior:
// a token signed by a newly rotated key authenticates after exactly one
// forced refetch, and a flood of forged kids inside the rate window cannot
// produce unbounded refetching (ADR-0012).
func TestUnknownKidTriggersOneBoundedRefresh(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)

	rotatedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp.keyID = "test-key-2"
	idp.jwksBody = idp.renderJWKS(rotatedKey)

	claims := idp.defaultClaims(time.Now())
	token := idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": "test-key-2"}, claims, rotatedKey)

	principal, err := authenticator.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("rotated-key token rejected: %v", err)
	}
	if principal.Subject != "alice" {
		t.Fatalf("principal subject = %q", principal.Subject)
	}
	if fetches := idp.fetches; fetches != 2 { // startup + one forced refresh
		t.Fatalf("jwks fetches = %d, want exactly 2 (startup + one rotation refresh)", fetches)
	}

	forge := func(kid string) string {
		return idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": kid}, claims, rotatedKey)
	}
	for _, kid := range []string{"forged-1", "forged-2", "forged-3"} {
		if _, err := authenticator.Authenticate(context.Background(), forge(kid)); err == nil {
			t.Fatalf("forged kid %q authenticated", kid)
		}
	}
	if fetches := idp.fetches; fetches != 2 {
		t.Fatalf("jwks fetches = %d after forged-kid flood, want 2; refresh is not rate-limited", fetches)
	}
}

// TestOversizedJWKSIsRejectedAtStartup pins the bounded-fetch behavior:
// metadata documents beyond the size cap fail composition loudly.
func TestOversizedJWKSIsRejectedAtStartup(t *testing.T) {
	idp := newTestIDP(t)
	previous := idp.jwksBody
	idp.jwksBody = []byte(`{"keys":[{"kty":"RSA","kid":"k","use":"sig","n":"` + strings.Repeat("A", 3<<20) + `","e":"AQAB"}]}`)
	defer func() { idp.jwksBody = previous }()

	if _, err := auth.NewOIDCAuthenticator(context.Background(), configFor(t, idp)); err == nil {
		t.Fatal("oversized JWKS document was accepted")
	}
}

// TestExcessiveKeyCountIsRejectedAtStartup pins the JWKS key-count cap.
func TestExcessiveKeyCountIsRejectedAtStartup(t *testing.T) {
	idp := newTestIDP(t)
	keys := make([]string, 0, 100)
	for index := range 100 {
		keys = append(keys, fmt.Sprintf(`{"kty":"RSA","kid":"key-%d","use":"sig","n":"AQAB","e":"AQAB"}`, index))
	}
	previous := idp.jwksBody
	idp.jwksBody = []byte(`{"keys":[` + strings.Join(keys, ",") + `]}`)
	defer func() { idp.jwksBody = previous }()

	if _, err := auth.NewOIDCAuthenticator(context.Background(), configFor(t, idp)); err == nil {
		t.Fatal("JWKS exceeding the supported key count was accepted")
	}
}

// TestInsecureMetadataURLsAreRefused pins HTTPS enforcement for secured
// runtimes: plain-HTTP issuers are refused outright, and a compromised or
// misconfigured discovery document advertising an HTTP jwks_uri is refused.
func TestInsecureMetadataURLsAreRefused(t *testing.T) {
	idp := newTestIDP(t)
	httpIssuer := strings.Replace(idp.issuer, "https://", "http://", 1)

	config := configFor(t, idp)
	config.Issuer = httpIssuer
	if _, err := auth.NewOIDCAuthenticator(context.Background(), config); err == nil {
		t.Fatal("HTTP issuer accepted in secured composition")
	}

	previous := idp.jwksURIOverride
	idp.jwksURIOverride = httpIssuer + "/jwks.json"
	defer func() { idp.jwksURIOverride = previous }()
	if _, err := auth.NewOIDCAuthenticator(context.Background(), configFor(t, idp)); err == nil {
		t.Fatal("discovery advertising an HTTP jwks_uri was accepted")
	}
}

func configFor(t *testing.T, idp *testIDP) auth.Config {
	t.Helper()
	return auth.Config{
		Issuer:     idp.issuer,
		Audience:   testAudience,
		Clock:      time.Now,
		HTTPClient: idp.client(),
	}
}

// TestStaleCacheForgedKidFloodIsRateLimited pins the adversarial review fix:
// even after the key cache ages past its TTL, cycling forged kids produces at
// most one JWKS fetch per rate window.
func TestStaleCacheForgedKidFloodIsRateLimited(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, func(config *auth.Config) {
		config.JWKSRefreshEvery = time.Nanosecond // instantly stale
	})

	valid := idp.token(t, "at+jwt", "RS256", idp.privateKey, nil)
	if _, err := authenticator.Authenticate(context.Background(), valid); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	fetchesAfterValid := idp.fetches

	forge := func(index int) string {
		return idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": "forged-" + string(rune('a'+index))},
			idp.defaultClaims(time.Now()), idp.privateKey)
	}
	for index := range 10 {
		if _, err := authenticator.Authenticate(context.Background(), forge(index)); err == nil {
			t.Fatalf("forged kid %d authenticated", index)
		}
	}
	if idp.fetches > fetchesAfterValid+1 {
		t.Fatalf("stale-cache forged-kid flood caused %d extra fetches; rate limit failed", idp.fetches-fetchesAfterValid)
	}

	// The aged-but-valid cached key keeps verifying within the rate window.
	if _, err := authenticator.Authenticate(context.Background(), valid); err != nil {
		t.Fatalf("aged valid token stopped verifying: %v", err)
	}
}
