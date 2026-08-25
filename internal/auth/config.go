// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"net/http"
	"time"

	"github.com/sithea-nou/liftr/internal/identity"
)

// Observers receives typed, bounded operational events from the
// authentication boundary. Every field is optional; nil fields are silent.
// The hooks carry structural vocabulary only — never tokens, claims, or
// provider text — so composition-layer telemetry can render them as bounded
// metric labels and log fields without this package importing any telemetry
// library.
type Observers struct {
	// Authentication is invoked exactly once per Authenticate call with the
	// outcome and, on failure, the typed reason.
	Authentication func(success bool, reason identity.AuthFailureReason)
	// JWKSRefresh is invoked after each background key-set fetch attempt
	// triggered by cache aging or an unknown key ID.
	JWKSRefresh func(success bool, duration time.Duration)
	// ForcedRefreshLimited is invoked when the unknown-kid forced-refetch
	// rate window suppresses a refresh.
	ForcedRefreshLimited func()
}

// Config configures the OIDC access-token authenticator. Exactly one issuer
// is trusted; federation is explicitly out of scope in M11 (ADR-0012).
type Config struct {
	// Issuer is the exact issuer URL of the trusted identity provider. It
	// must be HTTPS in secured runtimes; discovery is fetched once at
	// startup from issuer + "/.well-known/openid-configuration".
	Issuer string
	// Audience is the Liftr API audience; tokens must carry it in aud.
	Audience string
	// Algorithms restricts acceptable JWS algorithms. Empty means ["RS256"].
	Algorithms []string
	// KindClaim optionally names the claim distinguishing service accounts
	// ("user" or "serviceAccount"). When empty every principal is KindUser;
	// kind is audit metadata in M11 and no policy branches on it.
	KindClaim string
	// Mapper maps the configured group claim plus optional static grants into
	// typed owner memberships. The zero value is usable: claim "groups", no
	// prefix strip, no static grants, default bounds.
	Mapper ClaimMapper
	// ClockSkew bounds time-claim tolerance. Zero means 30 seconds.
	ClockSkew time.Duration
	// JWKSRefreshEvery bounds how long fetched signing keys stay trusted
	// without refetch. Zero means 15 minutes.
	JWKSRefreshEvery time.Duration
	// Clock lets tests pin time. Production composition leaves it nil.
	Clock func() time.Time
	// HTTPClient lets tests inject a deterministic client. Production
	// composition leaves it nil to receive the bounded default client.
	HTTPClient *http.Client
	// Observers optionally receives typed operational events. Production
	// composition wires the telemetry recorder here; tests leave it zero.
	Observers Observers
}
