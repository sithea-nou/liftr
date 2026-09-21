/** Create and shared update editors. Request bodies remain lossless and use the existing M21 semantics. */

import React, { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import {
  Box,
  Button,
  Chip,
  Grid,
  MenuItem,
  Paper,
  TextField,
  Typography,
  makeStyles,
} from '@material-ui/core';
import { InfoCard } from '@backstage/core-components';
import { identityApiRef, useApi } from '@backstage/core-plugin-api';
import {
  ReferenceSlotDescriptor,
  ResourceReferences,
  ResourceSummary,
  ResourceTypeDetail,
  ResourceTypeSummary,
  buildCreateResourceBody,
  buildUpdateResourceBody,
  parseLosslessJson,
  stringifyLosslessJson,
} from '@liftr/plugin-liftr-common';
import { LiftrApiError, LiftrFrontendClient } from '../api/client';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useIdempotentAction } from '../hooks/useIdempotentAction';
import { LiftrPageHeading, LoadingSkeleton, ProblemView, StateChip, humanize } from './common';

const useStyles = makeStyles(theme => ({
  page: { minWidth: 0, width: '100%', margin: 0, '& .MuiGrid-item': { minWidth: 0 } },
  section: { padding: theme.spacing(2), marginBottom: theme.spacing(2), minWidth: 0, maxWidth: '100%', boxSizing: 'border-box', overflowWrap: 'anywhere' },
  sectionHeading: { display: 'flex', gap: theme.spacing(1.5), alignItems: 'center', marginBottom: theme.spacing(2), minWidth: 0 },
  number: {
    width: 28,
    height: 28,
    borderRadius: '50%',
    display: 'inline-flex',
    alignItems: 'center',
    justifyContent: 'center',
    color: theme.palette.primary.contrastText,
    background: theme.palette.primary.main,
    fontWeight: 700,
  },
  editor: {
    width: '100%',
    minHeight: 170,
    boxSizing: 'border-box',
    resize: 'vertical',
    padding: theme.spacing(1.5),
    borderRadius: theme.shape.borderRadius,
    border: `1px solid ${theme.palette.divider}`,
    color: theme.palette.text.primary,
    background: theme.palette.background.default,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace',
    fontSize: 13,
    lineHeight: 1.55,
    '&:focus': { outline: `2px solid ${theme.palette.primary.main}`, outlineOffset: 1 },
  },
}));

const FormSection: React.FC<{ number: number; title: string; children: React.ReactNode }> = ({ number, title, children }) => {
  const classes = useStyles();
  return (
    <Paper variant="outlined" className={classes.section}>
      <div className={classes.sectionHeading}>
        <span className={classes.number} aria-hidden="true">{number}</span>
        <Typography variant="h6" component="h2">{title}</Typography>
      </div>
      {children}
    </Paper>
  );
};

async function loadVisibleInventory(client: LiftrFrontendClient): Promise<ResourceSummary[]> {
  const resources: ResourceSummary[] = [];
  let cursor: string | undefined;
  // Bounded traversal protects the browser while still covering a generous authorized inventory.
  for (let page = 0; page < 20; page += 1) {
    const result = await client.listResources({ limit: 100, cursor });
    resources.push(...result.items);
    cursor = result.nextCursor;
    if (!cursor) break;
  }
  return resources;
}

