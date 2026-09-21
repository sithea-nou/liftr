/**
 * @vitest-environment jsdom
 */
import React from 'react';
import { render, screen } from '@testing-library/react';
import { createTheme, ThemeProvider } from '@material-ui/core/styles';
import { MemoryRouter } from 'react-router-dom';
import {
  LosslessNumber,
  Operation,
  ResourceSummary,
  buildCreateResourceBody,
  parseLosslessJson,
} from '@liftr/plugin-liftr-common';
import { ConditionsCard, ReferencesCard, canRetryOperation } from '../components/ResourceDetailPage';
import { filterReferenceCandidates, buildUpdateFromEditor } from '../components/CreateResourcePage';
import { LiftrPageHeading, OperationStateChip, ProblemView, StateChip } from '../components/common';
import { LiftrApiError } from '../api/client';

function contrastRatio(foreground: string, background: string): number {
  const parse = (value: string): number[] => {
    const match = value.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
    if (!match) throw new Error(`Unsupported test color: ${value}`);
    return match.slice(1, 4).map(Number);
  };
  const luminance = (value: string): number => {
    const [red, green, blue] = parse(value).map(channel => {
      const normalized = channel / 255;
      return normalized <= 0.04045 ? normalized / 12.92 : ((normalized + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * red + 0.7152 * green + 0.0722 * blue;
  };
  const light = Math.max(luminance(foreground), luminance(background));
  const dark = Math.min(luminance(foreground), luminance(background));
  return (light + 0.05) / (dark + 0.05);
}

function number(value: string): LosslessNumber {
  return parseLosslessJson(value) as LosslessNumber;
}

function summary(id: string, typeName: string, state: ResourceSummary['status']['state'] = 'Ready'): ResourceSummary {
  return {
    id,
    type: { name: typeName, version: 'v1' },
    owner: { kind: 'team', id: 'demo' },
    generation: number('1'),
    status: { state, observedGeneration: number('1'), updatedAt: '2026-08-27T10:00:00Z' },
    createdAt: '2026-08-27T10:00:00Z',
    updatedAt: '2026-08-27T10:00:00Z',
  };
}

describe('M21.6 resource presentation', () => {
  it('keeps the primary navigation action readable over its theme color', () => {
    const theme = createTheme({ palette: { primary: { main: 'rgb(156, 201, 255)' } } });
    render(
      <ThemeProvider theme={theme}>
        <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
          <LiftrPageHeading title="Resources" />
        </MemoryRouter>
      </ThemeProvider>,
    );
    const action = screen.getByRole('button', { name: 'Create Resource' });
    const style = window.getComputedStyle(action);
    expect(contrastRatio(style.color, theme.palette.primary.main)).toBeGreaterThanOrEqual(4.5);
  });

  it.each(['Pending', 'Ready', 'Failed', 'Deleting', 'Deleted', 'Unknown'] as const)(
    'renders %s with a textual accessible state',
    state => {
      render(<StateChip state={state} />);
      expect(screen.getByLabelText(`Resource state: ${state}`).textContent).toContain(state);
    },
  );

  it('makes dependency waiting distinct from generic loading', () => {
    render(
      <ConditionsCard conditions={[{
        type: 'DependenciesReady',
        status: 'False',
        reason: 'WaitingForDependencies',
        message: 'Waiting for dependencies.',
      }]} />,
    );
    expect(screen.getByText('Dependencies Ready')).toBeTruthy();
    expect(screen.getByLabelText('Dependencies Ready: False').textContent).toContain('Waiting');
    expect(screen.getByText('Waiting for dependencies.')).toBeTruthy();
  });

  it('renders references as linked Resource cards with type and state', () => {
    render(
      <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <ReferencesCard references={{ database: ['database-a'] }} inventory={[summary('database-a', 'DependencyAnchor')]} />
      </MemoryRouter>,
    );
    expect(screen.getByRole('link', { name: 'database-a' }).getAttribute('href')).toBe('/liftr/resources/database-a');
    expect(screen.getByText('DependencyAnchor/v1')).toBeTruthy();
    expect(screen.getByLabelText('Resource state: Ready')).toBeTruthy();
  });

  it('filters only the supplied authorized inventory by allowed target type', () => {
    const supplied = [
      summary('allowed', 'DependencyAnchor'),
      summary('wrong-type', 'DependentResource'),
      summary('deleted', 'DependencyAnchor', 'Deleted'),
      summary('deleting', 'DependencyAnchor', 'Deleting'),
      { ...summary('other-owner', 'DependencyAnchor'), owner: { kind: 'team', id: 'other' } },
    ];
    const candidates = filterReferenceCandidates(supplied, {
      name: 'database',
      allowedTargetTypes: [{ name: 'DependencyAnchor', version: 'v1' }],
      minItems: 1,
      maxItems: 1,
    }, { kind: 'team', id: 'demo' });
    expect(candidates.map(item => item.id)).toEqual(['allowed']);
    expect(candidates).not.toContainEqual(expect.objectContaining({ id: 'not-supplied-and-therefore-not-authorized' }));
  });

  it('keeps exact create request semantics including separate references', () => {
    const built = buildCreateResourceBody({
      id: 'application-a',
      typeName: 'DependentResource',
      typeVersion: 'v1',
      ownerKind: 'team',
      ownerId: 'demo',
      specText: '{"weight":20.0}',
      referencesText: '{"database":["database-a"]}',
    });
    expect(built.ok).toBe(true);
    if (built.ok) {
      expect(built.bodyText).toContain('"weight":20.0');
      expect(built.bodyText).toContain('"references":{"database":["database-a"]}');
    }
  });

  it('update initialization can preserve references and explicit changes replace them', () => {
    const preserved = buildUpdateFromEditor('{"image":"demo:v2"}', '{"database":["database-a"]}');
    const changed = buildUpdateFromEditor('{"image":"demo:v2"}', '{"database":["database-b"]}');
    expect(preserved.ok && preserved.bodyText).toContain('"database":["database-a"]');
    expect(changed.ok && changed.bodyText).toContain('"database":["database-b"]');
  });

  it.each([
    ['RESOURCE_IN_USE', 'Resource cannot be deleted'],
    ['DEPENDENCY_CYCLE', 'Dependency cycle detected'],
  ])('curates %s without hiding its safe code', (code, heading) => {
    render(<ProblemView error={new LiftrApiError({ status: 409, code, title: 'Conflict', detail: 'Safe public detail' }, null, 409)} />);
    expect(screen.getByText(heading)).toBeTruthy();
    expect(screen.getByText(/Safe public detail/)).toBeTruthy();
    expect(screen.getByText(new RegExp(code))).toBeTruthy();
  });

  it('offers an explicit reload action for a generation conflict', () => {
    const reload = jest.fn();
    render(<ProblemView error={new LiftrApiError({ status: 409, code: 'GENERATION_CONFLICT', title: 'Conflict', currentGeneration: 4n }, null, 409)} onReload={reload} />);
    screen.getByRole('button', { name: 'Reload Resource' }).click();
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it('renders terminal operation state text and keeps retry limited to failed operations', () => {
    render(<OperationStateChip state="Succeeded" />);
    expect(screen.getByLabelText('Operation state: Succeeded').textContent).toContain('Succeeded');
    const failed: Operation = {
      id: 'op-failed', resourceId: 'application-a', capability: 'update', state: 'Failed',
      targetGeneration: number('2'), requestedAt: '2026-08-27T10:00:00Z',
    };
    expect(canRetryOperation(failed)).toBe(true);
    expect(canRetryOperation({ ...failed, state: 'Succeeded' })).toBe(false);
  });
});
