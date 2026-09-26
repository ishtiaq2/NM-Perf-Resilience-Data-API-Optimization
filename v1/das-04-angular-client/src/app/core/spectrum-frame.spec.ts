import { decodeFrame, peakDecimate } from './spectrum-frame';

/** Produced by the Node.js encoder (das-01 src/shared/spectrum-frame.js): cross-implementation check. */
const NODE_FRAME_B64 = 'RFNQQwEBAQAqAAAABwAAAAIAAAAFAAAAAAAAgJPcxEEAAAAAAIjjQAAAwAZFDHpCENpe6ACA0gTw2A==';

function fromB64(b64: string): ArrayBuffer {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer;
}

describe('DSPC frame decoder', () => {
  it('decodes a frame produced by the Node.js backend', () => {
    const t = decodeFrame(fromB64(NODE_FRAME_B64));
    expect(t.sweepId).toBe(42);
    expect(t.nodeId).toBe(7);
    expect(t.port).toBe(2);
    expect(t.startHz).toBe(700e6);
    expect(t.stepHz).toBe(40000);
    expect(t.timestampMs).toBe(1790000000000);
    expect(t.decimated).toBe(true);
    expect(Array.from(t.power.slice(0, 2))).toEqual([-97.12, -60.5].map((v) => Math.fround(v)));
    expect(Number.isNaN(t.power[2])).toBe(true);
    expect(t.power[3]).toBeCloseTo(12.34, 2);
    expect(t.power[4]).toBe(-100);
  });

  it('rejects foreign or truncated data', () => {
    expect(() => decodeFrame(new ArrayBuffer(8))).toThrow(/too short/);
    expect(() => decodeFrame(new ArrayBuffer(64))).toThrow(/not a DSPC frame/);
    expect(() => decodeFrame(fromB64(NODE_FRAME_B64).slice(0, 50))).toThrow(/truncated/);
  });

  it('client-side peak decimation keeps narrow spurs', () => {
    const power = new Float32Array(10000).fill(-100);
    power[4321] = -40;
    const t = peakDecimate({ sweepId: 1, nodeId: 1, port: 1, startHz: 0, stepHz: 1, timestampMs: 0, decimated: false, power, wireBytes: 0 }, 500);
    expect(t.power.length).toBeLessThanOrEqual(500);
    expect(Math.max(...t.power)).toBe(-40);
    expect(t.decimated).toBe(true);
  });
});
