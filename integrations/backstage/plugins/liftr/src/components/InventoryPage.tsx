/** Authoritative M15 resource inventory. All filters map directly to GET /v1/resources. */

import React, { useCallback, useEffect, useState } from 'react';
import {
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  Grid,
  MenuItem,
  TextField,
  Typography,
  makeStyles,
} from '@material-ui/core';
import AddIcon from '@material-ui/icons/Add';
import { InfoCard, Link, Table } from '@backstage/core-components';
import { useNavigate } from 'react-router-dom';
import {
  RESOURCE_STATES,
  ResourceSummary,
  ValidatedResourceListQuery,
} from '@liftr/plugin-liftr-common';
import { LiftrApiError } from '../api/client';
import { useLiftrClient } from '../hooks/useLiftrClient';
import {
  LiftrEmptyState,
  LiftrPageHeading,
  OperationStateChip,
  OwnerRefView,
  ProblemView,
  StateChip,
  formatTimestamp,
} from './common';

const useStyles = makeStyles(theme => ({
  page: {
    minWidth: 0,
    width: '100%',
    margin: 0,
    '& .MuiGrid-item': { minWidth: 0 },
  },
  tableRegion: {
    width: '100%',
    maxWidth: '100%',
    overflowX: 'auto',
    WebkitOverflowScrolling: 'touch',
    borderRadius: theme.shape.borderRadius,
    '& table': { minWidth: 880 },
  },
  resourceId: {
    overflowWrap: 'anywhere',
    wordBreak: 'break-word',
  },
  operationSummary: {
    minWidth: 0,
    flexWrap: 'wrap',
  },
  pagination: { flexWrap: 'wrap' },
}));

export const InventoryPage: React.FC = () => {
  const classes = useStyles();
  const client = useLiftrClient();
  const navigate = useNavigate();
  const [query, setQuery] = useState<ValidatedResourceListQuery>({ limit: 20 });
  const [items, setItems] = useState<ResourceSummary[]>([]);
  const [nextCursor, setNextCursor] = useState<string>();
  const [cursorStack, setCursorStack] = useState<Array<string | undefined>>([undefined]);
  const [error, setError] = useState<LiftrApiError | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    setLoading(true);
    setNextCursor(undefined);
    client.listResources(query)
      .then(result => {
        if (!alive) return;
        setItems(result.items);
        setNextCursor(result.nextCursor);
        setError(null);
      })
      .catch((nextError: LiftrApiError) => alive && setError(nextError))
      .finally(() => alive && setLoading(false));
    return () => { alive = false; };
  }, [client, JSON.stringify(query)]);

  const applyFilter = useCallback((patch: Partial<ValidatedResourceListQuery>) => {
    setCursorStack([undefined]);
    setQuery(previous => ({ ...previous, ...patch, cursor: undefined }));
  }, []);

  const goPrevious = () => {
    setCursorStack(stack => (stack.length > 1 ? stack.slice(0, -1) : stack));
    setQuery(previous => ({ ...previous, cursor: cursorStack[cursorStack.length - 2] }));
  };

  const columns = [
    {
      title: 'Resource',
      field: 'id',
      render: (resource: ResourceSummary) => (
        <Box py={0.5}>
          <Link to={`/liftr/resources/${encodeURIComponent(resource.id)}`}>
            <Typography className={classes.resourceId} component="span" variant="subtitle2">{resource.id}</Typography>
          </Link>
          <Typography variant="caption" display="block" color="textSecondary">Generation {resource.generation.toString()}</Typography>
        </Box>
      ),
    },
    { title: 'Type', render: (resource: ResourceSummary) => `${resource.type.name}/${resource.type.version}` },
    { title: 'State', render: (resource: ResourceSummary) => <StateChip state={resource.status.state} /> },
    {
      title: 'Latest operation',
      render: (resource: ResourceSummary) => resource.latestOperation ? (
        <Box className={classes.operationSummary} display="flex" alignItems="center" gridGap={8}>
          <Typography variant="body2" style={{ textTransform: 'capitalize' }}>{resource.latestOperation.capability}</Typography>
          <OperationStateChip state={resource.latestOperation.state} />
        </Box>
      ) : <Typography color="textSecondary">—</Typography>,
    },
    { title: 'Owner', render: (resource: ResourceSummary) => <OwnerRefView owner={resource.owner} /> },
    { title: 'Updated', render: (resource: ResourceSummary) => formatTimestamp(resource.updatedAt) },
  ];

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}>
        <LiftrPageHeading
          title="Liftr Resources"
          description="Manage platform Resources without depending on how they are implemented."
        />
      </Grid>
      <Grid item xs={12}>
        <InfoCard>
          <InventoryFilters value={query} onChange={applyFilter} />
          {error && <ProblemView error={error} />}
          {!error && !loading && items.length === 0 ? (
            <LiftrEmptyState
              title="No Resources found"
              description="Liftr Resources are the developer-facing services and infrastructure your platform makes available. Create one or adjust the filters above."
              action={(
                <Button color="primary" variant="contained" startIcon={<AddIcon />} onClick={() => navigate('/liftr/create')}>
                  Create Resource
                </Button>
              )}
            />
          ) : !error ? (
            <div
              className={classes.tableRegion}
              role="region"
              aria-label="Resource inventory table"
              tabIndex={0}
            >
              <Table
                title="Resources"
                options={{ search: false, paging: false, padding: 'dense' }}
                isLoading={loading}
                columns={columns}
                data={items}
                onRowClick={(row: unknown) => {
                  const resource = row as ResourceSummary;
                  if (resource?.id) navigate(`/liftr/resources/${encodeURIComponent(resource.id)}`);
                }}
              />
            </div>
          ) : null}
          {!error && items.length > 0 && (
            <Box className={classes.pagination} mt={2} display="flex" gridGap={8}>
              <Button variant="outlined" size="small" disabled={cursorStack.length <= 1 || loading} onClick={goPrevious}>
                Previous
              </Button>
              <Button
                variant="outlined"
                size="small"
                disabled={!nextCursor || loading}
                onClick={() => {
                  if (!nextCursor) return;
                  setCursorStack(stack => [...stack, nextCursor]);
                  setQuery(previous => ({ ...previous, cursor: nextCursor }));
                }}
              >
                Next
              </Button>
            </Box>
          )}
        </InfoCard>
      </Grid>
    </Grid>
  );
};

