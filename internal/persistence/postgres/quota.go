// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

const ownerQuotaLockNamespace = "liftr/quota-owner/v1"

// LockOwnerQuota acquires the one M18 quota lock for the actual authorized
// owner. PostgreSQL's two-int32 advisory namespace is disjoint from existing
// single-bigint Resource and idempotency locks.
func (r *repositories) LockOwnerQuota(ctx context.Context, owner domain.OwnerRef) error {
	first, second := ownerQuotaLockKey(owner)
	if _, err := r.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::integer,$2::integer)`, first, second); err != nil {
		return translateError(err)
	}
	return nil
}

// ResourceCountFacts must run in a statement after LockOwnerQuota. The LEFT
// JOIN and missing-status aggregate make corruption fail closed: a retained
// Resource can never disappear from quota merely because its status is absent.
func (r *repositories) ResourceCountFacts(ctx context.Context, owner domain.OwnerRef, ref domain.ResourceTypeRef) (application.ResourceCountFacts, error) {
	var ownerCount, typeCount, missingStatus uint64
	err := r.tx.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE s.state <> 'Deleted')::bigint,
		count(*) FILTER (WHERE s.state <> 'Deleted' AND r.type_name=$3 AND r.type_version=$4)::bigint,
		count(*) FILTER (WHERE s.resource_id IS NULL)::bigint
		FROM resources r
		LEFT JOIN resource_statuses s ON s.resource_id=r.id
		WHERE r.owner_kind=$1 AND r.owner_id=$2`, owner.Kind, owner.ID, ref.Name, ref.Version).Scan(&ownerCount, &typeCount, &missingStatus)
	if err != nil {
		return application.ResourceCountFacts{}, translateError(err)
	}
	if missingStatus != 0 {
		return application.ResourceCountFacts{}, fmt.Errorf("%w: owner has %d retained Resources without durable status", application.ErrQuotaInvariant, missingStatus)
	}
	return application.ResourceCountFacts{
		Available: true, Owner: owner, ResourceType: ref,
		OwnerNonDeleted: ownerCount, TypeNonDeleted: typeCount,
	}, nil
}

func ownerQuotaLockKey(owner domain.OwnerRef) (int32, int32) {
	hash := sha256.New()
	writeLockField(hash, ownerQuotaLockNamespace)
	writeLockField(hash, owner.Kind)
	writeLockField(hash, owner.ID)
	digest := hash.Sum(nil)
	return int32(binary.BigEndian.Uint32(digest[:4])), int32(binary.BigEndian.Uint32(digest[4:8]))
}

type lockHash interface {
	Write([]byte) (int, error)
}

func writeLockField(hash lockHash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}
