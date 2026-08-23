// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// httpClient is the bounded transport behavior for discovery and JWKS
// fetching: one short timeout, no redirects (a redirect could silently move
// trust to another origin), and response-size caps enforced by callers.
type httpClient struct {
	client http.Client
}

func newHTTPClient() *httpClient {
	return &httpClient{client: http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirects are not followed")
		},
	}}
}

// getJSON fetches one JSON document with a hard size cap. Non-200 answers
// and truncated bodies are rejected; error text is generic so nothing about
// the IdP's responses can leak through credentials failures.
func (c *httpClient) getJSON(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, invalid("metadata URL")
	}
	if parsed.Scheme != "https" {
		return nil, invalid("metadata URL scheme")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, invalid("metadata request")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, invalid("metadata fetch failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, invalid("metadata fetch status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return nil, invalid("metadata size")
	}
	return body, nil
}

type discoveryDocument struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// jwk carries exactly the fields needed to verify signatures. Unknown fields
// are ignored; unusable keys are skipped at selection time.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// keySet is an immutable snapshot of fetched verification keys.
type keySet struct {
	fetchedAt time.Time
	keys      map[string]verificationKey // keyed by kid; single-key sets use ""
}

// verificationKey pairs a parsed public key with its JWS algorithm.
type verificationKey struct {
	algorithm string
	rsa       *rsaPublicKey
	ec        *ecPublicKey
}

// OIDCAuthenticator verifies RFC 9068 JWT access tokens against one trusted
// issuer and produces normalized principals.
type OIDCAuthenticator struct {
	issuer      string
	audience    string
	algorithms  map[string]struct{}
	kindClaim   string
	mapper      ClaimMapper
	skew        time.Duration
	cacheTTL    time.Duration
	maxJWKSKeys int

	mu                sync.Mutex
	http              *httpClient
	jwksURI           string
	current           *keySet
	lastForcedRefresh time.Time
	now               func() time.Time
}

// NewOIDCAuthenticator validates configuration, fetches the issuer discovery
// document once at startup, and loads the initial key set. A failure here is
// a startup failure: secured runtimes never boot with unverifiable trust
// configuration (ADR-0012).
func NewOIDCAuthenticator(ctx context.Context, config Config) (*OIDCAuthenticator, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	authenticator := &OIDCAuthenticator{
		issuer:      config.Issuer,
		audience:    config.Audience,
		algorithms:  config.algorithmAllowlist(),
		kindClaim:   config.KindClaim,
		mapper:      config.Mapper.withDefaults(),
		skew:        defaultClockSkew,
		cacheTTL:    defaultJWKSRefreshEvery,
		maxJWKSKeys: defaultMaxJWKSKeys,
		http:        newHTTPClient(),
		now:         time.Now,
	}
	if config.ClockSkew > 0 {
		authenticator.skew = config.ClockSkew
	}
	if config.JWKSRefreshEvery > 0 {
		authenticator.cacheTTL = config.JWKSRefreshEvery
	}
	if config.Clock != nil {
		authenticator.now = config.Clock
	}
	if config.HTTPClient != nil {
		authenticator.http = &httpClient{client: *config.HTTPClient}
	}
	document, err := authenticator.fetchDiscovery(ctx)
	if err != nil {
		return nil, err
	}
	keySet, err := authenticator.fetchKeySet(ctx, document.JWKSURI)
	if err != nil {
		return nil, err
	}
	authenticator.jwksURI = document.JWKSURI
	authenticator.current = keySet
	return authenticator, nil
}

// Config validation ---------------------------------------------------------

func (c Config) validate() error {
	if strings.TrimSpace(c.Issuer) == "" {
		return fmt.Errorf("auth issuer is required")
	}
	parsed, err := url.Parse(c.Issuer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("auth issuer %q is not an absolute URL", c.Issuer)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("auth issuer must use HTTPS in secured runtimes")
	}
	if parsed.Fragment != "" || parsed.RawQuery != "" {
		return fmt.Errorf("auth issuer must not carry query or fragment")
	}
	if strings.TrimSpace(c.Audience) == "" {
		return fmt.Errorf("auth audience is required")
	}
	for _, algorithm := range c.Algorithms {
		if _, ok := supportedAlgorithms[algorithm]; !ok {
			return fmt.Errorf("auth algorithm %q is not supported", algorithm)
		}
	}
	return nil
}

func (c Config) algorithmAllowlist() map[string]struct{} {
	if len(c.Algorithms) == 0 {
		return map[string]struct{}{"RS256": {}}
	}
	allowlist := make(map[string]struct{}, len(c.Algorithms))
	for _, algorithm := range c.Algorithms {
		allowlist[algorithm] = struct{}{}
	}
	return allowlist
}

// Metadata fetching ----------------------------------------------------------

