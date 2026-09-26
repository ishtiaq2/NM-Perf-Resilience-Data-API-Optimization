/**
 * Pure state transitions for volatile_data (easy to unit-test).
 * A delta applies only on top of the revision it was computed from (`base`).
 */
import { NodeTelemetry, VolatileDelta, VolatileSnapshot } from './api.types';

export interface TelemetryState {
  rev: number;
  generatedAt: number;
  nodes: ReadonlyMap<number, NodeTelemetry>;
  /** Node ids changed by the last update (row highlight). */
  changed: ReadonlySet<number>;
}

export const EMPTY_STATE: TelemetryState = { rev: -1, generatedAt: 0, nodes: new Map(), changed: new Set() };

export function applySnapshot(s: VolatileSnapshot): TelemetryState {
  const nodes = new Map<number, NodeTelemetry>();
  for (const [id, n] of Object.entries(s.nodes)) nodes.set(Number(id), n);
  return { rev: s.rev, generatedAt: s.generatedAt, nodes, changed: new Set(nodes.keys()) };
}

/** Returns null when the delta does not fit the current revision: the caller must resync. */
export function applyDelta(state: TelemetryState, d: VolatileDelta): TelemetryState | null {
  if (d.base !== state.rev) return null;
  if (d.rev === state.rev) return state;
  const nodes = new Map(state.nodes);
  const changed = new Set<number>();
  for (const [id, n] of Object.entries(d.changed)) {
    nodes.set(Number(id), n); // full node state: replace, do not merge
    changed.add(Number(id));
  }
  for (const id of d.removed) nodes.delete(id);
  return { rev: d.rev, generatedAt: d.generatedAt, nodes, changed };
}

export function isDelta(x: VolatileSnapshot | VolatileDelta): x is VolatileDelta {
  return typeof (x as VolatileDelta).base === 'number';
}
