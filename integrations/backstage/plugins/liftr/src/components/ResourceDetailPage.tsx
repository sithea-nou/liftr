/** Resource detail: developer overview, desired configuration, and asynchronous operation history. */

import React, { useCallback, useEffect, useState } from 'react';
import { useLocation } from 'react-router-dom';
import {
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  Divider,
  Grid,
  Paper,
  Tab,
  Tabs,
  Typography,
  makeStyles,
} from '@material-ui/core';
import RefreshIcon from '@material-ui/icons/Refresh';
import { InfoCard, Link } from '@backstage/core-components';
import {
  Condition,
  LosslessNumber,
  Operation,
  Resource,
  ResourceReferences,
  ResourceSummary,
  ResourceTypeDetail,
  isLosslessNumber,
  newLogicalActionKey,
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { LiftrApiError, LiftrFrontendClient } from '../api/client';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useOperationMonitor } from '../hooks/useOperationMonitor';
import { useIdempotentAction } from '../hooks/useIdempotentAction';
import {
  LiftrEmptyState,
  LiftrPageHeading,
  LoadingSkeleton,
  OperationStateChip,
  OutputsFreshness,
  ProblemView,
  StateChip,
  formatTimestamp,
  gen,
  humanize,
} from './common';
import {
  ReferencePicker,
  ReferencesEditor,
  SpecEditor,
  buildUpdateFromEditor,
} from './CreateResourcePage';

interface LocationState { monitorOperationId?: string }
type MutationDraft =
  | { kind: 'update'; key: string; generation: string; bodyText: string }
  | { kind: 'delete'; key: string; generation: string };

const POLLED_RESOURCE_STATES = new Set(['Unknown', 'Pending', 'Deleting']);
const MAX_RESOURCE_POLLS = 120;

const useStyles = makeStyles(theme => ({
  page: { minWidth: 0, width: '100%', margin: 0, '& .MuiGrid-item': { minWidth: 0 } },
  header: { padding: theme.spacing(2.5), minWidth: 0, maxWidth: '100%', boxSizing: 'border-box' },
  headerRow: { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: theme.spacing(2), '& > *': { minWidth: 0 }, [theme.breakpoints.down('sm')]: { flexDirection: 'column' } },
  actions: { display: 'flex', gap: theme.spacing(1), flexWrap: 'wrap' },
  tabs: { marginTop: theme.spacing(2), borderBottom: `1px solid ${theme.palette.divider}` },
  panel: { paddingTop: theme.spacing(3), minWidth: 0, maxWidth: '100%' },
  card: { padding: theme.spacing(2), height: '100%', minWidth: 0, maxWidth: '100%', boxSizing: 'border-box', overflowWrap: 'anywhere' },
  identifier: { overflowWrap: 'anywhere', wordBreak: 'break-word' },
  sectionTitle: { marginBottom: theme.spacing(1.5) },
  metadata: { display: 'flex', gap: theme.spacing(2), flexWrap: 'wrap', marginTop: theme.spacing(1.5) },
  condition: { padding: theme.spacing(1.25, 0), '& + &': { borderTop: `1px solid ${theme.palette.divider}` } },
  conditionWaiting: { borderLeft: `3px solid ${theme.palette.warning.main}`, paddingLeft: theme.spacing(1.5) },
  reference: { padding: theme.spacing(1.5), marginBottom: theme.spacing(1), display: 'flex', justifyContent: 'space-between', gap: theme.spacing(1), alignItems: 'center', minWidth: 0, '& > *': { minWidth: 0 }, [theme.breakpoints.down('xs')]: { alignItems: 'flex-start', flexDirection: 'column' } },
  outputRow: { display: 'grid', gridTemplateColumns: 'minmax(120px, 1fr) minmax(0, 2fr)', gap: theme.spacing(2), padding: theme.spacing(1, 0), borderBottom: `1px solid ${theme.palette.divider}`, '& > *': { minWidth: 0, overflowWrap: 'anywhere' }, [theme.breakpoints.down('xs')]: { gridTemplateColumns: 'minmax(0, 1fr)', gap: theme.spacing(0.5) } },
  code: { width: '100%', maxWidth: '100%', boxSizing: 'border-box', overflowX: 'auto', WebkitOverflowScrolling: 'touch', padding: theme.spacing(2), borderRadius: theme.shape.borderRadius, background: theme.palette.type === 'dark' ? theme.palette.background.default : theme.palette.grey[100], border: `1px solid ${theme.palette.divider}`, fontSize: 12, lineHeight: 1.6 },
  operation: { padding: theme.spacing(1.5, 0), minWidth: 0, overflowWrap: 'anywhere', '& + &': { borderTop: `1px solid ${theme.palette.divider}` } },
}));

