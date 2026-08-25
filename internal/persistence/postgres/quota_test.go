// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestOwnerQuotaLockKeyFixedVectors(t *testing.T) {
	tests := []struct {
		owner         domain.OwnerRef
		first, second int32
	}{
		{domain.OwnerRef{Kind: "team", ID: "payments"}, -1497834922, 1328948451},
		{domain.OwnerRef{Kind: "user", ID: "alice"}, 1528191618, -1387437309},
	}
	for _, test := range tests {
		first, second := ownerQuotaLockKey(test.owner)
		if first != test.first || second != test.second {
			t.Fatalf("owner %+v key=(%d,%d), want (%d,%d)", test.owner, first, second, test.first, test.second)
		}
	}
}
