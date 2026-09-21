/**
 * Shared, provider-neutral Liftr presentation primitives.
 *
 * Security rule: every untrusted string renders as plain React text;
 * raw-HTML injection APIs are banned by the integration hygiene check.
 */

import React from 'react';
import { Link } from '@backstage/core-components';
import {
  Box,
  Button,
  Chip,
  Divider,
  Paper,
  Tooltip,
  Typography,
  makeStyles,
} from '@material-ui/core';
import CheckCircleOutlineIcon from '@material-ui/icons/CheckCircleOutline';
import DeleteOutlineIcon from '@material-ui/icons/DeleteOutline';
import ErrorOutlineIcon from '@material-ui/icons/ErrorOutline';
import HelpOutlineIcon from '@material-ui/icons/HelpOutline';
import HourglassEmptyIcon from '@material-ui/icons/HourglassEmpty';
import LoopIcon from '@material-ui/icons/Loop';
import { ConfigApi, configApiRef, useApi } from '@backstage/core-plugin-api';
import {
  BffErrorBody,
  LiftrProblem,
  LosslessNumber,
  OperationState,
  OwnerRef,
  ResourceState,
} from '@liftr/plugin-liftr-common';

const useStyles = makeStyles(theme => ({
  pageHeading: {
    display: 'flex',
    justifyContent: 'space-between',
    alignItems: 'flex-start',
    gap: theme.spacing(2),
    marginBottom: theme.spacing(3),
    minWidth: 0,
    maxWidth: '100%',
    '& > *': { minWidth: 0 },
    [theme.breakpoints.down('sm')]: { flexDirection: 'column' },
  },
  headingCopy: { minWidth: 0, overflowWrap: 'anywhere' },
  nav: { display: 'flex', gap: theme.spacing(1), flexWrap: 'wrap', maxWidth: '100%' },
  primaryNavAction: {
    '&&': { color: theme.palette.getContrastText(theme.palette.primary.main) },
    '&&:hover': { color: theme.palette.getContrastText(theme.palette.primary.main) },
    '&&:visited': { color: theme.palette.getContrastText(theme.palette.primary.main) },
  },
  status: { fontWeight: 600 },
  statusReady: { borderColor: theme.palette.success.main, color: theme.palette.success.main },
  statusPending: { borderColor: theme.palette.warning.main, color: theme.palette.warning.dark },
  statusFailed: { borderColor: theme.palette.error.main, color: theme.palette.error.main },
  statusDeleting: { borderColor: theme.palette.warning.main, color: theme.palette.warning.dark },
  statusMuted: { borderColor: theme.palette.divider, color: theme.palette.text.secondary },
  problem: {
    borderLeft: `4px solid ${theme.palette.error.main}`,
    padding: theme.spacing(2),
    background: theme.palette.type === 'dark' ? theme.palette.background.paper : theme.palette.error.light,
  },
  problemDetail: { marginTop: theme.spacing(0.5), maxWidth: 760, overflowWrap: 'anywhere' },
  problemActions: { display: 'flex', gap: theme.spacing(1), flexWrap: 'wrap', marginTop: theme.spacing(1.5) },
  skeleton: { padding: theme.spacing(2) },
  skeletonLine: {
    height: 14,
    borderRadius: 4,
    background: theme.palette.action.hover,
    marginBottom: theme.spacing(1.5),
  },
  empty: { padding: theme.spacing(4), textAlign: 'center' },
}));

export function gen(g: LosslessNumber): string {
  return g.toString();
}

export function generationGte(a: LosslessNumber, b: LosslessNumber): boolean {
  const ab = a.toBigInt();
  const bb = b.toBigInt();
  if (ab === undefined || bb === undefined) return false;
  return ab >= bb;
}

export function formatTimestamp(value?: string): string {
  if (!value) return '—';
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  });
}

export function humanize(value: string): string {
  return value
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_-]+/g, ' ')
    .replace(/^./, first => first.toUpperCase());
}

export const LiftrPageHeading: React.FC<{
  title: string;
  description?: string;
  primaryAction?: React.ReactNode;
}> = ({ title, description, primaryAction }) => {
  const classes = useStyles();
  return (
    <div className={classes.pageHeading}>
      <div className={classes.headingCopy}>
        <Typography variant="h4" component="h1">{title}</Typography>
        {description && <Typography color="textSecondary">{description}</Typography>}
      </div>
      <div className={classes.nav} aria-label="Liftr navigation">
        <Button component={Link as React.ElementType} to="/liftr" size="small">Resources</Button>
        <Button component={Link as React.ElementType} to="/liftr/resource-types" size="small">Resource Types</Button>
        {primaryAction ?? (
          <Button className={classes.primaryNavAction} component={Link as React.ElementType} to="/liftr/create" color="primary" variant="contained" size="small">
            Create Resource
          </Button>
        )}
      </div>
    </div>
  );
};