async function loadVisibleInventory(client: LiftrFrontendClient): Promise<ResourceSummary[]> {
  const resources: ResourceSummary[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < 20; page += 1) {
    const result = await client.listResources({ limit: 100, cursor });
    resources.push(...result.items);
    cursor = result.nextCursor;
    if (!cursor) break;
  }
  return resources;
}

export const ResourceDetailPage: React.FC<{ resourceId: string }> = ({ resourceId }) => {
  const classes = useStyles();
  const client = useLiftrClient();
  const location = useLocation();
  const action = useIdempotentAction();
  const [resource, setResource] = useState<Resource | null>(null);
  const [typeDetail, setTypeDetail] = useState<ResourceTypeDetail | null>(null);
  const [visibleResources, setVisibleResources] = useState<ResourceSummary[]>([]);
  const [error, setError] = useState<LiftrApiError | null>(null);
  const [mutationError, setMutationError] = useState<LiftrApiError | null>(null);
  const [mutationDraft, setMutationDraft] = useState<MutationDraft | null>(null);
  const [monitorOperationId, setMonitorOperationId] = useState<string | undefined>((location.state as LocationState | null)?.monitorOperationId);
  const [tab, setTab] = useState(0);
  const [editing, setEditing] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [specText, setSpecText] = useState('{}');
  const [referencesText, setReferencesText] = useState('{}');
  const [editReferences, setEditReferences] = useState<ResourceReferences>({});
  const monitor = useOperationMonitor(client, monitorOperationId);

  const load = useCallback(async () => {
    try {
      const next = await client.getResource(resourceId);
      setResource(next);
      setError(null);
    } catch (nextError) {
      setError(nextError as LiftrApiError);
    }
  }, [client, resourceId]);

  useEffect(() => { void load(); }, [load]);
  useEffect(() => { if (monitor.operation) void load(); }, [monitor.operation?.state, monitor.operation?.id, load]);
  useEffect(() => {
    if (!resource) return undefined;
    let alive = true;
    client.getResourceType(resource.type.name, resource.type.version).then(detail => alive && setTypeDetail(detail)).catch(() => {});
    loadVisibleInventory(client).then(items => alive && setVisibleResources(items)).catch(() => {});
    return () => { alive = false; };
  }, [client, resource?.type.name, resource?.type.version]);
  useEffect(() => {
    if (!resource || !POLLED_RESOURCE_STATES.has(resource.status.state)) return undefined;
    let polls = 0;
    const timer = setInterval(() => {
      if (document.visibilityState === 'hidden') return;
      polls += 1;
      if (polls > MAX_RESOURCE_POLLS) clearInterval(timer);
      else void load();
    }, 5_000);
    return () => clearInterval(timer);
  }, [resource?.status.state, load]);

  const sendMutation = async (draft: MutationDraft) => {
    setMutationError(null);
    try {
      const envelope = draft.kind === 'update'
        ? await client.update({ resourceId, bodyText: draft.bodyText, idempotencyKey: draft.key, viewedGeneration: draft.generation })
        : await client.remove({ resourceId, idempotencyKey: draft.key, viewedGeneration: draft.generation });
      action.markAdmitted();
      setEditing(false);
      setDeleteOpen(false);
      setMutationDraft(null);
      setMonitorOperationId(envelope.monitorOperationId);
      await load();
    } catch (nextError) {
      const apiError = nextError as LiftrApiError;
      setMutationError(apiError);
      if (apiError.outcomeUnknown) action.markUnknownOutcome();
      else action.markFailed();
    }
  };

  const beginUpdate = () => {
    if (!resource) return;
    const referenceValue = typeDetail?.referenceContract ? stringifyLosslessJson(editReferences) : referencesText;
    const built = buildUpdateFromEditor(specText, referenceValue);
    if (!built.ok) {
      setMutationError(new LiftrApiError(null, { code: 'LIFTR_REQUEST_INVALID', title: 'Invalid input', detail: built.error }, 400));
      return;
    }
    const draft: MutationDraft = { kind: 'update', key: action.begin(), generation: gen(resource.generation), bodyText: built.bodyText };
    setMutationDraft(draft);
    void sendMutation(draft);
  };

  const beginDelete = () => {
    if (!resource) return;
    const draft: MutationDraft = { kind: 'delete', key: action.begin(), generation: gen(resource.generation) };
    setMutationDraft(draft);
    void sendMutation(draft);
  };

  if (error) return <ProblemView error={error} onReload={() => void load()} />;
  if (!resource) return <LoadingSkeleton rows={8} label="Loading Resource" />;

  const specDisplay = stringifyLosslessJson(resource.spec);
  const referencesDisplay = stringifyLosslessJson(resource.references ?? {});
  const mutable = !['Deleting', 'Deleted'].includes(resource.status.state);

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}><LiftrPageHeading title={resource.id} description={`${resource.type.name}/${resource.type.version}`} /></Grid>
      <Grid item xs={12}>
        <Paper className={classes.header} variant="outlined">
          <div className={classes.headerRow}>
            <div>
              <Box display="flex" alignItems="center" gridGap={10} flexWrap="wrap">
                <Typography className={classes.identifier} variant="h5" component="h2">{resource.id}</Typography>
                <StateChip state={resource.status.state} />
              </Box>
              <Typography className={classes.identifier} variant="body2" color="textSecondary"><code>{resource.id}</code> · {resource.type.name}/{resource.type.version}</Typography>
              <div className={classes.metadata}>
                <Typography variant="body2">Generation <strong>{gen(resource.generation)}</strong></Typography>
                <Typography variant="body2">Observed <strong>{gen(resource.status.observedGeneration)}</strong></Typography>
                <Typography variant="body2">Updated <strong>{formatTimestamp(resource.status.updatedAt)}</strong></Typography>
              </div>
            </div>
            <div className={classes.actions}>
              <Button size="small" startIcon={<RefreshIcon />} onClick={() => void load()}>Refresh</Button>
              {mutable && <Button size="small" variant="outlined" onClick={() => {
                setSpecText(specDisplay);
                setReferencesText(referencesDisplay);
                setEditReferences(resource.references ?? {});
                setEditing(true);
                setTab(1);
                setMutationError(null);
              }}>Update</Button>}
              {resource.status.state !== 'Deleted' && <Button size="small" color="secondary" variant="outlined" onClick={() => setDeleteOpen(true)}>Delete</Button>}
            </div>
          </div>
          {monitorOperationId && (
            <Box mt={2} display="flex" alignItems="center" gridGap={8} flexWrap="wrap">
              <Typography variant="body2">Lifecycle operation</Typography>
              {monitor.operation ? <OperationStateChip state={monitor.operation.state} /> : <Chip size="small" label="Checking status" variant="outlined" />}
              <Typography variant="caption" color="textSecondary"><code>{monitorOperationId}</code></Typography>
            </Box>
          )}
          {monitor.error && <Typography color="error">Operation status could not be refreshed: {monitor.error}</Typography>}
          <Tabs className={classes.tabs} value={tab} onChange={(_, value: number) => setTab(value)} variant="scrollable" scrollButtons="auto" aria-label="Resource details">
            <Tab label="Overview" />
            <Tab label="Configuration" />
            <Tab label="Operations" />
          </Tabs>
        </Paper>

        {mutationError && <Box mt={2}><ProblemView error={mutationError} onReplaySameKey={mutationDraft ? () => void sendMutation(mutationDraft) : undefined} onReload={() => { setMutationError(null); void load(); }} /></Box>}

        <div className={classes.panel}>
          {tab === 0 && (
            <Grid container spacing={2}>
              <Grid item xs={12} md={6}><LifecycleCard resource={resource} /></Grid>
              <Grid item xs={12} md={6}><ReferencesCard references={resource.references ?? {}} inventory={visibleResources} /></Grid>
              <Grid item xs={12} md={6}><OutputsCard resource={resource} /></Grid>
              <Grid item xs={12} md={6}><LatestOperationCard resource={resource} /></Grid>
            </Grid>
          )}
          {tab === 1 && (
            <InfoCard title="Desired configuration" subheader="References are separate from the opaque Resource spec.">
              {editing ? (
                <Box display="grid" gridGap={20}>
                  <SpecEditor value={specText} onChange={setSpecText} />
                  {typeDetail?.referenceContract ? (
                    <Box display="grid" gridGap={16}>
                      <Typography variant="h6" component="h2">Dependencies</Typography>
                      {typeDetail.referenceContract.slots.map(slot => (
                        <ReferencePicker
                          key={slot.name}
                          slot={slot}
                          inventory={visibleResources.filter(item => item.id !== resource.id)}
                          sourceOwner={resource.owner}
                          value={editReferences[slot.name] ?? []}
                          onChange={targets => setEditReferences(previous => {
                            const next = { ...previous };
                            if (!targets.length) delete next[slot.name];
                            else next[slot.name] = targets;
                            return next;
                          })}
                        />
                      ))}
                    </Box>
                  ) : <ReferencesEditor value={referencesText} onChange={setReferencesText} />}
                  <Box display="flex" gridGap={8} flexWrap="wrap">
                    <Button color="primary" variant="contained" disabled={action.phase === 'running'} onClick={beginUpdate}>Update at generation {gen(resource.generation)}</Button>
                    <Button onClick={() => setEditing(false)}>Cancel</Button>
                  </Box>
                </Box>
              ) : (
                <>
                  <Typography variant="subtitle2">Spec</Typography>
                  <pre className={classes.code}>{specDisplay}</pre>
                  <Box mt={2}><Typography variant="subtitle2">Dependencies</Typography><pre className={classes.code}>{referencesDisplay}</pre></Box>
                </>
              )}
            </InfoCard>
          )}
          {tab === 2 && <OperationsPanel resourceId={resource.id} generation={resource.generation} onMutationAdmitted={setMonitorOperationId} />}
        </div>
      </Grid>

      <Dialog open={deleteOpen} onClose={() => setDeleteOpen(false)} aria-labelledby="delete-resource-title">
        <DialogTitle id="delete-resource-title">Delete {resource.id}?</DialogTitle>
        <DialogContent>
          <DialogContentText>This starts asynchronous deletion. Liftr will protect the Resource while another Resource still depends on it.</DialogContentText>
          <Typography variant="body2"><strong>Resource</strong></Typography>
          <Typography variant="body2">{resource.id}</Typography>
          <Typography variant="caption" color="textSecondary"><code>{resource.id}</code> · {resource.type.name}/{resource.type.version}</Typography>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setDeleteOpen(false)}>Cancel</Button>
          <Button color="secondary" variant="contained" disabled={action.phase === 'running'} onClick={beginDelete}>Start deletion</Button>
        </DialogActions>
      </Dialog>
    </Grid>
  );
};

