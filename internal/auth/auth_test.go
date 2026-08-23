// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
)

const testAudience = "liftr-api"

type testIDP struct {
	server     *httptest.Server
	issuer     string
	privateKey *rsa.PrivateKey
	otherKey   *rsa.PrivateKey
	keyID      string
	jwksBody   []byte
	fetches    int
	// jwksURIOverride lets tests advertise a nonstandard jwks_uri through the
	// discovery document.
	jwksURIOverride string
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	certificate := selfSignedCertificate(t, privateKey)
	idp := &testIDP{privateKey: privateKey, otherKey: otherKey, keyID: "test-key-1"}
	idp.jwksBody = idp.renderJWKS(privateKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		jwksURI := idp.issuer + "/jwks.json"
		if idp.jwksURIOverride != "" {
			jwksURI = idp.jwksURIOverride
		}
		fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, idp.issuer, jwksURI)
	})
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		idp.fetches++
		w.Header().Set("Content-Type", "application/json")
		w.Write(idp.jwksBody)
	})
	server := httptest.NewUnstartedServer(mux)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	idp.server = server
	idp.issuer = server.URL
	return idp
}

func (i *testIDP) client() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func (i *testIDP) renderJWKS(key *rsa.PrivateKey) []byte {
	body := fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":%q,"use":"sig","alg":"RS256","n":%q,"e":%q}]}`,
		i.keyID,
		base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	return []byte(body)
}

func selfSignedCertificate(t *testing.T, key *rsa.PrivateKey) tls.Certificate {
	t.Helper()
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "idp.test.example"},
		DNSNames:     []string{"idp.test.example", "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// token builds a signed compact JWT with full control over header and claims.
func (i *testIDP) token(t *testing.T, typ, alg string, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": alg, "kid": i.keyID}
	if typ != "" {
		header["typ"] = typ
	}
	if claims == nil {
		claims = i.defaultClaims(time.Now())
	}
	return i.seal(t, header, claims, key)
}

func (i *testIDP) seal(t *testing.T, header, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signingInput := encode(header) + "." + encode(claims)
	digest := crypto.SHA256.New()
	digest.Write([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func (i *testIDP) defaultClaims(now time.Time) map[string]any {
	return map[string]any{
		"iss":       i.issuer,
		"aud":       testAudience,
		"sub":       "alice",
		"client_id": "liftr-cli",
		"jti":       "token-" + fmt.Sprintf("%d", now.UnixNano()),
		"iat":       now.Unix(),
		"exp":       now.Add(time.Hour).Unix(),
		"groups":    []string{"team:payments"},
	}
}

func newAuthenticator(t *testing.T, idp *testIDP, mutate func(*auth.Config)) *auth.OIDCAuthenticator {
	t.Helper()
	now := time.Now
	config := auth.Config{
		Issuer:     idp.issuer,
		Audience:   testAudience,
		Clock:      func() time.Time { return now() },
		HTTPClient: idp.client(),
	}
	if mutate != nil {
		mutate(&config)
	}
	authenticator, err := auth.NewOIDCAuthenticator(context.Background(), config)
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	return authenticator
}

func TestValidAccessTokenAuthenticates(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)
	principal, err := authenticator.Authenticate(context.Background(), idp.token(t, "at+jwt", "RS256", idp.privateKey, nil))
	if err != nil {
		t.Fatalf("Authenticate(valid at+jwt): %v", err)
	}
	if principal.Subject != "alice" || principal.Issuer != idp.issuer || principal.ID == "" {
		t.Fatalf("principal = %+v", principal)
	}
	if !principal.IsMember(domain.OwnerRef{Kind: "team", ID: "payments"}) {
		t.Fatalf("group claim did not normalize into a typed membership: %+v", principal.Memberships)
	}
}

func TestApplicationAtJWTTypeIsAccepted(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)
	if _, err := authenticator.Authenticate(context.Background(), idp.token(t, "application/at+jwt", "RS256", idp.privateKey, nil)); err != nil {
		t.Fatalf("application/at+jwt must be accepted: %v", err)
	}
}

func TestCredentialFailureMatrixIsRejected(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)
	now := time.Now()

	valid := func(mutate func(map[string]any)) string {
		claims := idp.defaultClaims(now)
		if mutate != nil {
			mutate(claims)
		}
		return idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": idp.keyID}, claims, idp.privateKey)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"missing typ", idp.token(t, "", "RS256", idp.privateKey, nil)},
		{"generic JWT typ (ID-token-like)", idp.token(t, "JWT", "RS256", idp.privateKey, nil)},
		{"arbitrary typ", idp.token(t, "vendor+jwt", "RS256", idp.privateKey, nil)},
		{"none algorithm", unsignedToken(t, idp.defaultClaims(now))},
		{"HMAC algorithm not configured", hmacStyleHeader(t, idp.defaultClaims(now), idp.privateKey)},
		{"bad signature", idp.token(t, "at+jwt", "RS256", idp.otherKey, nil)},
		{"wrong issuer", valid(func(c map[string]any) { c["iss"] = "https://evil.example" })},
		{"wrong issuer but valid otherwise", func() string {
			claims := idp.defaultClaims(now)
			claims["iss"] = strings.TrimSuffix(idp.issuer, "/") + "/"
			return idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": idp.keyID}, claims, idp.privateKey)
		}()},
		{"wrong audience", valid(func(c map[string]any) { c["aud"] = "other-audience" })},
		{"expired", valid(func(c map[string]any) { c["exp"] = now.Add(-time.Hour).Unix() })},
		{"future nbf", valid(func(c map[string]any) { c["nbf"] = now.Add(time.Hour).Unix() })},
		{"future iat", valid(func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() })},
		{"missing sub", valid(func(c map[string]any) { delete(c, "sub") })},
		{"empty sub", valid(func(c map[string]any) { c["sub"] = "" })},
		{"missing iat (RFC 9068 requires)", valid(func(c map[string]any) { delete(c, "iat") })},
		{"missing client_id (RFC 9068 requires)", valid(func(c map[string]any) { delete(c, "client_id") })},
		{"malformed compact", "not.a.jwt"},
		{"garbage", "totally-invalid"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := authenticator.Authenticate(context.Background(), test.token); err == nil {
				t.Fatal("credential was accepted but must be rejected")
			}
		})
	}

	// The cross-JWT confusion pin: an ID-token-like JWT that is validly
	// signed AND carries the Liftr audience still cannot authenticate.
	idTokenClaims := idp.defaultClaims(now)
	idTokenClaims["aud"] = []string{testAudience, idp.issuer + "/userinfo"}
	idTokenClaims["nonce"] = "n-0S6_WzA2Mj"
	idTokenLike := idp.seal(t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": idp.keyID}, idTokenClaims, idp.privateKey)
	if _, err := authenticator.Authenticate(context.Background(), idTokenLike); err == nil {
		t.Fatal("validly signed ID-token-shaped token authenticated; token typing failed")
	}
}