function resourceStateIcon(state: string): React.ReactElement {
  switch (state) {
    case 'Ready': return <CheckCircleOutlineIcon />;
    case 'Pending': return <HourglassEmptyIcon />;
    case 'Failed': return <ErrorOutlineIcon />;
    case 'Deleting': return <DeleteOutlineIcon />;
    default: return <HelpOutlineIcon />;
  }
}

export function StateChip({ state }: { state: ResourceState | string }) {
  const classes = useStyles();
  const stateClass = state === 'Ready'
    ? classes.statusReady
    : state === 'Pending'
      ? classes.statusPending
      : state === 'Failed'
        ? classes.statusFailed
        : state === 'Deleting'
          ? classes.statusDeleting
          : classes.statusMuted;
  return (
    <Chip
      size="small"
      icon={resourceStateIcon(state)}
      label={String(state)}
      variant="outlined"
      className={`${classes.status} ${stateClass}`}
      aria-label={`Resource state: ${state}`}
    />
  );
}

export function OperationStateChip({ state }: { state: OperationState | string }) {
  const classes = useStyles();
  const icon = state === 'Succeeded'
    ? <CheckCircleOutlineIcon />
    : state === 'Failed'
      ? <ErrorOutlineIcon />
      : state === 'Pending' || state === 'Running'
        ? <LoopIcon />
        : <HelpOutlineIcon />;
  const stateClass = state === 'Succeeded'
    ? classes.statusReady
    : state === 'Failed'
      ? classes.statusFailed
      : state === 'Pending' || state === 'Running'
        ? classes.statusPending
        : classes.statusMuted;
  return (
    <Chip
      size="small"
      icon={icon}
      label={String(state)}
      variant="outlined"
      className={`${classes.status} ${stateClass}`}
      aria-label={`Operation state: ${state}`}
    />
  );
}

export const LoadingSkeleton: React.FC<{ rows?: number; label?: string }> = ({ rows = 4, label = 'Loading Liftr data' }) => {
  const classes = useStyles();
  return (
    <Paper className={classes.skeleton} aria-busy="true" aria-label={label}>
      {Array.from({ length: rows }, (_, index) => (
        <div
          className={classes.skeletonLine}
          key={index}
          style={{ width: index === rows - 1 ? '58%' : index % 2 === 0 ? '92%' : '76%' }}
        />
      ))}
    </Paper>
  );
};

export const LiftrEmptyState: React.FC<{
  title: string;
  description: string;
  action?: React.ReactNode;
}> = ({ title, description, action }) => {
  const classes = useStyles();
  return (
    <Paper variant="outlined" className={classes.empty}>
      <Typography variant="h6" component="h2">{title}</Typography>
      <Typography color="textSecondary" paragraph>{description}</Typography>
      {action}
    </Paper>
  );
};

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
  const entry = ownerMapping(configApi)[owner.kind];
  if (entry?.backstageKind) {
    const target = `${entry.backstageKind}:${entry.namespace ?? 'default'}/${owner.id}`;
    return (
      <Tooltip title={`Liftr owner ${owner.kind}/${owner.id} · catalog link is display only`}>
        <Link to={`../catalog/${target}`}>{`${owner.kind}/${owner.id}`}</Link>
      </Tooltip>
    );
  }
  return <span>{`${owner.kind}/${owner.id}`}</span>;
}

/** Output freshness per ADR-0011 semantics: desired vs observed output generation. */
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
    return <Typography variant="body2" color="textSecondary">No outputs published yet.</Typography>;
  }
  const fresh = observedGenerationStatus !== undefined
    ? generationGte(observedGenerationStatus, outputsGeneration) && generationGte(outputsGeneration, desiredGeneration)
    : generationGte(outputsGeneration, desiredGeneration);
  return (
    <Box display="flex" alignItems="center" gridGap={8} mb={1}>
      <Chip size="small" label={fresh ? 'Current' : 'Previous generation'} color={fresh ? 'primary' : 'default'} variant="outlined" />
      <Typography variant="body2" color={fresh ? 'textPrimary' : 'textSecondary'}>
        Generation {gen(outputsGeneration)} of {gen(desiredGeneration)}
      </Typography>
    </Box>
  );
}