export const CreateResourcePage: React.FC = () => {
  const classes = useStyles();
  const client = useLiftrClient();
  const navigate = useNavigate();
  const identityApi = useApi(identityApiRef);
  const action = useIdempotentAction();
  const [types, setTypes] = useState<ResourceTypeSummary[]>([]);
  const [typeDetail, setTypeDetail] = useState<ResourceTypeDetail | null>(null);
  const [typeName, setTypeName] = useState('');
  const [typeVersion, setTypeVersion] = useState('');
  const [resourceId, setResourceId] = useState('');
  const [ownerKind, setOwnerKind] = useState('team');
  const [ownerId, setOwnerId] = useState('');
  const [specText, setSpecText] = useState('{\n  \n}');
  const [references, setReferences] = useState<ResourceReferences>({});
  const [referencesText, setReferencesText] = useState('{}');
  const [visibleResources, setVisibleResources] = useState<ResourceSummary[]>([]);
  const [loadingTypes, setLoadingTypes] = useState(true);
  const [pickerError, setPickerError] = useState<Error | null>(null);
  const [error, setError] = useState<(LiftrApiError & { outcomeUnknown?: boolean }) | null>(null);
  const [admittedMonitorId, setAdmittedMonitorId] = useState<string | null>(null);
  const [bodyDraft, setBodyDraft] = useState<string | null>(null);
  const [suggestions, setSuggestions] = useState<string[]>([]);

  useEffect(() => {
    let alive = true;
    client.listResourceTypes()
      .then(result => {
        if (!alive) return;
        setTypes(result.items);
        if (result.items[0]) {
          setTypeName(result.items[0].name);
          setTypeVersion(result.items[0].version);
        }
      })
      .catch((nextError: LiftrApiError) => alive && setError(nextError))
      .finally(() => alive && setLoadingTypes(false));
    loadVisibleInventory(client)
      .then(resources => alive && setVisibleResources(resources))
      .catch((nextError: Error) => alive && setPickerError(nextError));
    return () => { alive = false; };
  }, [client]);

  useEffect(() => {
    if (!typeName || !typeVersion) return undefined;
    let alive = true;
    setTypeDetail(null);
    setReferences({});
    setReferencesText('{}');
    client.getResourceType(typeName, typeVersion)
      .then(detail => alive && setTypeDetail(detail))
      .catch((nextError: LiftrApiError) => alive && setError(nextError));
    return () => { alive = false; };
  }, [client, typeName, typeVersion]);

  useEffect(() => {
    let alive = true;
    void identityApi.getBackstageIdentity()
      .then(identity => alive && setSuggestions(
        identity.ownershipEntityRefs
          .filter(ref => ref.startsWith('group:'))
          .map(ref => ref.replace('group:default/', '')),
      ))
      .catch(() => {});
    return () => { alive = false; };
  }, [identityApi]);

  const versionsForType = types.filter(type => type.name === typeName);
  const selectedSummary = types.find(type => type.name === typeName && type.version === typeVersion);
  const pickerActive = Boolean(typeDetail?.referenceContract);

  const submit = async (key: string, bodyText: string) => {
    setBodyDraft(bodyText);
    setError(null);
    try {
      const envelope = await client.create({ bodyText, idempotencyKey: key });
      action.markAdmitted();
      setAdmittedMonitorId(envelope.monitorOperationId);
      navigate(`/liftr/resources/${encodeURIComponent(resourceId.trim())}`, {
        state: { monitorOperationId: envelope.monitorOperationId },
      });
    } catch (nextError) {
      const apiError = nextError as LiftrApiError;
      setError(apiError);
      if (apiError.outcomeUnknown) action.markUnknownOutcome();
      else action.markFailed();
    }
  };

  const beginCreate = () => {
    const built = buildCreateResourceBody({
      id: resourceId.trim(),
      typeName,
      typeVersion,
      ownerKind: ownerKind.trim(),
      ownerId: ownerId.trim(),
      specText,
      referencesText: pickerActive ? stringifyLosslessJson(references) : referencesText,
    });
    if (!built.ok) {
      setError(new LiftrApiError(null, { code: 'LIFTR_REQUEST_INVALID', title: 'Invalid input', detail: built.error }, 400));
      return;
    }
    void submit(action.begin(), built.bodyText);
  };

  return (
    <Grid className={classes.page} container spacing={3}>
      <Grid item xs={12}>
        <LiftrPageHeading title="Create Resource" description="Choose a platform contract, describe the Resource you need, and connect its dependencies." />
      </Grid>
      <Grid item xs={12} lg={9}>
        <InfoCard>
          {loadingTypes ? <LoadingSkeleton rows={4} label="Loading creation form" /> : (
            <>
              <FormSection number={1} title="Resource type">
                <Grid container spacing={2}>
                  <Grid item xs={12} sm={8}>
                    <TextField
                      select
                      fullWidth
                      variant="outlined"
                      label="Resource type"
                      value={typeName}
                      onChange={event => {
                        const nextName = event.target.value;
                        const firstVersion = types.find(type => type.name === nextName)?.version ?? '';
                        setTypeName(nextName);
                        setTypeVersion(firstVersion);
                      }}
                    >
                      {[...new Set(types.map(type => type.name))].map(name => <MenuItem key={name} value={name}>{name}</MenuItem>)}
                    </TextField>
                  </Grid>
                  <Grid item xs={12} sm={4}>
                    <TextField select fullWidth variant="outlined" label="Version" value={typeVersion} onChange={event => setTypeVersion(event.target.value)}>
                      {versionsForType.map(type => <MenuItem key={type.version} value={type.version}>{type.version}</MenuItem>)}
                    </TextField>
                  </Grid>
                </Grid>
                {selectedSummary && (
                  <Box mt={2}>
                    <Typography variant="subtitle2">{selectedSummary.displayName}</Typography>
                    <Typography variant="body2" color="textSecondary">{selectedSummary.description}</Typography>
                    <Box mt={1} display="flex" gridGap={6} flexWrap="wrap">
                      {selectedSummary.capabilities.map(capability => <Chip key={capability} size="small" variant="outlined" label={humanize(capability)} />)}
                    </Box>
                  </Box>
                )}
              </FormSection>

              <FormSection number={2} title="Resource details">
                <Grid container spacing={2}>
                  <Grid item xs={12}><TextField required fullWidth variant="outlined" label="Resource ID" value={resourceId} onChange={event => setResourceId(event.target.value)} helperText="A stable, URL-safe identifier chosen by you." /></Grid>
                  <Grid item xs={12} sm={4}><TextField required fullWidth variant="outlined" label="Owner kind" value={ownerKind} onChange={event => setOwnerKind(event.target.value)} /></Grid>
                  <Grid item xs={12} sm={8}>
                    <TextField required fullWidth variant="outlined" label="Owner ID" value={ownerId} onChange={event => setOwnerId(event.target.value)} helperText={suggestions.length ? `Your Backstage groups include: ${suggestions.join(', ')}. Liftr remains authoritative for access.` : 'Liftr remains authoritative for ownership and access.'} />
                  </Grid>
                </Grid>
              </FormSection>

              <FormSection number={3} title="Configuration">
                <SpecEditor value={specText} onChange={setSpecText} />
              </FormSection>

              <FormSection number={4} title="Dependencies">
                {typeDetail === null ? <LoadingSkeleton rows={2} label="Loading dependency contract" /> : typeDetail.referenceContract ? (
                  <Box display="grid" gridGap={16}>
                    {typeDetail.referenceContract.slots.map(slot => (
                      <ReferencePicker
                        key={slot.name}
                        slot={slot}
                        inventory={visibleResources}
                        sourceOwner={{ kind: ownerKind.trim(), id: ownerId.trim() }}
                        value={references[slot.name] ?? []}
                        onChange={targets => setReferences(previous => {
                          const next = { ...previous };
                          if (targets.length === 0) delete next[slot.name];
                          else next[slot.name] = [...new Set(targets)];
                          return next;
                        })}
                      />
                    ))}
                    {pickerError && <ProblemView error={pickerError} />}
                  </Box>
                ) : (
                  <Typography variant="body2" color="textSecondary">This ResourceType has no dependencies.</Typography>
                )}
              </FormSection>

              <FormSection number={5} title="Review and create">
                <Typography variant="body2" paragraph>
                  Liftr will create <strong>{resourceId.trim() || 'this Resource'}</strong> as <code>{typeName}/{typeVersion}</code>. Lifecycle work continues asynchronously after admission.
                </Typography>
                <Box display="flex" gridGap={8} flexWrap="wrap">
                  <Button variant="contained" color="primary" disabled={action.phase === 'running'} onClick={beginCreate}>Create Resource</Button>
                  {action.phase === 'unknown-outcome' && (
                    <>
                      <Button variant="outlined" onClick={() => bodyDraft && action.currentKey && submit(action.currentKey, bodyDraft)}>Replay same action</Button>
                      <Button onClick={() => { action.reset(); setError(null); }}>Start as new action</Button>
                    </>
                  )}
                </Box>
                {admittedMonitorId && <Typography variant="body2">Admitted. Monitoring operation {admittedMonitorId}.</Typography>}
                {error && <Box mt={2}><ProblemView error={error} /></Box>}
              </FormSection>
            </>
          )}
        </InfoCard>
      </Grid>
    </Grid>
  );
};

