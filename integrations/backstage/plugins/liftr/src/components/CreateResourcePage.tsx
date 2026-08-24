/**
 * Create / update / delete flows with generation-safe, idempotency-safe
 * semantics and JSON-editor-first spec editing (numeric lexemes preserved
 * verbatim; the server's 422 remains authoritative).
 */

import React, { useEffect, useMemo, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Button, Grid, MenuItem, TextField, Typography } from '@material-ui/core';
import { InfoCard } from '@backstage/core-components';
import {
  buildCreateResourceBody,
  buildUpdateResourceBody,
  stringifyLosslessJson,
  parseLosslessJson,
} from '@liftr/plugin-liftr-common';
import { identityApiRef, useApi } from '@backstage/core-plugin-api';
import { LiftrApiError } from '../api/client';
import { useLiftrClient } from '../hooks/useLiftrClient';
import { useIdempotentAction } from '../hooks/useIdempotentAction';
import { ProblemView } from './common';

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

export const CreateResourcePage: React.FC = () => {
  const client = useLiftrClient();
  const navigate = useNavigate();
  const identityApi = useApi(identityApiRef);
  const action = useIdempotentAction();

  const [types, setTypes] = useState<Array<{ name: string; version: string }>>([]);
  const [typeName, setTypeName] = useState('');
  const [typeVersion, setTypeVersion] = useState('');
  const [resourceId, setResourceId] = useState('');
  const [ownerKind, setOwnerKind] = useState('team');
  const [ownerId, setOwnerId] = useState('');
  const [specText, setSpecText] = useState('{\n  \n}');
  const [error, setError] = useState<(LiftrApiError & { outcomeUnknown?: boolean }) | null>(null);
  const [admittedMonitorId, setAdmittedMonitorId] = useState<string | null>(null);
  const [bodyDraft, setBodyDraft] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    client
      .listResourceTypes()
      .then(r => {
        if (!alive) return;
        setTypes(r.items.map(t => ({ name: t.name, version: t.version })));
        if (r.items.length > 0 && typeName === '') {
          setTypeName(r.items[0]!.name);
          setTypeVersion(r.items[0]!.version);
        }
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [client]);

  // Presentation-only owner suggestions from the Backstage catalog identity.
  const [suggestions, setSuggestions] = useState<string[]>([]);
  useEffect(() => {
    let alive = true;
    void identityApi
      .getBackstageIdentity()
      .then((id: { ownershipEntityRefs: string[] }) =>
        alive
          ? setSuggestions(
              id.ownershipEntityRefs
                .filter((ref: string) => ref.startsWith('group:'))
                .map((ref: string) => ref.replace('group:default/', '')),
            )
          : undefined,
      )
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [identityApi]);

  const versionsForType = types.filter(t => t.name === typeName).map(t => t.version);

  const submit = async (key: string) => {
    const built = buildCreateResourceBody({
      id: resourceId.trim(),
      typeName,
      typeVersion,
      ownerKind: ownerKind.trim(),
      ownerId: ownerId.trim(),
      specText,
    });
    if (!built.ok) {
      setError(new LiftrApiError(null, { code: 'LIFTR_REQUEST_INVALID', title: 'Invalid input', detail: built.error }, 400));
      return;
    }
    setBodyDraft(built.bodyText);
    try {
      const env = await client.create({ bodyText: built.bodyText, idempotencyKey: key });
      action.markAdmitted();
      setAdmittedMonitorId(env.monitorOperationId);
      navigate(`/liftr/resources/${encodeURIComponent(resourceId.trim())}`, {
        state: { monitorOperationId: env.monitorOperationId },
      });
    } catch (e) {
      const err = e as LiftrApiError;
      setError(err);
      if (err.outcomeUnknown) action.markUnknownOutcome();
      else action.markFailed();
    }
  };

  return (
    <Grid container spacing={3}>
      <Grid item xs={12} md={8}>
        <InfoCard title="Create a Liftr Resource">
          <div style={{ display: 'grid', gap: 12 }}>
            <TextField
              select
              label="Resource type"
              value={typeName}
              onChange={e => {
                setTypeName(e.target.value);
                setTypeVersion(versionsForType[0] ?? '');
              }}
            >
              {[...new Set(types.map(t => t.name))].map(n => (
                <MenuItem key={n} value={n}>
                  {n}
                </MenuItem>
              ))}
            </TextField>
            <TextField
              select
              label="Version"
              value={typeVersion}
              onChange={e => setTypeVersion(e.target.value)}
            >
              {versionsForType.map(v => (
                <MenuItem key={v} value={v}>
                  {v}
                </MenuItem>
              ))}
            </TextField>
            <TextField label="Resource ID" value={resourceId} onChange={e => setResourceId(e.target.value)} helperText="URL-segment-safe identifier you choose" />
            <div style={{ display: 'flex', gap: 12 }}>
              <TextField label="Owner kind" value={ownerKind} onChange={e => setOwnerKind(e.target.value)} />
              <TextField
                label="Owner id"
                value={ownerId}
                onChange={e => setOwnerId(e.target.value)}
                helperText="Suggestions below come from your Backstage catalog groups — Liftr decides authorization."
              />
            </div>
            {suggestions.length > 0 && (
              <Typography variant="caption" style={{ marginTop: -8 }}>
                Suggestions: {suggestions.join(', ')} (display only)
              </Typography>
            )}
            <SpecEditor value={specText} onChange={setSpecText} />
            <div style={{ display: 'flex', gap: 8 }}>
              <Button
                variant="contained"
                color="primary"
                disabled={action.phase === 'running'}
                onClick={() => submit(action.begin())}
              >
                Create resource
              </Button>
              {action.phase === 'unknown-outcome' && (
                <>
                  <Button
                    variant="outlined"
                    onClick={() => bodyDraft && submit(action.currentKey!)} // SAME key replay
                  >
                    Replay same action (same key)
                  </Button>
                  <Button
                    variant="text"
                    onClick={() => {
                      action.reset();
                      setError(null);
                    }}
                  >
                    Start as new action
                  </Button>
                </>
              )}
            </div>
            {admittedMonitorId && (
              <Typography variant="body2">Admitted. Monitoring operation {admittedMonitorId}.</Typography>
            )}
            {error && (
              <ProblemView error={error as unknown as Error & { problem?: unknown }} />
            )}
          </div>
        </InfoCard>
      </Grid>
    </Grid>
  );
};

// ---------------------------------------------------------------------------
// Spec editor: raw text in, raw text out — never through JS numbers.
// ---------------------------------------------------------------------------

export const SpecEditor: React.FC<{ value: string; onChange: (v: string) => void }> = ({
  value,
  onChange,
}) => {
  const [prettyError, setPrettyError] = useState<string | null>(null);
  return (
    <div>
      <Typography variant="subtitle2">Desired spec (raw JSON)</Typography>
      <textarea
        aria-label="spec json"
        spellCheck={false}
        style={{ width: '100%', minHeight: 180, fontFamily: 'monospace', fontSize: 13 }}
        value={value}
        onChange={e => {
          setPrettyError(null);
          onChange(e.target.value);
        }}
      />
      <Button
        size="small"
        onClick={() => {
          try {
            const parsed = parseLosslessJson(value); // lossless: keeps 20 vs 20.0
            onChange(stringifyLosslessJson(parsed));
            setPrettyError(null);
          } catch {
            setPrettyError('Not well-formed JSON — fix before formatting.');
          }
        }}
      >
        Format
      </Button>
      {prettyError && <Typography variant="caption" color="error">{prettyError}</Typography>}
      <Typography variant="caption" display="block">
        Numeric representation is preserved exactly: 20 and 20.0 are different values to Liftr.
        Server validation is authoritative.
      </Typography>
    </div>
  );
};

// ---------------------------------------------------------------------------
// Update + delete (used from ResourceDetail actions)
// ---------------------------------------------------------------------------

export function buildUpdateFromEditor(specText: string): { ok: true; bodyText: string } | { ok: false; error: string } {
  return buildUpdateResourceBody(specText);
}