const PROBLEM_COPY: Record<string, { heading: string; guidance: string }> = {
  RESOURCE_IN_USE: {
    heading: 'Resource cannot be deleted',
    guidance: 'It is still referenced by another Liftr Resource. Change that Resource’s dependency and wait for reconciliation before retrying.',
  },
  REFERENCE_INVALID: {
    heading: 'Dependency is not valid',
    guidance: 'Choose a visible Resource whose type is allowed by this ResourceType contract.',
  },
  DEPENDENCY_CYCLE: {
    heading: 'Dependency cycle detected',
    guidance: 'Choose dependency targets that do not create a cycle between Resources.',
  },
  POLICY_DENIED: {
    heading: 'Request does not meet platform policy',
    guidance: 'Adjust the Resource configuration to satisfy the platform admission policy.',
  },
  QUOTA_EXCEEDED: {
    heading: 'Resource quota reached',
    guidance: 'Delete unused Resources or ask the platform team for a quota change.',
  },
  GENERATION_CONFLICT: {
    heading: 'Resource changed since you opened it',
    guidance: 'Reload the Resource and review the current desired state before trying again.',
  },
};

/** Curated Problem/BFF failure presentation. Never displays headers, tokens, stack material, or private fields. */
export function ProblemView({
  error,
  onReplaySameKey,
  onReload,
}: {
  error: Error & { problem?: unknown; bff?: unknown; status?: number; outcomeUnknown?: boolean };
  onReplaySameKey?: () => void;
  onReload?: () => void;
}) {
  const classes = useStyles();
  const problem = error.problem as LiftrProblem | undefined;
  const bff = error.bff as BffErrorBody | undefined;
  const code = problem?.code ?? bff?.code;
  const mapped = code ? PROBLEM_COPY[code] : undefined;
  const auth = error.status === 401 || code === 'LIFTR_AUTHENTICATION_REQUIRED';
  const unavailable = error.status === 502 || error.status === 503 || error.status === 504;
  const heading = mapped?.heading
    ?? (auth ? 'Sign in to use Liftr' : unavailable ? 'Liftr is temporarily unavailable' : problem?.title ?? bff?.title ?? 'Request failed');
  const safeDetail = problem?.detail ?? bff?.detail;
  return (
    <Paper role="alert" className={classes.problem} elevation={0}>
      <Box display="flex" alignItems="center" gridGap={8}>
        <ErrorOutlineIcon color="error" aria-hidden="true" />
        <Typography variant="subtitle1" component="h2">{heading}</Typography>
      </Box>
      {safeDetail && <Typography variant="body2" className={classes.problemDetail}>{safeDetail}</Typography>}
      {mapped && <Typography variant="body2" className={classes.problemDetail}>{mapped.guidance}</Typography>}
      {problem?.currentGeneration !== undefined && (
        <Typography variant="body2" className={classes.problemDetail}>
          Server generation: {problem.currentGeneration.toString()}
        </Typography>
      )}
      {problem?.violations && problem.violations.length > 0 && (
        <Box component="ul" my={1} pl={3}>
          {problem.violations.map((violation, index) => (
            <li key={`${violation.path}-${index}`}>
              <Typography variant="body2">
                <code>{violation.path || '/'}</code>: {violation.message}
              </Typography>
            </li>
          ))}
        </Box>
      )}
      {problem?.truncated && <Typography variant="caption">Additional validation issues were omitted.</Typography>}
      {error.outcomeUnknown && (
        <Typography variant="body2" className={classes.problemDetail}>
          The connection ended before Liftr returned a definitive result. Replay only this exact action with its original key.
        </Typography>
      )}
      {(onReplaySameKey || (code === 'GENERATION_CONFLICT' && onReload)) && (
        <div className={classes.problemActions}>
          {onReplaySameKey && <Button size="small" variant="outlined" onClick={onReplaySameKey}>Replay same action</Button>}
          {code === 'GENERATION_CONFLICT' && onReload && (
            <Button size="small" color="primary" variant="contained" onClick={onReload}>Reload Resource</Button>
          )}
        </div>
      )}
      {(code || problem?.requestId || bff?.requestId) && <Divider style={{ marginTop: 12, marginBottom: 8 }} />}
      {code && <Typography variant="caption">{code}{error.status ? ` · HTTP ${error.status}` : ''}</Typography>}
      {(problem?.requestId || bff?.requestId) && (
        <Typography variant="caption" display="block">
          Request {problem?.requestId ?? bff?.requestId}{bff?.correlationId ? ` · Correlation ${bff.correlationId}` : ''}
        </Typography>
      )}
      {!problem && !bff && !unavailable && <Typography variant="body2">{error.message}</Typography>}
    </Paper>
  );
}
