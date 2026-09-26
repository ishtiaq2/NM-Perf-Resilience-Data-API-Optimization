# ADR 0008: Binary spectrum frames and server-side peak decimation

- Status: accepted
- Date: 2026-09-26

## Context

A 50 001-point sweep as legacy JSON is ~2 MB, ~40 bytes per point. A browser chart is at
most a few thousand pixels wide. Averaging or taking every n-th point hides narrow
carriers and spurs, which are exactly what RF engineers look for.

## Decision

- **DSPC v1 frame** (contract/spectrum-binary-frame.md): a 48-byte little-endian header
  followed by int16 centi-dBm (0.01 dB resolution) or float32 values; NaN is represented
  by -32768.
- **Positive-peak decimation** on the server to `maxPoints`, rounded to multiples of 64
  so the number of cached variants stays small. The client sends its canvas width.
- Each (sweep, format, width) variant is encoded once and shared. The legacy JSON stays
  available unchanged.

## Consequences

- 2 bytes per point instead of ~40, and 2–3 kB per update at screen width instead of
  2 MB. The browser needs no JSON parsing: it views the payload as an `Int16Array`.
- Byte-identical encoders exist in Node (01/02) and Go (03), verified with a shared
  fixture.
