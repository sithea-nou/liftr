ALTER TABLE provisioning_executions
    ADD COLUMN output_mapping_ref text NOT NULL DEFAULT '',
    ADD COLUMN output_resolution text NOT NULL DEFAULT 'None',
    ADD COLUMN output_failure_reason text,
    ADD COLUMN output_failure_message text;

ALTER TABLE provisioning_executions
    ADD CONSTRAINT executions_output_resolution_valid
    CHECK (output_resolution IN ('None', 'Pending', 'Published', 'Rejected'));

ALTER TABLE provisioning_executions
    ADD CONSTRAINT executions_output_rejection_details
    CHECK ((output_resolution = 'Rejected') = (output_failure_reason IS NOT NULL));

-- The durable output-mapping identity is assigned once, before any provider
-- work, and is immutable for the lifetime of the execution. Recovery resolves
-- decoders through this identity; silently rebinding an execution to a newer
-- mapping would corrupt provenance.
CREATE FUNCTION liftr_protect_output_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.output_mapping_ref IS DISTINCT FROM NEW.output_mapping_ref AND OLD.output_mapping_ref <> '' THEN
        RAISE EXCEPTION 'execution output mapping identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER execution_output_mapping_is_immutable
    BEFORE UPDATE ON provisioning_executions FOR EACH ROW EXECUTE FUNCTION liftr_protect_output_mapping();

-- Resource outputs are immutable generation-scoped snapshots. Each row is
-- tied to exactly one completing Operation through the composite Operation
-- uniqueness constraint, so capability and target generation provenance are
-- enforced by the schema itself.
CREATE TABLE resource_outputs (
    resource_id text NOT NULL REFERENCES resources(id) ON DELETE RESTRICT,
    observed_generation numeric(20,0) NOT NULL CHECK (observed_generation > 0 AND observed_generation <= 18446744073709551615),
    operation_id text NOT NULL UNIQUE,
    capability text NOT NULL CHECK (capability IN ('create', 'update')),
    output_mapping_ref text NOT NULL DEFAULT '',
    output_contract_digest text NOT NULL DEFAULT '',
    values_jsonb jsonb NOT NULL CHECK (jsonb_typeof(values_jsonb) = 'object'),
    values_digest text NOT NULL CHECK (btrim(values_digest) <> ''),
    published_at_ns bigint NOT NULL,
    PRIMARY KEY (resource_id, observed_generation),
    FOREIGN KEY (operation_id, resource_id, capability, observed_generation)
        REFERENCES operations(id, resource_id, capability, target_generation) ON DELETE RESTRICT
);

CREATE FUNCTION liftr_reject_resource_output_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'resource output snapshots are immutable' USING ERRCODE = 'integrity_constraint_violation';
END;
$$;

CREATE TRIGGER resource_outputs_are_append_only
    BEFORE UPDATE OR DELETE ON resource_outputs FOR EACH ROW EXECUTE FUNCTION liftr_reject_resource_output_mutation();