export const ConditionsCard: React.FC<{ conditions: Condition[] }> = ({ conditions }) => {
  const classes = useStyles();
  return (
    <>
      {conditions.map(condition => {
        const waiting = condition.type === 'DependenciesReady' && condition.status !== 'True';
        return (
          <div key={`${condition.type}-${condition.observedGeneration?.toString() ?? ''}`} className={`${classes.condition} ${waiting ? classes.conditionWaiting : ''}`}>
            <Box display="flex" justifyContent="space-between" alignItems="center" gridGap={8}>
              <Typography variant="subtitle2">{humanize(condition.type)}</Typography>
              <Chip
                size="small"
                variant="outlined"
                color={condition.status === 'True' ? 'primary' : condition.status === 'False' ? 'secondary' : 'default'}
                label={condition.status === 'True' ? 'Ready' : waiting ? 'Waiting' : condition.status}
                aria-label={`${humanize(condition.type)}: ${condition.status}`}
              />
            </Box>
            {condition.reason && <Typography variant="caption" color="textSecondary" display="block">{humanize(condition.reason)}</Typography>}
            {condition.message && <Typography variant="body2">{condition.message}</Typography>}
            {condition.lastTransitionAt && <Typography variant="caption" color="textSecondary">Changed {formatTimestamp(condition.lastTransitionAt)}</Typography>}
          </div>
        );
      })}
      {!conditions.length && <Typography variant="body2" color="textSecondary">No conditions reported.</Typography>}
    </>
  );
};

