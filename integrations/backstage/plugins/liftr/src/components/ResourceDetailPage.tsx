/**
 * Resource detail keeps desired state, observed state, outputs, and Operations
 * separate while exposing generation-safe lifecycle actions.
 */

import React, { useCallback, useEffect, useState } from 'react';
import { useLocation } from 'react-router-dom';
import { Grid, Tab, Tabs, Typography, Button, List, ListItem, ListItemText } from '@material-ui/core';
import { InfoCard, Link } from '@backstage/core-components';
import {
  LosslessNumber,
  Operation,
  Resource,
  isLosslessNumber,
  newLogicalActionKey,
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useOperationMonitor } from '../hooks/useOperationMonitor';
import { useIdempotentAction } from '../hooks/useIdempotentAction';
import { ProblemView, OutputsFreshness, StateChip, gen } from './common';
import { LiftrApiError } from '../api/client';
import { ReferencesEditor, SpecEditor, buildUpdateFromEditor } from './CreateResourcePage';

interface LocationState {
  monitorOperationId?: string;
}

type MutationDraft =
  | { kind: 'update'; key: string; generation: string; bodyText: string }
  | { kind: 'delete'; key: string; generation: string };

const POLLED_RESOURCE_STATES = new Set(['Unknown', 'Pending', 'Deleting']);
const MAX_RESOURCE_POLLS = 120;

