CREATE TABLE opentofu_attempt_evidence (
    resource_id text NOT NULL,
    operation_id text NOT NULL,
    attempt_number numeric(20,0) NOT NULL CHECK (
        attempt_number > 0 AND attempt_number <= 18446744073709551615
    ),
    provisioner_ref text NOT NULL CHECK (btrim(provisioner_ref) <> ''),
    phase text NOT NULL CHECK (phase IN (
        'Prepared', 'ApplyMayStart', 'ApplyExited', 'ApplyOutcomeUnknown', 'ObservedConverged'
    )),
    record_version numeric(20,0) NOT NULL CHECK (
        record_version > 0 AND record_version <= 18446744073709551615
    ),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (resource_id, operation_id, attempt_number, provisioner_ref),
    FOREIGN KEY (operation_id, resource_id)
        REFERENCES operations(id, resource_id) ON DELETE RESTRICT,
    FOREIGN KEY (operation_id, attempt_number)
        REFERENCES provisioning_submission_attempts(operation_id, attempt_number) ON DELETE RESTRICT,
    FOREIGN KEY (resource_id, provisioner_ref)
        REFERENCES provisioner_bindings(resource_id, provisioner_ref) ON DELETE RESTRICT
);

CREATE TABLE opentofu_state_bindings (
    resource_id text PRIMARY KEY REFERENCES resources(id) ON DELETE RESTRICT,
    provisioner_ref text NOT NULL CHECK (btrim(provisioner_ref) <> ''),
    engine text NOT NULL CHECK (btrim(engine) <> ''),
    program text NOT NULL CHECK (btrim(program) <> ''),
    backend text NOT NULL CHECK (btrim(backend) <> ''),
    state_key text NOT NULL CHECK (btrim(state_key) <> ''),
    lineage text,
    serial numeric(20,0),
    state_digest bytea,
    record_version numeric(20,0) NOT NULL CHECK (
        record_version > 0 AND record_version <= 18446744073709551615
    ),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (resource_id, provisioner_ref)
        REFERENCES provisioner_bindings(resource_id, provisioner_ref) ON DELETE RESTRICT,
    CHECK ((lineage IS NULL) = (serial IS NULL) AND (serial IS NULL) = (state_digest IS NULL)),
    CHECK (lineage IS NULL OR btrim(lineage) <> ''),
    CHECK (serial IS NULL OR (serial >= 0 AND serial <= 18446744073709551615)),
    CHECK (state_digest IS NULL OR octet_length(state_digest) = 32)
);

CREATE FUNCTION liftr_protect_opentofu_attempt_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.phase <> 'Prepared' OR NEW.record_version <> 1 THEN
            RAISE EXCEPTION 'OpenTofu attempt evidence must begin prepared at version one' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'OpenTofu attempt evidence cannot be deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.resource_id IS DISTINCT FROM NEW.resource_id OR
       OLD.operation_id IS DISTINCT FROM NEW.operation_id OR
       OLD.attempt_number IS DISTINCT FROM NEW.attempt_number OR
       OLD.provisioner_ref IS DISTINCT FROM NEW.provisioner_ref OR
       OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION 'OpenTofu attempt evidence identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.phase = 'ObservedConverged' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'converged OpenTofu attempt evidence is terminal' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.record_version <> OLD.record_version + 1 THEN
        RAISE EXCEPTION 'OpenTofu attempt evidence version must advance exactly once' USING ERRCODE = 'check_violation';
    END IF;
    IF NOT (
        (OLD.phase = 'Prepared' AND NEW.phase = 'ApplyMayStart') OR
        (OLD.phase = 'ApplyMayStart' AND NEW.phase IN ('ApplyExited', 'ApplyOutcomeUnknown')) OR
        (OLD.phase IN ('ApplyExited', 'ApplyOutcomeUnknown') AND NEW.phase = 'ObservedConverged')
    ) THEN
        RAISE EXCEPTION 'OpenTofu attempt phase must advance forward' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER opentofu_attempt_evidence_is_monotonic
    BEFORE INSERT OR UPDATE OR DELETE ON opentofu_attempt_evidence
    FOR EACH ROW EXECUTE FUNCTION liftr_protect_opentofu_attempt_evidence();

CREATE FUNCTION liftr_protect_opentofu_state_binding() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.record_version <> 1 THEN
            RAISE EXCEPTION 'OpenTofu state binding must begin at version one' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'OpenTofu state binding cannot be deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.resource_id IS DISTINCT FROM NEW.resource_id OR
       OLD.provisioner_ref IS DISTINCT FROM NEW.provisioner_ref OR
       OLD.engine IS DISTINCT FROM NEW.engine OR
       OLD.program IS DISTINCT FROM NEW.program OR
       OLD.backend IS DISTINCT FROM NEW.backend OR
       OLD.state_key IS DISTINCT FROM NEW.state_key OR
       OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION 'OpenTofu state binding identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.record_version <> OLD.record_version + 1 THEN
        RAISE EXCEPTION 'OpenTofu state binding version must advance exactly once' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lineage IS NOT NULL AND NEW.lineage IS DISTINCT FROM OLD.lineage THEN
        RAISE EXCEPTION 'OpenTofu state lineage cannot change or be erased' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.serial IS NOT NULL AND NEW.serial < OLD.serial THEN
        RAISE EXCEPTION 'OpenTofu state serial cannot regress' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.serial IS NOT NULL AND NEW.serial = OLD.serial AND NEW.state_digest IS DISTINCT FROM OLD.state_digest THEN
        RAISE EXCEPTION 'OpenTofu state digest conflicts at the same serial' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER opentofu_state_binding_is_immutable_and_monotonic
    BEFORE INSERT OR UPDATE OR DELETE ON opentofu_state_bindings
    FOR EACH ROW EXECUTE FUNCTION liftr_protect_opentofu_state_binding();
