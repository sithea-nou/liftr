ALTER TABLE provisioning_executions
    ADD COLUMN recovery_source_operation_id text,
    ADD COLUMN recovery_source_attempt numeric(20,0),
    ADD CONSTRAINT provisioning_execution_recovery_pair CHECK (
        (recovery_source_operation_id IS NULL) = (recovery_source_attempt IS NULL)
    ),
    ADD CONSTRAINT provisioning_execution_recovery_attempt_positive CHECK (
        recovery_source_attempt IS NULL OR
        (recovery_source_attempt > 0 AND recovery_source_attempt <= 18446744073709551615)
    ),
    ADD CONSTRAINT provisioning_execution_recovery_not_self CHECK (
        recovery_source_operation_id IS NULL OR recovery_source_operation_id <> operation_id
    ),
    ADD CONSTRAINT provisioning_execution_recovery_source_attempt
        FOREIGN KEY (recovery_source_operation_id, recovery_source_attempt)
        REFERENCES provisioning_submission_attempts(operation_id, attempt_number) ON DELETE RESTRICT;

CREATE OR REPLACE FUNCTION liftr_protect_execution_intent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.operation_id IS DISTINCT FROM NEW.operation_id OR OLD.resource_id IS DISTINCT FROM NEW.resource_id OR
       OLD.provisioner_ref IS DISTINCT FROM NEW.provisioner_ref OR OLD.resource_type_name IS DISTINCT FROM NEW.resource_type_name OR
       OLD.resource_type_version IS DISTINCT FROM NEW.resource_type_version OR OLD.capability IS DISTINCT FROM NEW.capability OR
       OLD.target_generation IS DISTINCT FROM NEW.target_generation OR OLD.spec_codec_version IS DISTINCT FROM NEW.spec_codec_version OR
       OLD.submitted_spec IS DISTINCT FROM NEW.submitted_spec OR
       OLD.recovery_source_operation_id IS DISTINCT FROM NEW.recovery_source_operation_id OR
       OLD.recovery_source_attempt IS DISTINCT FROM NEW.recovery_source_attempt THEN
        RAISE EXCEPTION 'submitted execution intent is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;

CREATE INDEX provisioning_executions_recovery_source_idx
    ON provisioning_executions(recovery_source_operation_id, recovery_source_attempt)
    WHERE recovery_source_operation_id IS NOT NULL;

CREATE OR REPLACE FUNCTION liftr_protect_referenced_recovery_execution() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM provisioning_executions child
        WHERE child.recovery_source_operation_id = OLD.operation_id
    ) THEN
        RAISE EXCEPTION 'referenced recovery source execution is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provisioning_executions_protect_recovery_source
    BEFORE UPDATE OR DELETE ON provisioning_executions
    FOR EACH ROW EXECUTE FUNCTION liftr_protect_referenced_recovery_execution();

CREATE OR REPLACE FUNCTION liftr_protect_referenced_recovery_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM provisioning_executions child
        WHERE child.recovery_source_operation_id = OLD.operation_id
          AND child.recovery_source_attempt = OLD.attempt_number
    ) THEN
        RAISE EXCEPTION 'referenced recovery source submission attempt is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER provisioning_submission_attempts_protect_recovery_source
    BEFORE UPDATE OR DELETE ON provisioning_submission_attempts
    FOR EACH ROW EXECUTE FUNCTION liftr_protect_referenced_recovery_attempt();
