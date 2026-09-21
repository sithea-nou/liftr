/** Developer-facing ResourceType discovery. Provider registrations are intentionally absent. */

import React, { useEffect, useState } from 'react';
import {
  Box,
  Chip,
  Grid,
  Paper,
  Typography,
  makeStyles,
} from '@material-ui/core';
import { InfoCard, Link } from '@backstage/core-components';
import {
  ReferenceSlotDescriptor,
  ResourceTypeDetail,
  ResourceTypeSummary,
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { LiftrEmptyState, LiftrPageHeading, LoadingSkeleton, ProblemView, humanize } from './common';

const useStyles = makeStyles(theme => ({
  page: { minWidth: 0, width: '100%', margin: 0, '& .MuiGrid-item': { minWidth: 0 } },
  typeCard: { height: '100%', minWidth: 0, padding: theme.spacing(2), borderTop: `3px solid ${theme.palette.primary.main}`, boxSizing: 'border-box', overflowWrap: 'anywhere' },
  capabilities: { display: 'flex', gap: theme.spacing(0.75), flexWrap: 'wrap', marginTop: theme.spacing(1.5) },
  contractGrid: { display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: theme.spacing(2), minWidth: 0, [theme.breakpoints.down('xs')]: { gridTemplateColumns: 'minmax(0, 1fr)' } },
  contractItem: { padding: theme.spacing(2), minWidth: 0, overflowWrap: 'anywhere' },
  code: {
    width: '100%',
    maxWidth: '100%',
    boxSizing: 'border-box',
    overflowX: 'auto',
    WebkitOverflowScrolling: 'touch',
    fontSize: 12,
    lineHeight: 1.6,
    padding: theme.spacing(2),
    borderRadius: theme.shape.borderRadius,
    background: theme.palette.type === 'dark' ? theme.palette.background.default : theme.palette.grey[100],
    border: `1px solid ${theme.palette.divider}`,
  },
}));

export const ResourceTypesPage: React.FC = () => {
  const classes = useStyles();
  const client = useLiftrClient();
  const [items, setItems] = useState<ResourceTypeSummary[] | null>(null);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let alive = true;
    client.listResourceTypes()
      .then(result => alive && setItems(result.items))
      .catch((nextError: Error) => alive && setError(nextError));
    return () => { alive = false; };
  }, [client]);

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}>
        <LiftrPageHeading
          title="Resource Types"
          description="Discover the contracts your platform team makes available to developers."
        />
      </Grid>
      {error && <Grid item xs={12}><ProblemView error={error} /></Grid>}
      {!error && !items && <Grid item xs={12}><LoadingSkeleton rows={5} label="Loading Resource Types" /></Grid>}
      {!error && items?.length === 0 && (
        <Grid item xs={12}>
          <LiftrEmptyState title="No Resource Types available" description="Ask your platform team to publish a developer-facing ResourceType." />
        </Grid>
      )}
      {items?.map(type => (
        <Grid item xs={12} md={6} lg={4} key={`${type.name}/${type.version}`}>
          <Paper variant="outlined" className={classes.typeCard}>
            <Typography variant="overline" color="textSecondary">Resource Type</Typography>
            <Typography variant="h6" component="h2">
              <Link to={`/liftr/resource-types/${encodeURIComponent(type.name)}/${encodeURIComponent(type.version)}`}>
                {type.displayName}
              </Link>
            </Typography>
            <Typography variant="body2" color="textSecondary"><code>{type.name}/{type.version}</code></Typography>
            <Typography variant="body2" style={{ marginTop: 12 }}>{type.description}</Typography>
            <div className={classes.capabilities} aria-label="Capabilities">
              {type.capabilities.map(capability => <Chip size="small" variant="outlined" key={capability} label={humanize(capability)} />)}
            </div>
          </Paper>
        </Grid>
      ))}
    </Grid>
  );
};

/** Retained for classic hosts that embedded the previous index component. */
export const ResourceTypeIndex: React.FC = () => <ResourceTypesPage />;

export const ResourceTypeDetailPage: React.FC<{ name: string; version: string }> = ({ name, version }) => {
  const classes = useStyles();
  const client = useLiftrClient();
  const [detail, setDetail] = useState<ResourceTypeDetail | null>(null);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let alive = true;
    client.getResourceType(name, version)
      .then(nextDetail => alive && setDetail(nextDetail))
      .catch((nextError: Error) => alive && setError(nextError));
    return () => { alive = false; };
  }, [client, name, version]);

  if (error) return <ProblemView error={error} />;
  if (!detail) return <LoadingSkeleton rows={7} label="Loading ResourceType contract" />;

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}>
        <LiftrPageHeading title={detail.displayName} description={detail.description} />
      </Grid>
      <Grid item xs={12}>
        <InfoCard title={`${detail.name}/${detail.version}`} subheader="Developer contract">
          <Typography variant="subtitle2">Capabilities</Typography>
          <Box display="flex" gridGap={8} flexWrap="wrap" mt={1} mb={3}>
            {detail.capabilities.map(capability => <Chip size="small" color="primary" variant="outlined" key={capability} label={humanize(capability)} />)}
          </Box>

          <div className={classes.contractGrid}>
            <Paper variant="outlined" className={classes.contractItem}>
              <Typography variant="h6" component="h2">Dependencies</Typography>
              {!detail.referenceContract?.slots.length ? (
                <Typography variant="body2" color="textSecondary">This ResourceType has no dependency slots.</Typography>
              ) : detail.referenceContract.slots.map(slot => <ReferenceContractView key={slot.name} slot={slot} />)}
            </Paper>
            <Paper variant="outlined" className={classes.contractItem}>
              <Typography variant="h6" component="h2">Outputs</Typography>
              {!detail.outputContract?.fields.length ? (
                <Typography variant="body2" color="textSecondary">This ResourceType does not publish outputs.</Typography>
              ) : detail.outputContract.fields.map(field => (
                <Box key={field.name} display="flex" justifyContent="space-between" py={0.75}>
                  <Typography variant="body2"><code>{field.name}</code></Typography>
                  <Typography variant="caption" color="textSecondary">
                    {field.jsonType}{field.requiredWhenReady ? ' · required when Ready' : ''}
                  </Typography>
                </Box>
              ))}
            </Paper>
          </div>

          <Box mt={3}>
            <Typography variant="h6" component="h2">Configuration schema</Typography>
            <Typography variant="body2" color="textSecondary" paragraph>
              JSON Schema draft 2020-12. The Resource spec remains intentionally provider-neutral and opaque to Liftr core.
            </Typography>
            <pre className={classes.code}>{stringifyLosslessJson(detail.specSchema)}</pre>
          </Box>
        </InfoCard>
      </Grid>
    </Grid>
  );
};

export const ReferenceContractView: React.FC<{ slot: ReferenceSlotDescriptor }> = ({ slot }) => (
  <Box mt={1.5} mb={2}>
    <Box display="flex" justifyContent="space-between" alignItems="baseline">
      <Typography variant="subtitle2">{humanize(slot.name)}</Typography>
      <Typography variant="caption" color="textSecondary">
        {slot.minItems > 0 ? 'Required' : 'Optional'} · {slot.minItems === slot.maxItems ? `Exactly ${slot.maxItems}` : `${slot.minItems}–${slot.maxItems}`}
      </Typography>
    </Box>
    <Typography variant="caption" color="textSecondary" display="block">Allowed Resource Types</Typography>
    {slot.allowedTargetTypes.map(target => (
      <Typography variant="body2" key={`${target.name}/${target.version}`}><code>{target.name}/{target.version}</code></Typography>
    ))}
  </Box>
);