func unsignedToken(t *testing.T, claims map[string]any) string {
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return encode(map[string]any{"alg": "none", "typ": "at+jwt"}) + "." + encode(claims) + "."
}

func hmacStyleHeader(t *testing.T, claims map[string]any, key *rsa.PrivateKey) string {
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signingInput := encode(map[string]any{"alg": "HS256", "typ": "at+jwt"}) + "." + encode(claims)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
}

func TestNumericDateOverflowIsRejected(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)
	now := time.Now()
	for _, hostileExp := range []float64{1e30, -1e30} {
		claims := idp.defaultClaims(now)
		claims["exp"] = hostileExp
		token := idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": idp.keyID}, claims, idp.privateKey)
		if _, err := authenticator.Authenticate(context.Background(), token); err == nil {
			t.Fatalf("exp %v authenticated; numeric overflow not guarded", hostileExp)
		}
	}
}

// TestJTIProfileValidation pins the RFC 9068 required-claim semantics for
// jti: present, a JSON string, non-empty, and length-bounded. A conformant
// token authenticates; every deviation is refused. The value is validation
// input only and never travels into the principal (ADR-0012).
func TestJTIProfileValidation(t *testing.T) {
	idp := newTestIDP(t)
	authenticator := newAuthenticator(t, idp, nil)
	now := time.Now()

	build := func(mutate func(claims map[string]any)) string {
		claims := idp.defaultClaims(now)
		if mutate != nil {
			mutate(claims)
		}
		return idp.seal(t, map[string]any{"alg": "RS256", "typ": "at+jwt", "kid": idp.keyID}, claims, idp.privateKey)
	}
	const marker = "jti-marker-3f2b9c"

	// A. Valid RFC 9068 token carrying jti authenticates.
	valid := build(func(claims map[string]any) { claims["jti"] = marker })
	principal, err := authenticator.Authenticate(context.Background(), valid)
	if err != nil {
		t.Fatalf("valid token with jti rejected: %v", err)
	}

	// B. Otherwise valid token missing jti is refused.
	if _, err := authenticator.Authenticate(context.Background(),
		build(func(claims map[string]any) { delete(claims, "jti") })); err == nil {
		t.Fatal("token without jti authenticated")
	}

	// C. Wrong JSON type for jti is refused.
	for _, wrong := range []any{123, true, []string{marker}, map[string]any{"v": marker}, nil} {
		if _, err := authenticator.Authenticate(context.Background(),
			build(func(claims map[string]any) { claims["jti"] = wrong })); err == nil {
			t.Fatalf("wrong-type jti %v authenticated", wrong)
		}
	}

	// D. Empty jti is refused.
	if _, err := authenticator.Authenticate(context.Background(),
		build(func(claims map[string]any) { claims["jti"] = "" })); err == nil {
		t.Fatal("empty jti authenticated")
	}
	if _, err := authenticator.Authenticate(context.Background(),
		build(func(claims map[string]any) { claims["jti"] = "   " })); err == nil {
		t.Fatal("whitespace-only jti authenticated")
	}

	// E. Oversized jti is refused.
	oversized := build(func(claims map[string]any) { claims["jti"] = strings.Repeat("x", 256) })
	if _, err := authenticator.Authenticate(context.Background(), oversized); err == nil {
		t.Fatal("oversized jti authenticated")
	}
	atLimit := build(func(claims map[string]any) { claims["jti"] = strings.Repeat("x", 255) })
	if _, err := authenticator.Authenticate(context.Background(), atLimit); err != nil {
		t.Fatalf("255-character jti must be accepted: %v", err)
	}

	// F. jti never reaches the principal: the distinctive marker value must
	// not appear anywhere in the normalized identity.
	rendered := fmt.Sprintf("%+v", principal)
	if strings.Contains(rendered, marker) {
		t.Fatal("jti leaked into the Principal representation")
	}
}
