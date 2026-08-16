CREATE TABLE resources (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    type_name text NOT NULL CHECK (btrim(type_name) <> ''),
    type_version text NOT NULL CHECK (btrim(type_version) <> ''),
    owner_kind text NOT NULL CHECK (btrim(owner_kind) <> ''),
    owner_id text NOT NULL CHECK (btrim(owner_id) <> ''),
    generation numeric(20,0) NOT NULL CHECK (generation > 0 AND generation <= 18446744073709551615),
    spec_codec_version integer NOT NULL CHECK (spec_codec_version > 0),
    spec jsonb NOT NULL,
    record_version numeric(20,0) NOT NULL CHECK (record_version > 0 AND record_version <= 18446744073709551615),
    created_at_ns bigint NOT NULL,
    updated_at_ns bigint NOT NULL CHECK (updated_at_ns >= created_at_ns),
    UNIQUE (id, type_name, type_version)
);

CREATE TABLE resource_statuses (
    resource_id text PRIMARY KEY REFERENCES resources(id) ON DELETE RESTRICT,
    observed_generation numeric(20,0) NOT NULL CHECK (observed_generation >= 0 AND observed_generation <= 18446744073709551615),
    state text NOT NULL CHECK (state IN ('Unknown', 'Pending', 'Ready', 'Deleting', 'Deleted', 'Failed')),
    updated_at_ns bigint NOT NULL
);

CREATE TABLE resource_conditions (
    resource_id text NOT NULL REFERENCES resource_statuses(resource_id) ON DELETE CASCADE,
    type text NOT NULL CHECK (btrim(type) <> ''),
    status text NOT NULL CHECK (status IN ('True', 'False', 'Unknown')),
    reason text NOT NULL CHECK (btrim(reason) <> ''),
    message text NOT NULL,
    observed_generation numeric(20,0) NOT NULL CHECK (observed_generation >= 0 AND observed_generation <= 18446744073709551615),
    last_transition_at_ns bigint NOT NULL,
    PRIMARY KEY (resource_id, type)
);

CREATE TABLE provisioner_bindings (
    resource_id text PRIMARY KEY REFERENCES resources(id) ON DELETE RESTRICT,
    provisioner_ref text NOT NULL CHECK (btrim(provisioner_ref) <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (resource_id, provisioner_ref)
);

CREATE TABLE operations (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    resource_id text NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    capability text NOT NULL CHECK (capability IN ('create', 'update', 'delete')),
    target_generation numeric(20,0) NOT NULL CHECK (target_generation > 0 AND target_generation <= 18446744073709551615),
    state text NOT NULL CHECK (state IN ('Pending', 'Running', 'Succeeded', 'Failed', 'Canceled')),
    phase text NOT NULL CHECK (phase IN ('Requested', 'Validating', 'Planning', 'Applying', 'Destroying')),
    requested_at_ns bigint NOT NULL,
    started_at_ns bigint,
    phase_changed_at_ns bigint NOT NULL,
    completed_at_ns bigint,
    failure_reason text,
    failure_message text,
    record_version numeric(20,0) NOT NULL CHECK (record_version > 0 AND record_version <= 18446744073709551615),
    UNIQUE (id, resource_id),
    UNIQUE (id, resource_id, capability, target_generation),
    CHECK (state <> 'Pending' OR started_at_ns IS NULL),
    CHECK (state NOT IN ('Running', 'Succeeded') OR started_at_ns IS NOT NULL),
    CHECK ((state IN ('Succeeded', 'Failed', 'Canceled')) = (completed_at_ns IS NOT NULL)),
    CHECK ((state = 'Failed') = (failure_reason IS NOT NULL))
);

CREATE UNIQUE INDEX operations_one_active_per_resource
    ON operations(resource_id) WHERE state IN ('Pending', 'Running');
CREATE INDEX operations_resource_created ON operations(resource_id, requested_at_ns DESC);

CREATE TABLE events (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    resource_id text NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    operation_id text,
    generation numeric(20,0) NOT NULL CHECK (generation > 0 AND generation <= 18446744073709551615),
    type text NOT NULL CHECK (btrim(type) <> ''),
    reason text NOT NULL CHECK (btrim(reason) <> ''),
    message text NOT NULL,
    occurred_at_ns bigint NOT NULL,
    data jsonb,
    FOREIGN KEY (operation_id, resource_id) REFERENCES operations(id, resource_id) ON DELETE RESTRICT
);
CREATE INDEX events_resource_occurred ON events(resource_id, occurred_at_ns);
CREATE INDEX events_operation_occurred ON events(operation_id, occurred_at_ns) WHERE operation_id IS NOT NULL;

CREATE TABLE provisioning_executions (
    operation_id text PRIMARY KEY,
    resource_id text NOT NULL,
    provisioner_ref text NOT NULL,
    resource_type_name text NOT NULL,
    resource_type_version text NOT NULL,
    capability text NOT NULL CHECK (capability IN ('create', 'update', 'delete')),
    target_generation numeric(20,0) NOT NULL CHECK (target_generation > 0 AND target_generation <= 18446744073709551615),
    spec_codec_version integer NOT NULL CHECK (spec_codec_version > 0),
    submitted_spec jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('Pending', 'Dispatching', 'Accepted', 'Succeeded', 'Failed', 'Unknown')),
    handle text,
    acceptance_confirmed boolean NOT NULL DEFAULT false,
    correlation_status text NOT NULL DEFAULT 'Unknown' CHECK (correlation_status IN ('Found', 'NotFound', 'Unknown')),
    submission jsonb,
    latest_observation jsonb,
    last_observed_at_ns bigint,
    last_failure_kind text,
    last_failure_reason text,
    last_failure_message text,
    current_attempt_number numeric(20,0) NOT NULL DEFAULT 0 CHECK (current_attempt_number >= 0),
    next_observation_sequence numeric(20,0) NOT NULL DEFAULT 1 CHECK (next_observation_sequence > 0),
    record_version numeric(20,0) NOT NULL CHECK (record_version > 0 AND record_version <= 18446744073709551615),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (operation_id, resource_id) REFERENCES operations(id, resource_id) ON DELETE RESTRICT,
    FOREIGN KEY (operation_id, resource_id, capability, target_generation) REFERENCES operations(id, resource_id, capability, target_generation) ON DELETE RESTRICT,
    FOREIGN KEY (resource_id, provisioner_ref) REFERENCES provisioner_bindings(resource_id, provisioner_ref) ON DELETE RESTRICT,
    FOREIGN KEY (resource_id, resource_type_name, resource_type_version) REFERENCES resources(id, type_name, type_version) ON DELETE RESTRICT
);