export function filterReferenceCandidates(
  inventory: ResourceSummary[],
  slot: ReferenceSlotDescriptor,
  sourceOwner: ResourceSummary['owner'],
): ResourceSummary[] {
  const allowed = new Set(slot.allowedTargetTypes.map(type => `${type.name}/${type.version}`));
  return inventory.filter(resource =>
    resource.owner.kind === sourceOwner.kind
    && resource.owner.id === sourceOwner.id
    && allowed.has(`${resource.type.name}/${resource.type.version}`)
    && resource.status.state !== 'Deleting'
    && resource.status.state !== 'Deleted',
  );
}

export const ReferencePicker: React.FC<{
  slot: ReferenceSlotDescriptor;
  inventory: ResourceSummary[];
  sourceOwner: ResourceSummary['owner'];
  value: string[];
  onChange: (resourceIds: string[]) => void;
}> = ({ slot, inventory, sourceOwner, value, onChange }) => {
  const candidates = useMemo(
    () => filterReferenceCandidates(inventory, slot, sourceOwner),
    [inventory, slot, sourceOwner.kind, sourceOwner.id],
  );
  const multiple = slot.maxItems > 1;
  const label = `${humanize(slot.name)}${slot.minItems > 0 ? ' *' : ''}`;
  return (
    <TextField
        select
        fullWidth
        required={slot.minItems > 0}
        variant="outlined"
        label={label}
        value={multiple ? value : value[0] ?? ''}
        onChange={event => {
          const selected = event.target.value;
          const next = multiple
            ? (Array.isArray(selected) ? selected.map(String) : [String(selected)])
            : selected ? [String(selected)] : [];
          onChange([...new Set(next)].slice(0, slot.maxItems));
        }}
        SelectProps={{
          multiple,
          renderValue: selected => {
            const ids = Array.isArray(selected) ? selected.map(String) : selected ? [String(selected)] : [];
            return ids.length ? ids.join(', ') : 'Select Resource';
          },
          inputProps: { 'aria-describedby': `reference-${slot.name}-help` },
        }}
        helperText={(
          <span id={`reference-${slot.name}-help`}>
            {slot.minItems === slot.maxItems ? `Select ${slot.maxItems}.` : `Select ${slot.minItems}–${slot.maxItems}.`} Candidates come only from Resources visible to you.
          </span>
        )}
      >
        {!multiple && slot.minItems === 0 && <MenuItem value=""><em>None</em></MenuItem>}
        {candidates.length === 0 && <MenuItem disabled value="">No compatible Resources available</MenuItem>}
        {candidates.map(resource => (
          <MenuItem key={resource.id} value={resource.id}>
            <Box width="100%" display="flex" justifyContent="space-between" alignItems="center" gridGap={12}>
              <span>{resource.id} · {resource.type.name}/{resource.type.version}</span>
              <StateChip state={resource.status.state} />
            </Box>
          </MenuItem>
        ))}
    </TextField>
  );
};

