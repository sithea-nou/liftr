/**
 * Shared UI primitives. Security rule: every untrusted string renders as
 * plain React text; raw-HTML injection APIs are banned across this plugin
 * (enforced by the hygiene script).
 */

import { Link } from '@backstage/core-components';
import { Chip, Tooltip, Typography } from '@material-ui/core';
import { ConfigApi, configApiRef, useApi } from '@backstage/core-plugin-api';
import {
  BffErrorBody,
  LosslessNumber,
  OwnerRef,
  LiftrProblem,
  ResourceState,
} from '@liftr/plugin-liftr-common';

export function gen(g: LosslessNumber): string {
  return g.toString();
}

export function generationGte(a: LosslessNumber, b: LosslessNumber): boolean {
  const ab = a.toBigInt();
  const bb = b.toBigInt();
  if (ab === undefined || bb === undefined) return false;
  return ab >= bb;
}

const STATE_COLORS: Record<ResourceState, 'default' | 'primary' | 'secondary'> = {
  Ready: 'primary',
  Pending: 'default',
  Deleting: 'secondary',
  Deleted: 'default',
  Failed: 'secondary',
  Unknown: 'default',
};

export function StateChip({ state }: { state: ResourceState | string }) {
  return (
    <Chip
      size="small"
      label={String(state)}
      color={(STATE_COLORS as Record<string, 'default' | 'primary' | 'secondary'>)[state] ?? 'default'}
      variant={state === 'Failed' ? 'default' : 'outlined'}
    />
  );
}

/**
 * Presentation-only owner display. Catalog entity mapping is navigation
 * sugar configured by the operator; it NEVER affects outgoing Liftr
 * requests or authorization (byte-identical requests regardless of
 * mapping/catalog contents).
 */
interface OwnerDisplayEntry {
  backstageKind?: string;
  namespace?: string;
}

function ownerMapping(configApi: ConfigApi): Record<string, OwnerDisplayEntry> {
  const c = configApi.getOptionalConfig('liftr.ownerDisplay');
  if (!c) return {};
  const out: Record<string, OwnerDisplayEntry> = {};
  for (const kind of c.keys()) {
    out[kind] = {
      backstageKind: c.getOptionalString(`${kind}.backstageKind`),
      namespace: c.getOptionalString(`${kind}.namespace`) ?? 'default',
    };
  }
  return out;
}

export function OwnerRefView({ owner }: { owner: OwnerRef }) {
  const configApi = useApi(configApiRef);
  const map = ownerMapping(configApi);
  const entry = map[owner.kind];
  if (entry?.backstageKind) {
    const target = `${entry.backstageKind}:${entry.namespace ?? 'default'}/${owner.id}`;
    return (
      <Tooltip title={`Liftr owner ${owner.kind}/${owner.id} · linked to catalog entity (display only)`}>
        <Link to={`../catalog/${target}`}>{`${owner.kind}/${owner.id}`}</Link>
      </Tooltip>
    );
  }
  return <span>{`${owner.kind}/${owner.id}`}</span>;
}

/** Output freshness per ADR-0011 semantics: O vs S vs D. */
export function OutputsFreshness({
  desiredGeneration,
  observedGenerationStatus,
  outputsGeneration,
}: {
  desiredGeneration: LosslessNumber;
  observedGenerationStatus?: LosslessNumber;
  outputsGeneration?: LosslessNumber;
}) {
  if (!outputsGeneration) {
    return (
      <Typography variant="body2" color="textSecondary">
        No outputs published yet.
      </Typography>
    );
  }
  const fresh =
    observedGenerationStatus !== undefined
      ? generationGte(observedGenerationStatus, outputsGeneration) &&
        generationGte(outputsGeneration, desiredGeneration)
      : generationGte(outputsGeneration, desiredGeneration);
  return (
    <Typography component="div" variant="body2">
      {fresh ? (
        <>
          <Chip size="small" label="Fresh" color="primary" variant="outlined" />{' '}
          Outputs reflect generation {gen(outputsGeneration)} of {gen(desiredGeneration)}.
        </>
      ) : (
        <>
          <Chip size="small" label="Stale" color="secondary" variant="outlined" />{' '}
          Outputs reflect generation {gen(outputsGeneration)} while desired is{' '}
          {gen(desiredGeneration)}. Values below belong to the older generation.
        </>
      )}
    </Typography>
  );
}