const LifecycleCard: React.FC<{ resource: Resource }> = ({ resource }) => {
  const classes = useStyles();
  return (
    <Paper className={classes.card} variant="outlined">
      <Typography className={classes.sectionTitle} variant="h6" component="h2">Lifecycle</Typography>
      <Box display="flex" justifyContent="space-between" alignItems="center" mb={1}><Typography variant="body2">Resource state</Typography><StateChip state={resource.status.state} /></Box>
      <Divider />
      <ConditionsCard conditions={resource.status.conditions} />
    </Paper>
  );
};

export const ReferencesCard: React.FC<{ references: ResourceReferences; inventory: ResourceSummary[] }> = ({ references, inventory }) => {
  const classes = useStyles();
  const entries = Object.entries(references);
  return (
    <Paper className={classes.card} variant="outlined">
      <Typography className={classes.sectionTitle} variant="h6" component="h2">Dependencies</Typography>
      {!entries.length ? <Typography variant="body2" color="textSecondary">No dependencies.</Typography> : entries.map(([slot, targets]) => (
        <Box key={slot} mb={2}>
          <Typography variant="subtitle2">{humanize(slot)}</Typography>
          {targets.map(targetId => {
            const target = inventory.find(item => item.id === targetId);
            return (
              <Paper key={targetId} className={classes.reference} variant="outlined">
                <div>
                  <Link to={`/liftr/resources/${encodeURIComponent(targetId)}`}><Typography variant="subtitle2" component="span">{targetId}</Typography></Link>
                  <Typography variant="caption" display="block" color="textSecondary">{target ? `${target.type.name}/${target.type.version}` : 'Liftr Resource'}</Typography>
                  <Typography variant="caption" color="textSecondary"><code>{targetId}</code></Typography>
                </div>
                {target && <StateChip state={target.status.state} />}
              </Paper>
            );
          })}
        </Box>
      ))}
    </Paper>
  );
};