export const SpecEditor: React.FC<{ value: string; onChange: (value: string) => void }> = ({ value, onChange }) => {
  const classes = useStyles();
  const [prettyError, setPrettyError] = useState<string | null>(null);
  return (
    <div>
      <Typography variant="subtitle2" component="label" htmlFor="liftr-spec-json">Resource spec (JSON)</Typography>
      <Typography variant="body2" color="textSecondary" paragraph>The schema is defined by the selected ResourceType.</Typography>
      <textarea
        id="liftr-spec-json"
        aria-label="spec json"
        className={classes.editor}
        spellCheck={false}
        value={value}
        onChange={event => { setPrettyError(null); onChange(event.target.value); }}
      />
      <Box mt={0.5} display="flex" justifyContent="space-between" alignItems="baseline" flexWrap="wrap">
        <Button size="small" onClick={() => {
          try {
            onChange(stringifyLosslessJson(parseLosslessJson(value)));
            setPrettyError(null);
          } catch {
            setPrettyError('Not well-formed JSON. Fix the syntax before formatting.');
          }
        }}>Format JSON</Button>
        <Typography variant="caption" color="textSecondary">Numeric representation is preserved: 20 and 20.0 remain distinct.</Typography>
      </Box>
      {prettyError && <Typography variant="caption" color="error">{prettyError}</Typography>}
    </div>
  );
};

/** Fallback for ResourceTypes without a discoverable reference contract. */
export const ReferencesEditor: React.FC<{ value: string; onChange: (value: string) => void }> = ({ value, onChange }) => {
  const classes = useStyles();
  return (
    <div>
      <Typography variant="subtitle2" component="label" htmlFor="liftr-references-json">Dependencies (JSON)</Typography>
      <textarea id="liftr-references-json" aria-label="references json" className={classes.editor} style={{ minHeight: 100 }} spellCheck={false} value={value} onChange={event => onChange(event.target.value)} />
      <Typography variant="caption" color="textSecondary">Map contract slots to Resource ID arrays. Liftr validates visibility, type, cardinality, and cycles.</Typography>
    </div>
  );
};

export function buildUpdateFromEditor(
  specText: string,
  referencesText?: string,
): { ok: true; bodyText: string } | { ok: false; error: string } {
  return buildUpdateResourceBody(specText, referencesText);
}