export const ResourceDetailPage: React.FC<{ resourceId: string }> = ({ resourceId }) => {
  const client = useLiftrClient();
  const location = useLocation();
  const action = useIdempotentAction();
  const [resource, setResource] = useState<Resource | null>(null);
  const [error, setError] = useState<LiftrApiError | null>(null);
  const [mutationError, setMutationError] = useState<LiftrApiError | null>(null);
  const [mutationDraft, setMutationDraft] = useState<MutationDraft | null>(null);
  const [monitorOperationId, setMonitorOperationId] = useState<string | undefined>(
    (location.state as LocationState | null)?.monitorOperationId,
  );
  const [tab, setTab] = useState(0);
  const [editing, setEditing] = useState(false);
  const [specText, setSpecText] = useState('{}');
  const [referencesText, setReferencesText] = useState('{}');
  const monitor = useOperationMonitor(client, monitorOperationId);

  const load = useCallback(async () => {
    try {
      const next = await client.getResource(resourceId);
      setResource(next);
      setError(null);
    } catch (e) {
      setError(e as LiftrApiError);
    }
  }, [client, resourceId]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!monitor.operation) return;
    void load();
  }, [monitor.operation?.state, monitor.operation?.id, load]);

  useEffect(() => {
    if (!resource || !POLLED_RESOURCE_STATES.has(resource.status.state)) return undefined;
    let polls = 0;
    const timer = setInterval(() => {
      if (document.visibilityState === 'hidden') return;
      polls += 1;
      if (polls > MAX_RESOURCE_POLLS) {
        clearInterval(timer);
        return;
      }
      void load();
    }, 5_000);
    return () => clearInterval(timer);
  }, [resource?.status.state, load]);

  const sendMutation = async (draft: MutationDraft) => {
    setMutationError(null);
    try {
      const envelope = draft.kind === 'update'
        ? await client.update({
            resourceId,
            bodyText: draft.bodyText,
            idempotencyKey: draft.key,
            viewedGeneration: draft.generation,
          })
        : await client.remove({
            resourceId,
            idempotencyKey: draft.key,
            viewedGeneration: draft.generation,
          });
      action.markAdmitted();
      setEditing(false);
      setMutationDraft(null);
      setMonitorOperationId(envelope.monitorOperationId);
      await load();
    } catch (e) {
      const nextError = e as LiftrApiError;
      setMutationError(nextError);
      if (nextError.outcomeUnknown) action.markUnknownOutcome();
      else action.markFailed();
    }
  };

  const beginUpdate = () => {
    if (!resource) return;
    const built = buildUpdateFromEditor(specText, referencesText);
    if (!built.ok) {
      setMutationError(new LiftrApiError(null, {
        code: 'LIFTR_REQUEST_INVALID',
        title: 'Invalid input',
        detail: built.error,
      }, 400));
      return;
    }
    const draft: MutationDraft = {
      kind: 'update',
      key: action.begin(),
      generation: gen(resource.generation),
      bodyText: built.bodyText,
    };
    setMutationDraft(draft);
    void sendMutation(draft);
  };

  const beginDelete = () => {
    if (!resource || !window.confirm(`Delete Liftr Resource ${resource.id}?`)) return;
    const draft: MutationDraft = {
      kind: 'delete',
      key: action.begin(),
      generation: gen(resource.generation),
    };
    setMutationDraft(draft);
    void sendMutation(draft);
  };

  if (error) return <ProblemView error={error as unknown as Error & { problem?: unknown }} />;
  if (!resource) return <Typography>Loading...</Typography>;

  const specDisplay = stringifyLosslessJson(resource.spec);
  const referencesDisplay = stringifyLosslessJson(resource.references ?? {});
  const mutable = !['Deleting', 'Deleted'].includes(resource.status.state);

  return (
    <Grid container spacing={3}>
      <Grid item xs={12}>
        <InfoCard
          title={`Resource ${resource.id}`}
          subheader={`${resource.type.name}/${resource.type.version} · owner ${resource.owner.kind}/${resource.owner.id}`}
        >
          <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
            <StateChip state={resource.status.state} />
            <Typography variant="body2">
              Generation {gen(resource.generation)} · observed {gen(resource.status.observedGeneration)}
            </Typography>
            <Button size="small" onClick={() => void load()}>Refresh</Button>
            {mutable && (
              <Button
                size="small"
                onClick={() => {
                  setSpecText(specDisplay);
                  setReferencesText(referencesDisplay);
                  setEditing(true);
                  setMutationError(null);
                }}
              >
                Update
              </Button>
            )}
            {resource.status.state !== 'Deleted' && (
              <Button size="small" color="secondary" onClick={beginDelete}>Delete</Button>
            )}
          </div>
          <Typography variant="body2" style={{ marginTop: 8 }}>
            ResourceID: <code>{resource.id}</code> · ResourceType: <code>{resource.type.name}/{resource.type.version}</code>
          </Typography>
          <Typography variant="body2">
            Latest Operation: {resource.latestOperation
              ? `${resource.latestOperation.id} · ${resource.latestOperation.capability} · ${resource.latestOperation.state} · target generation ${gen(resource.latestOperation.targetGeneration)}`
              : 'none'}
          </Typography>
          {monitorOperationId && (
            <Typography variant="body2">
              Monitoring Operation <code>{monitorOperationId}</code>: {monitor.operation?.state ?? monitor.status}
            </Typography>
          )}
          {monitor.error && <Typography color="error">Operation polling stopped: {monitor.error}</Typography>}
          {editing && (
            <div style={{ marginTop: 16, display: 'grid', gap: 12 }}>
              <SpecEditor value={specText} onChange={setSpecText} />
              <ReferencesEditor value={referencesText} onChange={setReferencesText} />
              <div style={{ display: 'flex', gap: 8 }}>
                <Button variant="contained" color="primary" disabled={action.phase === 'running'} onClick={beginUpdate}>
                  Submit update at generation {gen(resource.generation)}
                </Button>
                <Button onClick={() => setEditing(false)}>Cancel</Button>
              </div>
            </div>
          )}
          {mutationError && (
            <div style={{ marginTop: 12 }}>
              <ProblemView
                error={mutationError as unknown as Error & { problem?: unknown }}
                onReplaySameKey={mutationDraft ? () => void sendMutation(mutationDraft) : undefined}
              />
            </div>
          )}
          <Tabs value={tab} onChange={(_, value: number) => setTab(value)}>
            <Tab label="Desired" />
            <Tab label="Observed" />
            <Tab label="Outputs" />
            <Tab label="Operations" />
          </Tabs>
          {tab === 0 && (
            <>
              <Typography variant="subtitle2">Spec</Typography>
              <pre style={{ overflowX: 'auto', fontSize: 12 }}>{specDisplay}</pre>
              <Typography variant="subtitle2">Desired references</Typography>
              {Object.entries(resource.references ?? {}).length === 0 ? (
                <Typography variant="body2" color="textSecondary">No desired references.</Typography>
              ) : (
                <List dense>
                  {Object.entries(resource.references ?? {}).map(([slot, targets]) => (
                    <ListItem key={slot}>
                      <ListItemText
                        primary={slot}
                        secondary={targets.map((target, index) => (
                          <React.Fragment key={target}>
                            {index > 0 ? ', ' : ''}
                            <Link to={`/liftr/resources/${encodeURIComponent(target)}`}>{target}</Link>
                          </React.Fragment>
                        ))}
                      />
                    </ListItem>
                  ))}
                </List>
              )}
            </>
          )}
          {tab === 1 && (
            <List dense>
              <ListItem>
                <ListItemText primary={`state: ${resource.status.state}`} secondary={`observedGeneration: ${gen(resource.status.observedGeneration)} · updatedAt: ${resource.status.updatedAt}`} />
              </ListItem>
              {resource.status.conditions.map(condition => (
                <ListItem key={condition.type}>
                  <ListItemText
                    primary={`${condition.type}=${condition.status}${condition.reason ? ` (${condition.reason})` : ''}`}
                    secondary={`${condition.message ?? ''}${condition.observedGeneration ? ` · generation ${gen(condition.observedGeneration)}` : ''}${condition.lastTransitionAt ? ` · ${condition.lastTransitionAt}` : ''}`}
                  />
                </ListItem>
              ))}
              {resource.status.conditions.length === 0 && <ListItem><ListItemText primary="No conditions reported." /></ListItem>}
            </List>
          )}
          {tab === 2 && (
            <>
              <OutputsFreshness
                desiredGeneration={resource.generation}
                observedGenerationStatus={resource.status.observedGeneration}
                outputsGeneration={resource.outputs?.observedGeneration}
              />
              {resource.outputs && (
                <table style={{ marginTop: 8 }}>
                  <tbody>
                    {Object.entries(resource.outputs.values).map(([key, value]) => (
                      <tr key={key}>
                        <td><code>{key}</code></td>
                        <td>{isLosslessNumber(value) ? value.toString() : String(value)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </>
          )}
          {tab === 3 && (
            <OperationsPanel
              resourceId={resource.id}
              generation={resource.generation}
              onMutationAdmitted={setMonitorOperationId}
            />
          )}
        </InfoCard>
      </Grid>
    </Grid>
  );
};

interface RetryDraft {
  sourceOperationId: string;
  key: string;
  generation: string;
}

export const OperationsPanel: React.FC<{
  resourceId: string;
  generation: LosslessNumber;
  onMutationAdmitted?: (operationId: string) => void;
}> = ({ resourceId, generation, onMutationAdmitted }) => {
  const client = useLiftrClient();
  const [ops, setOps] = useState<Operation[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [error, setError] = useState<Error | null>(null);
  const [retryDraft, setRetryDraft] = useState<RetryDraft | null>(null);
  const [reload, setReload] = useState(0);

  useEffect(() => {
    let alive = true;
    client.listOperations(resourceId, 20, cursor).then(result => {
      if (!alive) return;
      setOps(result.items);
      setNextCursor(result.nextCursor);
      setError(null);
    }).catch((nextError: Error) => alive && setError(nextError));
    return () => { alive = false; };
  }, [client, resourceId, cursor, reload]);

  const sendRetry = async (draft: RetryDraft) => {
    setError(null);
    try {
      const envelope = await client.retry({
        sourceOperationId: draft.sourceOperationId,
        idempotencyKey: draft.key,
        viewedGeneration: draft.generation,
      });
      setRetryDraft(null);
      onMutationAdmitted?.(envelope.monitorOperationId);
      setReload(value => value + 1);
    } catch (nextError) {
      setError(nextError as Error);
    }
  };

  return (
    <>
      {error && (
        <ProblemView
          error={error as Error & { problem?: unknown }}
          onReplaySameKey={retryDraft ? () => void sendRetry(retryDraft) : undefined}
        />
      )}
      <List dense>
        {ops.map(operation => (
          <ListItem key={operation.id}>
            <ListItemText
              primary={`${operation.id} · ${operation.capability} · ${operation.state} · target generation ${gen(operation.targetGeneration)}${operation.retryOf ? ` · retry of ${operation.retryOf}` : ''}`}
              secondary={`${operation.requestedAt}${operation.completedAt ? ` -> ${operation.completedAt}` : ''}${operation.failure ? ` · ${operation.failure.reason}: ${operation.failure.message ?? ''}` : ''}`}
            />
            {operation.state === 'Failed' && (
              <Button
                size="small"
                onClick={() => {
                  const draft = {
                    sourceOperationId: operation.id,
                    key: newLogicalActionKey(),
                    generation: gen(generation),
                  };
                  setRetryDraft(draft);
                  void sendRetry(draft);
                }}
              >
                Retry
              </Button>
            )}
          </ListItem>
        ))}
        {ops.length === 0 && <ListItem><ListItemText primary="No operations yet." /></ListItem>}
      </List>
      <div style={{ display: 'flex', gap: 8 }}>
        <Button size="small" onClick={() => setReload(value => value + 1)}>Refresh operations</Button>
        {nextCursor && <Button size="small" onClick={() => setCursor(nextCursor)}>Next page</Button>}
      </div>
    </>
  );
};