const OutputsCard: React.FC<{ resource: Resource }> = ({ resource }) => {
  const classes = useStyles();
  return (
    <Paper className={classes.card} variant="outlined">
      <Typography className={classes.sectionTitle} variant="h6" component="h2">Outputs</Typography>
      <OutputsFreshness desiredGeneration={resource.generation} observedGenerationStatus={resource.status.observedGeneration} outputsGeneration={resource.outputs?.observedGeneration} />
      {resource.outputs && Object.entries(resource.outputs.values).map(([key, value]) => (
        <div key={key} className={classes.outputRow}>
          <Typography variant="body2"><code>{key}</code></Typography>
          <Typography variant="body2">{isLosslessNumber(value) ? value.toString() : String(value)}</Typography>
        </div>
      ))}
    </Paper>
  );
};

const LatestOperationCard: React.FC<{ resource: Resource }> = ({ resource }) => {
  const classes = useStyles();
  const operation = resource.latestOperation;
  return (
    <Paper className={classes.card} variant="outlined">
      <Typography className={classes.sectionTitle} variant="h6" component="h2">Latest operation</Typography>
      {!operation ? <Typography variant="body2" color="textSecondary">No operations yet.</Typography> : (
        <>
          <Box display="flex" justifyContent="space-between" alignItems="center" mb={1}>
            <Typography variant="subtitle2" style={{ textTransform: 'capitalize' }}>{operation.capability} · Generation {gen(operation.targetGeneration)}</Typography>
            <OperationStateChip state={operation.state} />
          </Box>
          <Typography variant="caption" color="textSecondary"><code>{operation.id}</code></Typography>
        </>
      )}
    </Paper>
  );
};