func (a *OIDCAuthenticator) fetchDiscovery(ctx context.Context) (*discoveryDocument, error) {
	wellKnown := strings.TrimSuffix(a.issuer, "/") + "/.well-known/openid-configuration"
	body, err := a.http.getJSON(ctx, wellKnown, defaultFetchBytes)
	if err != nil {
		return nil, fmt.Errorf("issuer discovery failed at startup: %w", err)
	}
	var document discoveryDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("issuer discovery document is malformed")
	}
	if document.Issuer != a.issuer {
		return nil, fmt.Errorf("discovery issuer does not match the configured issuer")
	}
	if document.JWKSURI == "" {
		return nil, fmt.Errorf("discovery document has no jwks_uri")
	}
	parsed, err := url.Parse(document.JWKSURI)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("jwks_uri must be an absolute HTTPS URL")
	}
	return &document, nil
}

func (a *OIDCAuthenticator) fetchKeySet(ctx context.Context, jwksURI string) (*keySet, error) {
	body, err := a.http.getJSON(ctx, jwksURI, defaultFetchBytes)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch failed: %w", err)
	}
	var document jwksDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("jwks document is malformed")
	}
	if len(document.Keys) == 0 {
		return nil, fmt.Errorf("jwks document has no keys")
	}
	if len(document.Keys) > a.maxJWKSKeys {
		return nil, fmt.Errorf("jwks document exceeds the supported key count")
	}
	set := &keySet{fetchedAt: a.now(), keys: make(map[string]verificationKey, len(document.Keys))}
	for _, candidate := range document.Keys {
		if candidate.Use != "" && candidate.Use != "sig" {
			continue
		}
		var key verificationKey
		switch candidate.Kty {
		case "RSA":
			public, alg, err := parseRSAJWK(candidate)
			if err != nil {
				continue
			}
			key = verificationKey{algorithm: alg, rsa: public}
		case "EC":
			public, err := parseECJWK(candidate)
			if err != nil {
				continue
			}
			key = verificationKey{algorithm: "ES256", ec: public}
		default:
			continue
		}
		if _, allowed := a.algorithms[key.algorithm]; !allowed {
			continue
		}
		set.keys[candidate.Kid] = key
	}
	return set, nil
}

// verificationKeyFor resolves the token's signing key with strictly bounded
// JWKS fetching: at most one forced refetch per rate window, whatever the
// request pattern. A matching cached key verifies regardless of cache age —
// TTL expresses freshness preference, never trust revocation — so forged-kid
// floods cannot loop and temporary IdP outages cannot invalidate already
// cached keys (ADR-0012). Key rotation is picked up through the unknown-kid
// path or the next opportunistic refresh after the cache ages past its TTL.
func (a *OIDCAuthenticator) verificationKeyFor(ctx context.Context, kid, algorithm string) (verificationKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	set := a.current
	if set != nil && !a.stale(set) {
		if resolved, ok := selectKey(set, kid, algorithm); ok {
			return resolved, nil
		}
	}
	// Either the cache aged past its TTL (refresh opportunistically) or the
	// kid is unknown (refresh once). Both share one rate window.
	now := a.now()
	if !a.lastForcedRefresh.IsZero() && now.Sub(a.lastForcedRefresh) < minForcedRefreshInterval {
		if set != nil {
			if resolved, ok := selectKey(set, kid, algorithm); ok {
				return resolved, nil
			}
		}
		return verificationKey{}, invalid("unknown signing key")
	}
	a.lastForcedRefresh = now
	fetched, err := a.fetchKeySet(ctx, a.jwksURI)
	if err != nil {
		// Availability first: an aged-but-valid cache keeps verifying while
		// the IdP recovers; the next attempt retries after the window.
		if set != nil {
			if resolved, ok := selectKey(set, kid, algorithm); ok {
				return resolved, nil
			}
		}
		return verificationKey{}, invalid("signing key refresh failed")
	}
	a.current = fetched
	if resolved, ok := selectKey(fetched, kid, algorithm); ok {
		return resolved, nil
	}
	return verificationKey{}, invalid("unknown signing key")
}

// stale reports whether a key set has aged past its refresh preference. It
// never gates verification of a matching key; see verificationKeyFor.
func (a *OIDCAuthenticator) stale(set *keySet) bool {
	return a.now().Sub(set.fetchedAt) > a.cacheTTL
}

// selectKey resolves a token's signing key: by exact kid when present, or by
// the set's single key when the token omits kid entirely (RFC 9068 makes kid
// optional). Ambiguous or unknown selections fail.
func selectKey(set *keySet, kid, algorithm string) (verificationKey, bool) {
	if kid != "" {
		key, ok := set.keys[kid]
		return key, ok && key.algorithm == algorithm
	}
	if len(set.keys) == 1 {
		for _, key := range set.keys {
			return key, key.algorithm == algorithm
		}
	}
	return verificationKey{}, false
}
