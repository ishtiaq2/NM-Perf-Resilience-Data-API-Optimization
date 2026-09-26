import { findPeaks } from './spectrum';

describe('findPeaks', () => {
  it('returns the strongest separated maxima', () => {
    const power = new Float32Array(1000).fill(-100);
    power[100] = -50; power[101] = -52; // one carrier (two bins)
    power[500] = -60;
    power[800] = -70;
    const peaks = findPeaks({ sweepId: 1, nodeId: 1, port: 1, startHz: 700e6, stepHz: 1e6, timestampMs: 0, decimated: false, power, wireBytes: 0 }, 3);
    expect(peaks.map((p) => p.dbm)).toEqual([-50, -60, -70]);
    expect(peaks[0].freqMhz).toBe(800);
  });
});