CREATE TABLE provisioning_submission_attempts (
    operation_id text NOT NULL REFERENCES provisioning_executions(operation_id) ON DELETE RESTRICT,
    attempt_number numeric(20,0) NOT NULL CHECK (attempt_number > 0),
    state text NOT NULL CHECK (state IN ('Pending', 'Leased', 'Accepted', 'Rejected', 'NotFound', 'Unknown')),
    dispatch_message_id text NOT NULL UNIQUE,
    claimed_at timestamptz,
    resolved_at timestamptz,
    failure_kind text,
    failure_reason text,
    failure_message text,
    PRIMARY KEY (operation_id, attempt_number),
    UNIQUE (operation_id, attempt_number, dispatch_message_id),
    CHECK ((state IN ('Accepted', 'Rejected', 'NotFound')) = (resolved_at IS NOT NULL))
);

CREATE TABLE idempotency_records (
    scope text NOT NULL CHECK (btrim(scope) <> ''),
    idempotency_key text NOT NULL CHECK (btrim(idempotency_key) <> ''),
    command_kind text NOT NULL CHECK (btrim(command_kind) <> ''),
    fingerprint bytea NOT NULL,
    resource_id text NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    operation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (scope, idempotency_key),
    FOREIGN KEY (operation_id, resource_id) REFERENCES operations(id, resource_id) ON DELETE RESTRICT
);

