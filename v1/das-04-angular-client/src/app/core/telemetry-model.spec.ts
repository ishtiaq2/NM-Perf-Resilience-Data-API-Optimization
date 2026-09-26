import { NodeTelemetry } from './api.types';
import { EMPTY_STATE, applyDelta, applySnapshot, isDelta } from './telemetry-model';

const n = (id: number, t: number): NodeTelemetry => ({ id, name: 'RN-' + id, status: 'online', temperatureC: t });

describe('telemetry model', () => {
  const snap = { rev: 10, generatedAt: 1, nodeCount: 2, nodes: { '1': n(1, 40), '2': n(2, 41) } };

  it('applies a snapshot', () => {
    const s = applySnapshot(snap);
    expect(s.rev).toBe(10);
    expect(s.nodes.size).toBe(2);
    expect(s.nodes.get(2)?.temperatureC).toBe(41);
  });

  it('applies a delta only on top of its base revision', () => {
    const s = applySnapshot(snap);
    const d = { rev: 12, base: 10, generatedAt: 2, changed: { '2': n(2, 55), '3': n(3, 30) }, removed: [1] };
    const next = applyDelta(s, d);
    expect(next).not.toBeNull();
    expect(next!.rev).toBe(12);
    expect(next!.nodes.has(1)).toBe(false);
    expect(next!.nodes.get(2)?.temperatureC).toBe(55);
    expect([...next!.changed].sort()).toEqual([2, 3]);
    // The previous state is untouched (signals compare by reference).
    expect(s.nodes.get(2)?.temperatureC).toBe(41);
  });

  it('asks for a resync when a delta does not fit', () => {
    const s = applySnapshot(snap);
    expect(applyDelta(s, { rev: 13, base: 11, generatedAt: 3, changed: {}, removed: [] })).toBeNull();
    expect(applyDelta(EMPTY_STATE, { rev: 1, base: 0, generatedAt: 0, changed: {}, removed: [] })).toBeNull();
  });

  it('tells deltas from snapshots', () => {
    expect(isDelta({ rev: 1, base: 0, generatedAt: 0, changed: {}, removed: [] })).toBe(true);
    expect(isDelta(snap)).toBe(false);
  });
});
