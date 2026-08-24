/**
 * Standalone operation view: polls only the authoritative monitorOperationId
 * handed over via navigation state (Correction 2) or fetched explicitly.
 */

import React, { useState } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { Grid, Typography, Button } from '@material-ui/core';
import { InfoCard } from '@backstage/core-components';
import { Operation } from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useOperationMonitor } from '../hooks/useOperationMonitor';
import { ProblemView, gen } from './common';

interface LocationState {
  monitorOperationId?: string;
}

export const OperationPage: React.FC = () => {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const client = useLiftrClient();
  const [manualOp, setManualOp] = useState<Operation | null>(null);
  const [error, setError] = useState<Error | null>(null);

  const monitorId =
    (location.state as LocationState | null)?.monitorOperationId ?? manualOp?.id ?? undefined;
  const { operation, status, refresh } = useOperationMonitor(client, monitorId);

  const shown = operation ?? manualOp;

  if (!monitorId) {
    return (
      <Grid container spacing={3}>
        <Grid item xs={12} md={8}>
          <InfoCard title={`Operation ${id}`}>
            <Typography variant="body2">
              This link was opened without an authoritative monitor reference.
            </Typography>
            <Button
              size="small"
              onClick={() => {
                client
                  .getOperation(id!)
                  .then(op => setManualOp(op))
                  .catch((e: Error) => setError(e));
              }}
            >
              Fetch once
            </Button>
            {error && <ProblemView error={error as Error & { problem?: unknown }} />}
            {shown && (
              <Typography variant="body2" style={{ marginTop: 12 }}>
                {shown.capability} · {shown.state} · target generation {gen(shown.targetGeneration)}
                {shown.retryOf ? ` · retryOf ${shown.retryOf}` : ''}
              </Typography>
            )}
          </InfoCard>
        </Grid>
      </Grid>
    );
  }

  return (
    <Grid container spacing={3}>
      <Grid item xs={12} md={8}>
        <InfoCard
          title={`Monitoring ${monitorId}`}
          subheader={
            status === 'polling'
              ? 'Polling every few seconds while this tab is open…'
              : `Terminal state: ${status}`
          }
        >
          <Button size="small" onClick={refresh}>
            Refresh now
          </Button>
          {shown ? (
            <>
              <Typography variant="body2" style={{ marginTop: 8 }}>
                {shown.capability} · {shown.state} · resource {shown.resourceId} · target
                generation {gen(shown.targetGeneration)}
                {shown.retryOf ? ` · retryOf ${shown.retryOf}` : ''}
              </Typography>
              {shown.failure && (
                <Typography variant="body2" color="secondary">
                  Failure: {shown.failure.reason}
                  {shown.failure.message ? ` — ${shown.failure.message}` : ''}
                </Typography>
              )}
            </>
          ) : (
            <Typography variant="body2">Waiting for first poll…</Typography>
          )}
        </InfoCard>
      </Grid>
    </Grid>
  );
};
