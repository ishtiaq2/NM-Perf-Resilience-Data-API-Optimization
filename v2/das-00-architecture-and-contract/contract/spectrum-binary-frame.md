# DSPC v1: binary spectrum frame

Media type: `application/vnd.das.spectrum`. The same bytes are used as the HTTP
body of `GET /api/spectrum/latest` (with that `Accept` header), as a binary
WebSocket message on `/api/ws/spectrum`, and as the `power_centi_dbm` payload in
`das.v1.SpectrumService`.

## Why

A sweep of 50,001 points in the legacy JSON shape is ~2 MB and needs a full
`JSON.parse` in the browser. The frequency axis is a linear grid, so it only
needs `startHz` and `stepHz`. The power values fit in 2 bytes each at 0.01 dB
resolution; analyzer amplitude accuracy is about ±1 dB. A DSPC frame of the
same sweep is 100 kB and costs **zero parsing**: the browser wraps the bytes in an
`Int16Array`. With peak-detector decimation to the canvas width (e.g. 1,600
points) a frame is ~3 kB.

## Layout (little-endian, 48-byte header, 8-byte aligned)

| Offset | Size | Type | Field | Notes |
|---:|---:|---|---|---|
| 0 | 4 | u32 | magic | `0x43505344`, the ASCII bytes `D S P C` |
| 4 | 1 | u8 | version | `1` |
| 5 | 1 | u8 | encoding | `1` = int16 centi-dBm, `2` = float32 dBm |
| 6 | 2 | u16 | flags | bit 0 = decimated (peak detector) |
| 8 | 4 | u32 | sweepId | increases by 1 per sweep of this analyzer session |
| 12 | 4 | u32 | nodeId | |
| 16 | 2 | u16 | port | |
| 18 | 2 | u16 | reserved | `0` |
| 20 | 4 | u32 | count | number of power values |
| 24 | 8 | f64 | startHz | frequency of value 0 |
| 32 | 8 | f64 | stepHz | `frequency(i) = startHz + i * stepHz` |
| 40 | 8 | f64 | timestampMs | epoch ms, sweep completion |
| 48 | count × 2 (or × 4) | i16 / f32 | power | int16: `dBm = value / 100`; `-32768` = no data |

The payload starts at offset 48, a multiple of 8, so `new Int16Array(buf, 48, count)`
works without copying whenever the frame itself starts at an aligned offset
(which is always the case for a WebSocket `ArrayBuffer` or a `fetch()` body).

## Decimation (`maxPoints`)

When the sweep has more points than requested, the server keeps the **maximum** of
every bucket of `ceil(count / maxPoints)` consecutive values. That is the positive-peak
detector of a real spectrum analyzer: narrow carriers and one-bin spurs are never
averaged away. The output frequency axis points at bucket centres:
`startHz' = startHz + (bucket - 1) * stepHz / 2`, `stepHz' = stepHz * bucket`.

## Reference decoders

TypeScript (browser), from `das-04-angular-client/src/app/core/spectrum-frame.ts`:

```ts
export function decodeFrame(buf: ArrayBuffer): SpectrumFrame {
  const dv = new DataView(buf);
  if (dv.getUint32(0, true) !== 0x43505344) throw new Error('not a DSPC frame');
  const encoding = dv.getUint8(5), count = dv.getUint32(20, true);
  const power = new Float32Array(count);
  if (encoding === 1) {
    const raw = new Int16Array(buf, 48, count);
    for (let i = 0; i < count; i++) power[i] = raw[i] === -32768 ? NaN : raw[i] / 100;
  } else power.set(new Float32Array(buf, 48, count));
  return { sweepId: dv.getUint32(8, true), nodeId: dv.getUint32(12, true), port: dv.getUint16(16, true),
           decimated: (dv.getUint16(6, true) & 1) === 1, count, startHz: dv.getFloat64(24, true),
           stepHz: dv.getFloat64(32, true), timestampMs: dv.getFloat64(40, true), power };
}
```

Node.js: `das-01-hotfix-node/src/shared/spectrum-frame.js`. Go: `das-03-lts-go-edge/internal/spectrum/frame.go`.
All three are checked against the same frames by the conformance suite.

## Evolution

New fields go into a new `version`. A decoder rejects versions it does not know,
and the server negotiates the version via the media type
(`application/vnd.das.spectrum; v=2`) when that becomes necessary.