interface RetryDraft { sourceOperationId: string; key: string; generation: string }

export function canRetryOperation(operation: Operation): boolean {
  return operation.state === 'Failed';
}

export const OperationsPanel: React.FC<{
  resourceId: string;
  generation: LosslessNumber;
  onMutationAdmitted?: (operationId: string) => void;
}> = ({ resourceId, generation, onMutationAdmitted }) => {
  const classes = useStyles();
  const client = useLiftrClient();
  const [operations, setOperations] = useState<Operation[] | null>(null);
  const [nextCursor, setNextCursor] = useState<string>();
  const [cursor, setCursor] = useState<string>();
  const [error, setError] = useState<Error | null>(null);
  const [retryDraft, setRetryDraft] = useState<RetryDraft | null>(null);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let alive = true;
    setOperations(null);
    client.listOperations(resourceId, 20, cursor).then(result => {
      if (!alive) return;
      setOperations(result.items);
      setNextCursor(result.nextCursor);
      setError(null);
    }).catch((nextError: Error) => alive && setError(nextError));
    return () => { alive = false; };
  }, [client, resourceId, cursor, reload]);

  const sendRetry = async (draft: RetryDraft) => {
    setError(null);
    try {
      const envelope = await client.retry({ sourceOperationId: draft.sourceOperationId, idempotencyKey: draft.key, viewedGeneration: draft.generation });
      setRetryDraft(null);
      onMutationAdmitted?.(envelope.monitorOperationId);
      setReload(value => value + 1);
    } catch (nextError) { setError(nextError as Error); }
  };

  if (!operations && !error) return <LoadingSkeleton rows={5} label="Loading operation history" />;
  return (
    <InfoCard title="Operation history" subheader="Lifecycle actions run asynchronously; server state remains authoritative.">
      {error && <ProblemView error={error} onReplaySameKey={retryDraft ? () => void sendRetry(retryDraft) : undefined} />}
      {operations?.length === 0 && <LiftrEmptyState title="No operations yet" description="Lifecycle operations will appear here after the Resource is admitted." />}
      {operations?.map(operation => (
        <div className={classes.operation} key={operation.id}>
          <Box display="flex" justifyContent="space-between" alignItems="flex-start" gridGap={12} flexWrap="wrap">
            <div>
              <Typography variant="subtitle2" style={{ textTransform: 'capitalize' }}>{operation.capability} · Generation {gen(operation.targetGeneration)}</Typography>
              <Typography variant="caption" color="textSecondary"><code>{operation.id}</code>{operation.retryOf ? ` · Retry of ${operation.retryOf}` : ''}</Typography>
            </div>
            <Box display="flex" alignItems="center" gridGap={8}>
              <OperationStateChip state={operation.state} />
              {canRetryOperation(operation) && <Button size="small" variant="outlined" onClick={() => {
                const draft = { sourceOperationId: operation.id, key: newLogicalActionKey(), generation: gen(generation) };
                setRetryDraft(draft);
                void sendRetry(draft);
              }}>Retry</Button>}
            </Box>
          </Box>
          <Typography variant="body2" color="textSecondary">
            Requested {formatTimestamp(operation.requestedAt)}{operation.completedAt ? ` · Completed ${formatTimestamp(operation.completedAt)}` : operation.startedAt ? ` · Started ${formatTimestamp(operation.startedAt)}` : ''}
          </Typography>
          {operation.failure && <Typography variant="body2" color="error">{humanize(operation.failure.reason)}{operation.failure.message ? ` — ${operation.failure.message}` : ''}</Typography>}
        </div>
      ))}
      <Box mt={2} display="flex" gridGap={8}>
        <Button size="small" startIcon={<RefreshIcon />} onClick={() => setReload(value => value + 1)}>Refresh</Button>
        {nextCursor && <Button size="small" variant="outlined" onClick={() => setCursor(nextCursor)}>Next page</Button>}
      </Box>
    </InfoCard>
  );
};