CREATE TABLE outbox_messages (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    kind text NOT NULL CHECK (kind IN ('Drive', 'Dispatch', 'Observe', 'PassiveObserve')),
    operation_id text REFERENCES operations(id) ON DELETE RESTRICT,
    resource_id text REFERENCES resources(id) ON DELETE RESTRICT,
    attempt_number numeric(20,0),
    dedupe_key text NOT NULL UNIQUE CHECK (btrim(dedupe_key) <> ''),
    expected_version numeric(20,0) CHECK (expected_version >= 0 AND expected_version <= 18446744073709551615),
    sequence numeric(20,0) CHECK (sequence > 0),
    payload_version integer NOT NULL DEFAULT 1 CHECK (payload_version > 0),
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    state text NOT NULL DEFAULT 'Pending' CHECK (state IN ('Pending', 'Leased', 'Completed', 'Dead')),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    lease_token text,
    leased_until timestamptz,
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error text,
    terminal_reason text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    dead_at timestamptz,
    CHECK (
        (kind IN ('Drive', 'Dispatch', 'Observe') AND operation_id IS NOT NULL AND resource_id IS NULL) OR
        (kind = 'PassiveObserve' AND operation_id IS NULL AND resource_id IS NOT NULL)
    ),
    CHECK ((kind = 'Dispatch') = (attempt_number IS NOT NULL)),
    CHECK ((state = 'Leased') = (lease_token IS NOT NULL AND leased_until IS NOT NULL)),
    CHECK (state = 'Leased' OR (lease_token IS NULL AND leased_until IS NULL)),
    CHECK ((state = 'Completed') = (completed_at IS NOT NULL)),
    CHECK ((state = 'Dead') = (dead_at IS NOT NULL)),
    FOREIGN KEY (operation_id, attempt_number, id) REFERENCES provisioning_submission_attempts(operation_id, attempt_number, dispatch_message_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX outbox_one_active_drive_per_operation
    ON outbox_messages(operation_id) WHERE kind = 'Drive' AND state IN ('Pending', 'Leased');
CREATE UNIQUE INDEX outbox_one_active_dispatch_per_operation
    ON outbox_messages(operation_id) WHERE kind = 'Dispatch' AND state IN ('Pending', 'Leased');
CREATE UNIQUE INDEX outbox_one_active_observe_per_operation
    ON outbox_messages(operation_id) WHERE kind = 'Observe' AND state IN ('Pending', 'Leased');
CREATE UNIQUE INDEX outbox_one_active_passive_observe_per_resource
    ON outbox_messages(resource_id) WHERE kind = 'PassiveObserve' AND state IN ('Pending', 'Leased');
CREATE INDEX outbox_claimable ON outbox_messages(available_at, created_at) WHERE state = 'Pending';
CREATE INDEX outbox_expired_leases ON outbox_messages(leased_until) WHERE state = 'Leased';

CREATE FUNCTION liftr_reject_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is immutable', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER events_are_append_only
    BEFORE UPDATE OR DELETE ON events FOR EACH ROW EXECUTE FUNCTION liftr_reject_mutation();

CREATE FUNCTION liftr_protect_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.resource_id IS DISTINCT FROM NEW.resource_id OR OLD.provisioner_ref IS DISTINCT FROM NEW.provisioner_ref THEN
        RAISE EXCEPTION 'provisioner binding identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provisioner_binding_identity_is_immutable
    BEFORE UPDATE ON provisioner_bindings FOR EACH ROW EXECUTE FUNCTION liftr_protect_binding();

CREATE FUNCTION liftr_protect_resource_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.id IS DISTINCT FROM NEW.id OR OLD.type_name IS DISTINCT FROM NEW.type_name OR OLD.type_version IS DISTINCT FROM NEW.type_version THEN
        RAISE EXCEPTION 'resource identity and type are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER resource_identity_is_immutable
    BEFORE UPDATE ON resources FOR EACH ROW EXECUTE FUNCTION liftr_protect_resource_identity();

CREATE FUNCTION liftr_protect_operation_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.id IS DISTINCT FROM NEW.id OR OLD.resource_id IS DISTINCT FROM NEW.resource_id OR
       OLD.capability IS DISTINCT FROM NEW.capability OR OLD.target_generation IS DISTINCT FROM NEW.target_generation OR
       OLD.requested_at_ns IS DISTINCT FROM NEW.requested_at_ns THEN
        RAISE EXCEPTION 'operation intent is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER operation_intent_is_immutable
    BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION liftr_protect_operation_intent();

CREATE FUNCTION liftr_protect_execution_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.operation_id IS DISTINCT FROM NEW.operation_id OR OLD.resource_id IS DISTINCT FROM NEW.resource_id OR
       OLD.provisioner_ref IS DISTINCT FROM NEW.provisioner_ref OR OLD.resource_type_name IS DISTINCT FROM NEW.resource_type_name OR
       OLD.resource_type_version IS DISTINCT FROM NEW.resource_type_version OR OLD.capability IS DISTINCT FROM NEW.capability OR
       OLD.target_generation IS DISTINCT FROM NEW.target_generation OR OLD.spec_codec_version IS DISTINCT FROM NEW.spec_codec_version OR
       OLD.submitted_spec IS DISTINCT FROM NEW.submitted_spec THEN
        RAISE EXCEPTION 'submitted execution intent is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER execution_intent_is_immutable
    BEFORE UPDATE ON provisioning_executions FOR EACH ROW EXECUTE FUNCTION liftr_protect_execution_intent();

CREATE FUNCTION liftr_protect_terminal_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'submission attempt audit record is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.operation_id IS DISTINCT FROM NEW.operation_id OR OLD.attempt_number IS DISTINCT FROM NEW.attempt_number OR
       OLD.dispatch_message_id IS DISTINCT FROM NEW.dispatch_message_id THEN
        RAISE EXCEPTION 'submission attempt identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('Accepted', 'Rejected', 'NotFound') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'terminal submission attempt is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER terminal_submission_attempt_is_immutable
    BEFORE UPDATE OR DELETE ON provisioning_submission_attempts FOR EACH ROW EXECUTE FUNCTION liftr_protect_terminal_attempt();

CREATE FUNCTION liftr_protect_terminal_outbox() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('Completed', 'Dead') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'terminal outbox message is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER terminal_outbox_is_immutable
    BEFORE UPDATE ON outbox_messages FOR EACH ROW EXECUTE FUNCTION liftr_protect_terminal_outbox();

CREATE FUNCTION liftr_protect_terminal_outbox_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('Completed', 'Dead') THEN
        RAISE EXCEPTION 'terminal outbox message is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$;

CREATE TRIGGER terminal_outbox_cannot_be_deleted
    BEFORE DELETE ON outbox_messages FOR EACH ROW EXECUTE FUNCTION liftr_protect_terminal_outbox_delete();