/**
 * Renders sanitized Liftr problems and BFF failures. Shows requestId for
 * support correlation; never shows tokens, headers, or stack material.
 */
export function ProblemView({
  error,
  onReplaySameKey,
}: {
  error: Error & { problem?: unknown; bff?: unknown; status?: number; outcomeUnknown?: boolean };
  onReplaySameKey?: () => void;
}) {
  const problem = error.problem as LiftrProblem | undefined;
  const bff = error.bff as BffErrorBody | undefined;
  const guidance = problem?.code ? PROBLEM_GUIDANCE[problem.code] : undefined;
  return (
    <div role="alert" style={{ border: '1px solid #d32f2f', borderRadius: 4, padding: 12 }}>
      <Typography variant="subtitle2" style={{ color: '#d32f2f' }}>
        {problem?.title ?? bff?.title ?? 'Request failed'} {error.status ? `(HTTP ${error.status})` : ''}
      </Typography>
      {(problem?.detail || bff?.detail) && (
        <Typography variant="body2">{problem?.detail ?? bff?.detail}</Typography>
      )}
      {problem?.code && (
        <Typography variant="caption" display="block">
          Code: {problem.code}
        </Typography>
      )}
      {guidance && <Typography variant="body2">{guidance}</Typography>}
      {problem?.currentGeneration !== undefined && (
        <Typography variant="body2">
          Current generation on the server: {problem.currentGeneration.toString()} — review and reload
          before retrying.
        </Typography>
      )}
      {problem?.violations && problem.violations.length > 0 && (
        <ul style={{ margin: '8px 0' }}>
          {problem.violations.map((v, i) => (
            <li key={i}>
              <Typography variant="body2">
                <code>{v.path || '/'}</code> ({v.keyword}): {v.message}
              </Typography>
            </li>
          ))}
        </ul>
      )}
      {problem?.truncated && (
        <Typography variant="caption">Additional violations were truncated.</Typography>
      )}
      {error.outcomeUnknown && (
        <div>
          <Typography variant="body2">
            The result of your last action is unknown (connection lost before a definitive
            answer). You may replay this exact action with its original key.
          </Typography>
          {onReplaySameKey && (
            <button type="button" onClick={onReplaySameKey}>
              Replay same action
            </button>
          )}
        </div>
      )}
      {(problem?.requestId || bff?.requestId) && (
        <Typography variant="caption" display="block">
          Request ID: {problem?.requestId ?? bff?.requestId}
          {bff?.correlationId ? ` · Correlation ID: ${bff.correlationId}` : ''}
        </Typography>
      )}
      {!problem && !bff && <Typography variant="body2">{error.message}</Typography>}
    </div>
  );
}

const PROBLEM_GUIDANCE: Record<string, string> = {
  RESOURCE_IN_USE: 'Remove the desired reference and wait for the referencing Resource to finish deletion before retrying.',
  REFERENCE_INVALID: 'Review the ResourceType reference contract and choose a visible Resource with an allowed type.',
  DEPENDENCY_CYCLE: 'Choose reference targets that do not create a dependency cycle.',
  POLICY_DENIED: 'Adjust the requested Resource to satisfy platform admission policy.',
  QUOTA_EXCEEDED: 'Delete unused Resources or request a quota change before retrying.',
  GENERATION_CONFLICT: 'The Resource changed after this page loaded. Refresh and review the current generation before retrying.',
};
