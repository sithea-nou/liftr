-- SPDX-License-Identifier: Apache-2.0

-- M20 operator diagnostics and safe recovery (ADR-0021).
--
-- operator_actions is the immutable audit of ACCEPTED privileged mutations.
-- Row existence is the entire persisted result semantics: one row means Liftr
-- durably accepted and scheduled exactly one recovery mutation. There is no
-- result column and no replay state: an idempotent replay returns the original
-- row untouched, and rejected attempts are deliberately never persisted.
--
-- created_work_id is a single typed foreign key into outbox_messages: every
-- approved M20 action creates at most ONE new work item, and the reference
-- preserves provenance without extending the hot-path outbox schema.
-- source_work_id points at the immutable Dead row being recovered; it is
-- required exactly when the action recovers dead work.

CREATE TABLE operator_actions (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    actor_principal_id text NOT NULL CHECK (btrim(actor_principal_id) <> ''),
    actor_kind text NOT NULL CHECK (actor_kind IN ('user', 'serviceAccount', 'system')),
    action text NOT NULL CHECK (action IN ('trigger_observe', 'trigger_passive_observe', 'recover_dead_work')),
    target_kind text NOT NULL CHECK (target_kind IN ('operation', 'resource', 'work')),
    target_id text NOT NULL CHECK (btrim(target_id) <> ''),
    source_work_id text REFERENCES outbox_messages(id),
    created_work_id text NOT NULL UNIQUE REFERENCES outbox_messages(id),
    idempotency_digest bytea NOT NULL CHECK (octet_length(idempotency_digest) = 32),
    request_id text NOT NULL CHECK (btrim(request_id) <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((action = 'trigger_observe') = (target_kind = 'operation')),
    CHECK ((action = 'trigger_passive_observe') = (target_kind = 'resource')),
    CHECK ((action = 'recover_dead_work') = ((target_kind = 'work') AND (source_work_id IS NOT NULL))),
    CHECK ((action = 'recover_dead_work') = (source_work_id IS NOT NULL)),
    CHECK (action <> 'recover_dead_work' OR source_work_id = target_id),
    CHECK (source_work_id IS NULL OR source_work_id <> created_work_id)
);

CREATE INDEX operator_actions_target ON operator_actions(target_kind, target_id, created_at);
CREATE INDEX operator_actions_created_work ON operator_actions(created_work_id);

CREATE FUNCTION liftr_operator_action_is_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'operator actions are append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER operator_actions_are_append_only
    BEFORE UPDATE OR DELETE ON operator_actions
    FOR EACH ROW EXECUTE FUNCTION liftr_operator_action_is_append_only();

-- Operator idempotency is a separate logical domain from developer admission
-- idempotency: keys are scoped by operator PrincipalID and bind to accepted
-- OperatorAction rows instead of Resource/Operation admissions. Only applied
-- mutations ever insert here, so a rejected request may be retried later with
-- the same key.

-- Bounded operator diagnostics aggregate work history per Operation/Resource.
-- Without these indexes the GROUP BY counts and active-set probes would scan
-- the entire outbox table regardless of which aggregate is inspected
-- (EXPLAIN evidence in the M20 review).
CREATE INDEX IF NOT EXISTS outbox_operation ON outbox_messages(operation_id);
CREATE INDEX IF NOT EXISTS outbox_resource ON outbox_messages(resource_id);

CREATE TABLE operator_idempotency (
    scope text NOT NULL CHECK (btrim(scope) <> ''),
    key text NOT NULL CHECK (btrim(key) <> '' AND octet_length(key) <= 200),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    operator_action_id text NOT NULL REFERENCES operator_actions(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (scope, key)
);

CREATE TRIGGER operator_idempotency_is_append_only
    BEFORE UPDATE OR DELETE ON operator_idempotency
    FOR EACH ROW EXECUTE FUNCTION liftr_operator_action_is_append_only();