const InventoryFilters: React.FC<{
  value: ValidatedResourceListQuery;
  onChange: (patch: Partial<ValidatedResourceListQuery>) => void;
}> = ({ value, onChange }) => {
  const [ownerKind, setOwnerKind] = useState(value.ownerKind ?? '');
  const [ownerId, setOwnerId] = useState(value.ownerId ?? '');
  const [type, setType] = useState(value.type ?? '');
  const [state, setState] = useState<NonNullable<ValidatedResourceListQuery['state']> | ''>(value.state ?? '');
  const [includeDeleted, setIncludeDeleted] = useState(value.includeDeleted === true);
  const apply = () => onChange({
    ownerKind: ownerKind.trim() && ownerId.trim() ? ownerKind.trim() : undefined,
    ownerId: ownerKind.trim() && ownerId.trim() ? ownerId.trim() : undefined,
    type: type.trim() || undefined,
    version: undefined,
    state: state || undefined,
    includeDeleted: includeDeleted || state === 'Deleted' || undefined,
  });
  return (
  <Box mb={2} display="flex" gridGap={12} flexWrap="wrap" alignItems="center" aria-label="Resource filters">
    <TextField
      label="Owner kind"
      variant="outlined"
      size="small"
      value={ownerKind}
      onChange={event => setOwnerKind(event.target.value)}
    />
    <TextField
      label="Owner ID"
      variant="outlined"
      size="small"
      value={ownerId}
      onChange={event => setOwnerId(event.target.value)}
    />
    <TextField
      label="Resource type"
      variant="outlined"
      size="small"
      value={type}
      onChange={event => setType(event.target.value)}
    />
    <TextField
      select
      label="State"
      variant="outlined"
      size="small"
      value={state}
      style={{ minWidth: 140 }}
      onChange={event => {
        setState(event.target.value as NonNullable<ValidatedResourceListQuery['state']> | '');
        if (event.target.value === 'Deleted') setIncludeDeleted(true);
      }}
    >
      <MenuItem value="">Any state</MenuItem>
      {RESOURCE_STATES.map(state => <MenuItem key={state} value={state}>{state}</MenuItem>)}
    </TextField>
    <FormControlLabel
      control={(
        <Checkbox
          color="primary"
          checked={includeDeleted}
          onChange={event => {
            setIncludeDeleted(event.target.checked);
            if (!event.target.checked && state === 'Deleted') setState('');
          }}
        />
      )}
      label="Include deleted"
    />
    <Button color="primary" variant="outlined" size="small" onClick={apply}>Apply filters</Button>
    <Button size="small" onClick={() => {
      setOwnerKind(''); setOwnerId(''); setType(''); setState(''); setIncludeDeleted(false);
      onChange({ ownerKind: undefined, ownerId: undefined, type: undefined, version: undefined, state: undefined, includeDeleted: undefined });
    }}>Clear</Button>
  </Box>
  );
};
