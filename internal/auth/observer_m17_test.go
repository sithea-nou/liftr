// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/identity"
)

type recordedAuthEvent struct {
	success bool
	reason  identity.AuthFailureReason
	limited bool
}

// The verifier reports typed structural failure reasons through its observer
// hook; the reasons are bounded vocabulary owned by the authentication
// boundary and are the only source of auth metric labels (ADR-0018).
func TestObserverReceivesTypedFailureReasons(t *testing.T) {
	idp := newTestIDP(t)
	defer idp.server.Close()

	var mu sync.Mutex
	var events []recordedAuthEvent

	authenticator := newAuthenticator(t, idp, func(config *auth.Config) {
		config.Observers = auth.Observers{
			Authentication: func(success bool, reason identity.AuthFailureReason) {
				mu.Lock()
				events = append(events, recordedAuthEvent{success: success, reason: reason})
				mu.Unlock()
			},
			JWKSRefresh: func(bool, time.Duration) {},
			ForcedRefreshLimited: func() {
				mu.Lock()
				events = append(events, recordedAuthEvent{limited: true})
				mu.Unlock()
			},
		}
	})
	ctx := context.Background()

	// Expired token.
	expired := idp.token(t, "at+jwt", "RS256", idp.privateKey, map[string]any{
		"iss": idp.issuer, "aud": testAudience, "sub": "alice", "client_id": "cli",
		"jti": "j1", "exp": float64(time.Now().Add(-time.Hour).Unix()), "iat": float64(time.Now().Add(-2 * time.Hour).Unix()),
	})
	if _, err := authenticator.Authenticate(ctx, expired); err == nil {
		t.Fatal("expired token authenticated")
	}
	// Wrong signature.
	forged := idp.seal(t,
		map[string]any{"typ": "at+jwt", "alg": "RS256", "kid": idp.keyID},
		map[string]any{"iss": idp.issuer, "aud": testAudience, "sub": "alice", "client_id": "cli",
			"jti": "j2", "exp": float64(time.Now().Add(time.Hour).Unix()), "iat": float64(time.Now().Unix())},
		idp.otherKey)
	if _, err := authenticator.Authenticate(ctx, forged); err == nil {
		t.Fatal("forged token authenticated")
	}
	// Success.
	valid := idp.token(t, "at+jwt", "RS256", idp.privateKey, nil)
	if _, err := authenticator.Authenticate(ctx, valid); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("events=%d (%+v), want exactly one per attempt", len(events), events)
	}
	if events[0].success || events[0].reason != identity.AuthFailureExpired {
		t.Fatalf("expired event=%+v", events[0])
	}
	if events[1].success || events[1].reason != identity.AuthFailureInvalidSignature {
		t.Fatalf("signature event=%+v", events[1])
	}
	if !events[2].success || events[2].reason != identity.AuthFailureNone {
		t.Fatalf("success event=%+v", events[2])
	}
}
