// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// resourceListColumns selects summary fields only. The spec document and the
// condition table are deliberately never read here: inventory summaries
// cannot disclose them because they are not loaded (ADR-0016). The lateral
// join projects exactly the four latest-Operation fields the summary carries.
const resourceListSelect = `SELECT r.id, r.type_name, r.type_version, r.owner_kind, r.owner_id,
	r.generation::text, r.created_at_ns, r.updated_at_ns, r.resource_seq::text,
	s.state, s.observed_generation::text, s.updated_at_ns,
	latest.id, latest.capability, latest.state, latest.target_generation::text
	FROM resources r
	JOIN resource_statuses s ON s.resource_id = r.id
	LEFT JOIN LATERAL (
		SELECT latest_op.id, latest_op.capability, latest_op.state, latest_op.target_generation
		FROM operations latest_op
		WHERE latest_op.resource_id = r.id
		ORDER BY latest_op.operation_seq DESC
		LIMIT 1
	) latest ON true`

// resourceListPredicates applies every narrowing filter as a nullable
// parameter so the SQL text never expands with input size. $1/$2 carry the
// complete authorized owner set for restricted queries (ADR-0016).
const resourceListPredicates = `WHERE ($1::bigint IS NULL OR r.resource_seq < $1::bigint)
	AND ($2::boolean OR s.state <> 'Deleted')
	AND ($3::text IS NULL OR (r.owner_kind = $3::text AND r.owner_id = $4::text))
	AND ($5::text IS NULL OR r.type_name = $5::text)
	AND ($6::text IS NULL OR r.type_version = $6::text)
	AND ($7::text IS NULL OR s.state = $7::text)
	ORDER BY r.resource_seq DESC
	LIMIT $8::bigint`

// ListResources returns one keyset page ordered by the private immutable
// resource sequence, newest first. Restricted queries join the authorized
// owner set through bounded arrays — never dynamically expanded SQL text —
// so persistence evaluates no policy and receives no identity concepts.
func (r *repositories) ListResources(ctx context.Context, query application.ResourceListQuery) (application.ResourceInventoryPage, error) {
	if query.Limit <= 0 {
		return application.ResourceInventoryPage{}, application.ErrInvalidApplicationCall
	}
	if !query.Unrestricted && len(query.AllowedOwners) == 0 {
		// Authorized with empty visibility: a valid empty page without a
		// database round trip. The shape matches a scanned empty result.
		return application.ResourceInventoryPage{Items: []application.ResourceInventoryItem{}}, nil
	}

	var after any
	if query.AfterSequence != 0 {
		after = uintText(query.AfterSequence)
	}
	var ownerKind, ownerID any
	if query.OwnerFilter != nil {
		ownerKind, ownerID = query.OwnerFilter.Kind, query.OwnerFilter.ID
	}
	var typeName, typeVersion, stateFilter any
	if query.TypeName != "" {
		typeName = query.TypeName
	}
	if query.TypeVersion != "" {
		typeVersion = query.TypeVersion
	}
	if query.StateFilter != nil {
		stateFilter = string(*query.StateFilter)
	}
	includeDeleted := query.IncludeDeleted

	statement := resourceListSelect + `
	JOIN unnest($9::text[], $10::text[]) AS allowed(kind, id)
	ON r.owner_kind = allowed.kind AND r.owner_id = allowed.id
	` + resourceListPredicates
	args := func(limit int) []any {
		return []any{after, includeDeleted, ownerKind, ownerID, typeName, typeVersion, stateFilter,
			limit, ownerKindArray(query.AllowedOwners), ownerIDArray(query.AllowedOwners)}
	}
	if query.Unrestricted {
		statement = resourceListSelect + ` ` + resourceListPredicates
		args = func(limit int) []any {
			return []any{after, includeDeleted, ownerKind, ownerID, typeName, typeVersion, stateFilter, limit}
		}
	}

	rows, err := r.tx.Query(ctx, statement, args(query.Limit+1)...)
	if err != nil {
		return application.ResourceInventoryPage{}, translateError(err)
	}
	defer rows.Close()

	items := make([]application.ResourceInventoryItem, 0, query.Limit+1)
	for rows.Next() {
		item, err := scanInventoryItem(rows)
		if err != nil {
			return application.ResourceInventoryPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return application.ResourceInventoryPage{}, translateError(err)
	}

	page := application.ResourceInventoryPage{Items: items}
	if len(page.Items) > query.Limit {
		page.Items = page.Items[:query.Limit]
		page.NextSequence = page.Items[len(page.Items)-1].Sequence
	}
	return page, nil
}

type inventoryScanner interface {
	Scan(...any) error
}

// ownerKindArray and ownerIDArray split the authorized owner set into the
// two parallel text arrays consumed by the unnest join. The set is already
// normalized by the application, so positions correspond across arrays.
func ownerKindArray(owners []domain.OwnerRef) []string {
	values := make([]string, len(owners))
	for i, owner := range owners {
		values[i] = owner.Kind
	}
	return values
}

func ownerIDArray(owners []domain.OwnerRef) []string {
	values := make([]string, len(owners))
	for i, owner := range owners {
		values[i] = owner.ID
	}
	return values
}

func scanInventoryItem(row inventoryScanner) (application.ResourceInventoryItem, error) {
	var id, typeName, typeVersion, ownerKind, ownerID string
	var generationText string
	var createdNS, updatedNS int64
	var sequenceText, state, observedText string
	var statusUpdatedNS int64
	var latestID, capability, latestState, targetGeneration *string
	if err := row.Scan(&id, &typeName, &typeVersion, &ownerKind, &ownerID,
		&generationText, &createdNS, &updatedNS, &sequenceText,
		&state, &observedText, &statusUpdatedNS,
		&latestID, &capability, &latestState, &targetGeneration); err != nil {
		return application.ResourceInventoryItem{}, err
	}
	generation, err := parseUint64(generationText)
	if err != nil {
		return application.ResourceInventoryItem{}, err
	}
	observed, err := parseUint64(observedText)
	if err != nil {
		return application.ResourceInventoryItem{}, err
	}
	sequence, err := parseUint64(sequenceText)
	if err != nil {
		return application.ResourceInventoryItem{}, err
	}
	item := application.ResourceInventoryItem{
		ID:         domain.ResourceID(id),
		Type:       domain.ResourceTypeRef{Name: typeName, Version: typeVersion},
		Owner:      domain.OwnerRef{Kind: ownerKind, ID: ownerID},
		Generation: generation,
		CreatedAt:  time.Unix(0, createdNS).UTC(),
		UpdatedAt:  time.Unix(0, updatedNS).UTC(),
		Status: application.ResourceInventoryStatus{
			State:              domain.ResourceState(state),
			ObservedGeneration: observed,
			UpdatedAt:          time.Unix(0, statusUpdatedNS).UTC(),
		},
		Sequence: sequence,
	}
	if latestID != nil && capability != nil && latestState != nil && targetGeneration != nil {
		target, err := parseUint64(*targetGeneration)
		if err != nil {
			return application.ResourceInventoryItem{}, err
		}
		item.Latest = &application.ResourceInventoryLatestOperation{
			ID:               domain.OperationID(*latestID),
			Capability:       domain.Capability(*capability),
			State:            domain.OperationState(*latestState),
			TargetGeneration: target,
		}
	}
	return item, nil
}
