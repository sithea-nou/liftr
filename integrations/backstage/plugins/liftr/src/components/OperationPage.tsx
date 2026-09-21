/** Standalone public Operation view. No private phase, execution handle, or provisioner metadata. */

import React, { useState } from 'react';
import { useLocation, useParams } from 'react-router-dom';
import { Box, Button, Grid, Typography, makeStyles } from '@material-ui/core';
import RefreshIcon from '@material-ui/icons/Refresh';
import { InfoCard } from '@backstage/core-components';
import { Operation } from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useOperationMonitor } from '../hooks/useOperationMonitor';
import {
  LiftrPageHeading,
  LoadingSkeleton,
  OperationStateChip,
  ProblemView,
  formatTimestamp,
  gen,
  humanize,
} from './common';

interface LocationState { monitorOperationId?: string }

const useStyles = makeStyles({
  page: { minWidth: 0, width: '100%', margin: 0, '& .MuiGrid-item': { minWidth: 0 } },
  breakable: { overflowWrap: 'anywhere', wordBreak: 'break-word' },
});

export const OperationPage: React.FC = () => {
  const classes = useStyles();
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const client = useLiftrClient();
  const [manualOperation, setManualOperation] = useState<Operation | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const monitorId = (location.state as LocationState | null)?.monitorOperationId ?? manualOperation?.id;
  const { operation, status, refresh } = useOperationMonitor(client, monitorId);
  const shown = operation ?? manualOperation;

  const fetchOnce = () => {
    if (!id) return;
    client.getOperation(id).then(setManualOperation).catch((nextError: Error) => setError(nextError));
  };

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}><LiftrPageHeading title="Lifecycle Operation" description={id ? `Operation ${id}` : undefined} /></Grid>
      <Grid item xs={12} md={9}>
        <InfoCard title={monitorId ? `Operation ${monitorId}` : `Operation ${id}`} subheader="Authoritative asynchronous lifecycle status">
          {!monitorId && !shown && (
            <>
              <Typography variant="body2" paragraph>This link was opened without an admitted monitor reference. Fetch this public Operation once by ID.</Typography>
              <Button variant="outlined" size="small" onClick={fetchOnce}>Fetch Operation</Button>
            </>
          )}
          {monitorId && !shown && <LoadingSkeleton rows={3} label="Loading operation" />}
          {shown && (
            <Box display="grid" gridGap={16}>
              <Box display="flex" justifyContent="space-between" alignItems="center" flexWrap="wrap" gridGap={8}>
                <Typography variant="h6" style={{ textTransform: 'capitalize' }}>{shown.capability} · Generation {gen(shown.targetGeneration)}</Typography>
                <OperationStateChip state={shown.state} />
              </Box>
              <Grid container spacing={2}>
                <Grid item xs={6} sm={3}><Typography variant="caption" color="textSecondary">Resource</Typography><Typography className={classes.breakable} variant="body2">{shown.resourceId}</Typography></Grid>
                <Grid item xs={6} sm={3}><Typography variant="caption" color="textSecondary">Requested</Typography><Typography variant="body2">{formatTimestamp(shown.requestedAt)}</Typography></Grid>
                <Grid item xs={6} sm={3}><Typography variant="caption" color="textSecondary">Started</Typography><Typography variant="body2">{formatTimestamp(shown.startedAt)}</Typography></Grid>
                <Grid item xs={6} sm={3}><Typography variant="caption" color="textSecondary">Completed</Typography><Typography variant="body2">{formatTimestamp(shown.completedAt)}</Typography></Grid>
              </Grid>
              {shown.retryOf && <Typography className={classes.breakable} variant="body2">Retry of <code>{shown.retryOf}</code></Typography>}
              {shown.failure && (
                <Typography variant="body2" color="error">
                  {humanize(shown.failure.reason)}{shown.failure.message ? ` — ${shown.failure.message}` : ''}
                </Typography>
              )}
              <Box><Button size="small" startIcon={<RefreshIcon />} onClick={monitorId ? refresh : fetchOnce}>Refresh</Button></Box>
            </Box>
          )}
          {monitorId && <Typography variant="caption" color="textSecondary">Monitor status: {status}</Typography>}
          {error && <Box mt={2}><ProblemView error={error} /></Box>}
        </InfoCard>
      </Grid>
    </Grid>
  );
};
