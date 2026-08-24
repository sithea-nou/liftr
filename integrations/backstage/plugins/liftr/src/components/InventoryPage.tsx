/**
 * Resource inventory — the authoritative M15 listing.
 *
 * GET /v1/resources is the ONLY inventory source. Filters map 1:1 to M15
 * query parameters; filter changes restart cursor traversal; there is no
 * client-side global search, no counts, and no persisted state.
 */

import { Grid, Typography } from '@material-ui/core';
import React, { useCallback, useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  ResourceSummary,
  RESOURCE_STATES,
  ValidatedResourceListQuery,
} from '@liftr/plugin-liftr-common';
import { InfoCard, Link, Table } from '@backstage/core-components';
import { ResourceTypeIndex } from './ResourceTypesPage';
import { LiftrApiError } from '../api/client';
import { ProblemView, OwnerRefView, StateChip, gen } from './common';
import { useLiftrClient } from '../hooks/useLiftrClient';

export const InventoryPage: React.FC = () => {
  const client = useLiftrClient();
  const navigate = useNavigate();
  const [query, setQuery] = useState<ValidatedResourceListQuery>({ limit: 20 });
  const [items, setItems] = useState<ResourceSummary[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [cursorStack, setCursorStack] = useState<Array<string | undefined>>([undefined]);
  const [error, setError] = useState<LiftrApiError | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    setNextCursor(undefined);
    client
      .listResources(query)
      .then(r => {
        if (!alive) return;
        setItems(r.items);
        setNextCursor(r.nextCursor);
        setError(null);
      })
      .catch((e: LiftrApiError) => alive && setError(e))
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
    };
  }, [client, JSON.stringify(query)]);

  const applyFilter = useCallback(
    (patch: Partial<ValidatedResourceListQuery>) => {
      // Filter changes restart traversal (cursor binding on the server).
      setCursorStack([undefined]);
      setQuery(prev => ({ ...prev, ...patch, cursor: undefined }));
    },
    [],
  );

  const goNext = () => {
    if (!nextCursor) return;
    setCursorStack(stack => [...stack, nextCursor]);
    setQuery(prev => ({ ...prev, cursor: nextCursor }));
  };
  const goPrev = () => {
    setCursorStack(stack => (stack.length > 1 ? stack.slice(0, -1) : stack));
    setQuery(prev => ({ ...prev, cursor: cursorStack[cursorStack.length - 2] ?? undefined }));
  };

  if (error) {
    return <ProblemView error={error as unknown as Error & { problem?: unknown }} />;
  }

  const columns: Array<{ title: string; field?: string; render?: (r: ResourceSummary) => React.ReactNode }> = [
    {
      title: 'ID',
      field: 'id',
      render: (r: ResourceSummary) => (
        <Link to={`/liftr/resources/${encodeURIComponent(r.id)}`}>{r.id}</Link>
      ),
    },
    { title: 'Type', render: (r: ResourceSummary) => `${r.type.name}/${r.type.version}` },
    { title: 'Owner', render: (r: ResourceSummary) => <OwnerRefView owner={r.owner} /> },
    { title: 'State', render: (r: ResourceSummary) => <StateChip state={r.status.state} /> },
    { title: 'Generation', render: (r: ResourceSummary) => `${gen(r.status.observedGeneration)} / ${gen(r.generation)}` },
    {
      title: 'Latest Operation',
      render: (r: ResourceSummary) =>
        r.latestOperation ? (
          <span>
            {r.latestOperation.capability} · {r.latestOperation.state}
          </span>
        ) : (
          '—'
        ),
    },
  ];

  return (
    <Grid container spacing={3}>
      <Grid item xs={12}>
        <InfoCard title="Liftr Resources" subheader="Authorized inventory served live from Liftr (GET /v1/resources)">
          <InventoryFilters value={query} onChange={applyFilter} />
          {error === null && items.length === 0 && !loading && (
            <Typography variant="body2" color="textSecondary">
              No resources visible to you. Visibility comes from your Liftr memberships.
            </Typography>
          )}
          <Table
            options={{ search: false, paging: false, toolbar: false }}
            isLoading={loading}
            columns={columns}
            data={items}
            onRowClick={(row: unknown) => {
              const r = row as ResourceSummary;
              if (r?.id) navigate(`/liftr/resources/${encodeURIComponent(r.id)}`);
            }}
          />
          <div style={{ marginTop: 8, display: 'flex', gap: 8 }}>
            <button type="button" disabled={cursorStack.length <= 1 || loading} onClick={goPrev}>
              Previous page
            </button>
            <button type="button" disabled={!nextCursor || loading} onClick={goNext}>
              Next page
            </button>
          </div>
        </InfoCard>
      </Grid>
      <ResourceTypeIndex />
    </Grid>
  );
};

const InventoryFilters: React.FC<{
  value: ValidatedResourceListQuery;
  onChange: (patch: Partial<ValidatedResourceListQuery>) => void;
}> = ({ value, onChange }) => (
  <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginBottom: 12 }}>
    <input
      aria-label="owner kind"
      placeholder="owner kind"
      defaultValue={value.ownerKind ?? ''}
      onBlur={e =>
        onChange(
          e.target.value.trim() === ''
            ? { ownerKind: undefined, ownerId: undefined }
            : { ownerKind: e.target.value.trim(), ownerId: value.ownerId ?? '' } as ValidatedResourceListQuery,
        )
      }
    />
    <input
      aria-label="owner id"
      placeholder="owner id"
      defaultValue={value.ownerId ?? ''}
      onBlur={e =>
        onChange(
          e.target.value.trim() === ''
            ? { ownerKind: undefined, ownerId: undefined }
            : { ownerKind: value.ownerKind ?? '', ownerId: e.target.value.trim() } as ValidatedResourceListQuery,
        )
      }
    />
    <input
      aria-label="type"
      placeholder="type"
      defaultValue={value.type ?? ''}
      onBlur={e => onChange({ type: e.target.value.trim() === '' ? undefined : e.target.value.trim(), version: undefined })}
    />
    <input
      aria-label="version"
      placeholder="version"
      disabled={!value.type}
      defaultValue={value.version ?? ''}
      onBlur={e => onChange({ version: e.target.value.trim() === '' ? undefined : e.target.value.trim() })}
    />
    <select
      aria-label="state"
      value={value.state ?? ''}
      onChange={e => onChange({ state: (e.target.value || undefined) as ValidatedResourceListQuery['state'] })}
    >
      <option value="">any state</option>
      {RESOURCE_STATES.map(s => (
        <option key={s} value={s}>
          {s}
        </option>
      ))}
    </select>
    <label style={{ alignSelf: 'center' }}>
      <input
        type="checkbox"
        checked={value.includeDeleted === true}
        onChange={e => onChange({ includeDeleted: e.target.checked || undefined })}
      />{' '}
      include deleted
    </label>
  </div>
);
