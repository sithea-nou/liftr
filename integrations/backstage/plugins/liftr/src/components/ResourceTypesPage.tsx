/**
 * ResourceType discovery (ADR-0009): developer contracts only — no
 * provisioner, platform, or cloud vocabulary anywhere in these views.
 */

import React, { useEffect, useState } from 'react';
import { Grid, Typography, List, ListItem, ListItemText } from '@material-ui/core';
import { InfoCard } from '@backstage/core-components';
import { ResourceTypeDetail, ResourceTypeSummary } from '@liftr/plugin-liftr-common';
import {
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { ProblemView } from './common';

export const ResourceTypesPage: React.FC = () => {
  const client = useLiftrClient();
  const [items, setItems] = useState<ResourceTypeSummary[] | null>(null);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let alive = true;
    client
      .listResourceTypes()
      .then(r => alive && setItems(r.items))
      .catch((e: Error) => alive && setError(e));
    return () => {
      alive = false;
    };
  }, [client]);

  if (error) return <ProblemView error={error as Error & { problem?: unknown }} />;

  return (
    <Grid item xs={12}>
      <InfoCard title="Liftr Resource Types" subheader="Discoverable developer contracts">
        <List dense>
          {(items ?? []).map(t => (
            <ListItem key={`${t.name}/${t.version}`}>
              <ListItemText
                primary={`${t.name} / ${t.version}`}
                secondary={`${t.displayName} — ${t.description} · capabilities: ${t.capabilities.join(', ')}`}
              />
            </ListItem>
          ))}
        </List>
        {!items && <Typography variant="body2">Loading…</Typography>}
      </InfoCard>
    </Grid>
  );
};

export const ResourceTypeIndex: React.FC = () => (
  <Grid item xs={12}>
    <ResourceTypesPage />
  </Grid>
);

export const ResourceTypeDetailPage: React.FC<{ name: string; version: string }> = ({ name, version }) => {
  const client = useLiftrClient();
  const [detail, setDetail] = useState<ResourceTypeDetail | null>(null);
  const [error, setError] = useState<Error | null>(null);

  useEffect(() => {
    let alive = true;
    client
      .getResourceType(name, version)
      .then(d => alive && setDetail(d))
      .catch((e: Error) => alive && setError(e));
    return () => {
      alive = false;
    };
  }, [client, name, version]);

  if (error) return <ProblemView error={error as Error & { problem?: unknown }} />;
  if (!detail) return <Typography>Loading…</Typography>;

  return (
    <Grid container spacing={3}>
      <Grid item xs={12}>
        <InfoCard
          title={`${detail.displayName} (${detail.name}/${detail.version})`}
          subheader={detail.description}
        >
          <Typography variant="body2">Capabilities: {detail.capabilities.join(', ')}</Typography>
          {detail.outputContract && (
            <>
              <Typography variant="subtitle2" style={{ marginTop: 12 }}>
                Output contract
              </Typography>
              <ul>
                {detail.outputContract.fields.map(f => (
                  <li key={f.name}>
                    <code>{f.name}</code> : {f.jsonType}
                    {f.requiredWhenReady ? ' (required when ready)' : ''}
                  </li>
                ))}
              </ul>
            </>
          )}
          <Typography variant="subtitle2" style={{ marginTop: 12 }}>
            Spec schema (JSON Schema draft 2020-12)
          </Typography>
          <pre style={{ overflowX: 'auto', fontSize: 12 }}>
            {stringifyLosslessJson(detail.specSchema)}
          </pre>
        </InfoCard>
      </Grid>
    </Grid>
  );
};
