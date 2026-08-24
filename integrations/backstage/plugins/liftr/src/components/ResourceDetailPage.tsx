/**
 * Resource detail: Desired / Observed / Outputs / Operations are kept
 * structurally separate (M7 semantics). Outputs carry explicit generation
 * freshness; endpoint values are data, never "health".
 */

import React, { useCallback, useEffect, useState } from 'react';
import { Grid, Tab, Tabs, Typography, Button, List, ListItem, ListItemText } from '@material-ui/core';
import { InfoCard } from '@backstage/core-components';
import {
  Operation,
  Resource,
  parseLosslessJson,
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { ProblemView, OutputsFreshness, StateChip, OwnerRefView, gen } from './common';
import { LiftrApiError } from '../api/client';

export const ResourceDetailPage: React.FC<{ resourceId: string }> = ({ resourceId }) => {
  const client = useLiftrClient();
  const [resource, setResource] = useState<Resource | null>(null);
  const [error, setError] = useState<LiftrApiError | null>(null);
  const [tab, setTab] = useState(0);

  const load = useCallback(() => {
    client
      .getResource(resourceId)
      .then(r => {
        setResource(r);
        setError(null);
      })
      .catch((e: LiftrApiError) => setError(e));
  }, [client, resourceId]);

  useEffect(() => {
    load();
  }, [load]);

  if (error) return <ProblemView error={error as unknown as Error & { problem?: unknown }} />;
  if (!resource) return <Typography>Loading…</Typography>;

  const specText = stringifyLosslessJson(parseLosslessJson(JSON.stringify(resource.spec === undefined ? {} : resource.spec)));

  return (
    <Grid container spacing={3}>
      <Grid item xs={12}>
        <InfoCard
          title={`Resource ${resource.id}`}
          subheader={`${resource.type.name}/${resource.type.version} · owner ${resource.owner.kind}/${resource.owner.id}`}
        >
          <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
            <StateChip state={resource.status.state} />
            <Typography variant="body2">
              Generation {gen(resource.generation)} · observed {gen(resource.status.observedGeneration)}
            </Typography>
            <Button size="small" onClick={load}>
              Refresh
            </Button>
          </div>
          <Tabs value={tab} onChange={(_, v: number) => setTab(v)}>
            <Tab label="Desired" />
            <Tab label="Observed" />
            <Tab label="Outputs" />
            <Tab label="Operations" />
          </Tabs>
          {tab === 0 && (
            <pre style={{ overflowX: 'auto', fontSize: 12 }}>{specText}</pre>
          )}
          {tab === 1 && (
            <List dense>
              <ListItem>
                <ListItemText primary={`state: ${resource.status.state}`} secondary={`observedGeneration: ${gen(resource.status.observedGeneration)} · updatedAt: ${resource.status.updatedAt}`} />
              </ListItem>
              {resource.status.conditions.map(c => (
                <ListItem key={c.type}>
                  <ListItemText
                    primary={`${c.type}=${c.status}${c.reason ? ` (${c.reason})` : ''}`}
                    secondary={`${c.message ?? ''}${
                      c.observedGeneration ? ` · generation ${gen(c.observedGeneration)}` : ''
                    }${c.lastTransitionAt ? ` · ${c.lastTransitionAt}` : ''}`}
                  />
                </ListItem>
              ))}
              {resource.status.conditions.length === 0 && (
                <ListItem><ListItemText primary="No conditions reported." /></ListItem>
              )}
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
                    {Object.entries(resource.outputs.values).map(([k, v]) => (
                      <tr key={k}>
                        <td><code>{k}</code></td>
                        <td>{typeof v === 'object' ? JSON.stringify(v) : String(v)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </>
          )}
          {tab === 3 && <OperationsPanel resourceId={resource.id} />}
        </InfoCard>
      </Grid>
    </Grid>
  );
};

export const OperationsPanel: React.FC<{ resourceId: string; onRetry?: () => void }> = ({
  resourceId,
}) => {
  const client = useLiftrClient();
  const [ops, setOps] = useState<Operation[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let alive = true;
    client
      .listOperations(resourceId, 20, cursor)
      .then(r => {
        if (!alive) return;
        setOps(r.items);
        setNextCursor(r.nextCursor);
        setError(null);
      })
      .catch((e: Error) => alive && setError(e));
    return () => {
      alive = false;
    };
  }, [client, resourceId, cursor]);

  if (error) return <ProblemView error={error as Error & { problem?: unknown }} />;

  return (
    <>
      <List dense>
        {ops.map(op => (
          <ListItem key={op.id}>
            <ListItemText
              primary={
                <>
                  {op.capability} · {op.state} · target gen {gen(op.targetGeneration)}
                  {op.retryOf ? ` · retry of ${op.retryOf}` : ''}
                </>
              }
              secondary={`${op.requestedAt}${op.completedAt ? ` → ${op.completedAt}` : ''}${
                op.failure ? ` · ${op.failure.reason}: ${op.failure.message ?? ''}` : ''
              }`}
            />
          </ListItem>
        ))}
        {ops.length === 0 && <ListItem><ListItemText primary="No operations yet." /></ListItem>}
      </List>
      {nextCursor && (
        <Button size="small" onClick={() => setCursor(nextCursor)}>
          Next page
        </Button>
      )}
    </>
  );
};
