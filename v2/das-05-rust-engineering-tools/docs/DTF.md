# Distance-to-Fault (DTF)

A Remote Node's analyzer sweeps the reflection coefficient Γ(f) of an antenna feeder (S11)
across a frequency band. Each discontinuity at distance *d* (a connector, a jumper, a kink,
water in a cable, the antenna) adds a term ρ·e^(−j2πf·2d/v) to Γ(f). The inverse Fourier
transform of Γ(f) is therefore a **reflection profile over distance**: one peak per
discontinuity, whose height is its reflection magnitude. `das-engtools` computes this
profile, compensates the cable loss, finds the reflections and flags the faults.

## Request

`GET /api/dtf?nodeId=3&port=1&maxDistanceM=60`

| Parameter | Default | |
|---|---|---|
| `nodeId`, `port` | 1, 1 | The analyzer, i.e. the feeder under test. |
| `startHz`, `stopHz` | 700 MHz, 2 700 MHz | The swept band. Its width *B* sets the resolution. |
| `points` | 1024, or derived from `maxDistanceM` | 64 … 16 001 frequency points. Together with *B* this sets the range. |
| `maxDistanceM` | 0 (full range) | Distance to analyse and show. Without `points`, it picks enough points for 1.25 × this distance. |
| `velocityFactor` | 0.88 | 0.5 … 1.0; foam-dielectric coax is typically 0.85 … 0.89. |
| `cableLossDbPer100m` | 6 | 0 … 50; loss at mid-band, compensated along the distance axis. |
| `window` | `hann` | `rect`, `hann` or `kaiser` (β = 6). See [Windows](#windows). |
| `thresholdDb` | 20 | Events with a return loss below this are **faults** (20 dB ≈ VSWR 1.22). |
| `waitMs` | 0 | With `If-None-Match`: wait up to 10 s for the next measurement (long-poll). |

As with the spectrum analyzer, requests with the same settings share one measurement
session. The latest profile is served with an ETag, a matching `If-None-Match` gets `304`,
and nobody watching for 15 s stops the measurement.

## Response

```json
{
  "measurementId": 7, "nodeId": 3, "port": 1, "timestamp": 1790461306289,
  "startHz": 700000000, "stopHz": 2700000000, "points": 1139,
  "velocityFactor": 0.88, "cableLossDbPer100m": 6, "window": "hann", "thresholdDb": 20,
  "maxDistanceM": 60, "resolutionM": 0.066, "maxRangeM": 75.056,
  "noiseFloorRlDb": 92.8, "startM": 0.00458, "binM": 0.01832, "count": 3275,
  "returnLossDb": [ … 3275 values … ],
  "events": [
    { "distanceM": 0.35,  "returnLossDb": 30.46, "vswr": 1.06, "fault": false },
    { "distanceM": 1.782, "returnLossDb": 26.04, "vswr": 1.11, "fault": false },
    { "distanceM": 37.262,"returnLossDb": 22.26, "vswr": 1.17, "fault": false }
  ],
  "eventsTruncated": false
}
```

- `returnLossDb[j]` is at distance `startM + j × binM`. Long ranges are **peak-hold
  decimated** to at most 4 096 values: each value keeps the *worst* return loss of its
  group of bins, so a fault never disappears from the trace.
- `resolutionM` is the two-point resolution c·VF / 2B, and `maxRangeM` is the unambiguous
  range c·VF / 2Δf.
- `noiseFloorRlDb` is the measured noise floor, expressed as a return loss at 0 m. With
  loss compensation, the floor shown on the trace rises by 2 × `cableLossDbPer100m`/100 dB
  per metre. A UI can draw it as a line: `noiseFloorRlDb − 2·α·d`.
- `events` are sorted by distance. There are at most 64, the strongest ones, and
  `eventsTruncated` tells when there were more.

## Resolution and range

| | formula | defaults (B = 2 GHz, VF 0.88) |
|---|---|---|
| resolution | ΔR = c·VF / (2·B) | 6.6 cm |
| unambiguous range | R = (N − 1)·ΔR = c·VF / (2·Δf) | 67.5 m with 1 024 points; 1 055 m with 16 001 |

Reflections **beyond** the unambiguous range do not disappear. They alias into it and
appear at *d − R*. That is physics, not a software choice, so the range must exceed the
feeder length. `maxDistanceM` makes this the default: it picks N for 1.25 × the requested
distance. For example, 60 m gives 1 139 points and a 75 m range.

**Choose the band with the hardware.** The resolution only depends on the swept width B.
Real feeder DTF is usually swept inside the antenna's band, because antennas reflect
strongly out of band. A 2 GHz sweep is what the simulated analyzer offers. A narrower real
band gives proportionally coarser resolution (100 MHz gives 1.3 m).

## Processing

`dtf::compute`, on a DSP thread:

1. **Window** Γ(f) (see below), zero-pad to *M* = next power of two ≥ 4N, and inverse-FFT
   (rustfft, with the plan cached per thread). Normalise by the window sum, so that a
   single reflection ρ gives a peak of height ρ.
2. **Distance axis**: bin *j* is at j·c·VF / (2·M·Δf). The 4× zero-padding interpolates the
   profile; it does not improve resolution.
3. **Cable loss compensation**: multiply by 10^(2·α·d/20). The loss is round-trip, and α is
   in dB per metre. It makes the return loss of a reflection independent of how far away it
   is. It also amplifies the noise and leakage at long range. Step 5 exists because of that.
4. **Return loss** = −20·log10|Γ| and VSWR = (1+|Γ|)/(1−|Γ|), clamped at 99.99.
5. **Events.** A bin is reported when all of the following hold:
   1. It is a **local maximum** within ± one resolution cell. The comparison is circular:
      the inverse FFT wraps, so the leakage of reflections close to the port (negative
      distances) appears at the far end of the range.
   2. It is at least **15 dB above the global noise floor**, taken as the median of the
      uncompensated profile. For Rayleigh-distributed noise magnitudes, P(|x| > 5.6 ×
      median) ≈ 2⁻³¹ per cell.
   3. It is **not window leakage of a stronger peak anywhere** in the circular range. The
      leakage envelope is 0 dB within one cell (main lobe); 13 / 28 / 40 dB (rect / Hann /
      Kaiser) up to 6 cells; then a further 6 dB per octave. The rectangular and Kaiser
      windows roll off at about that rate, and Hann faster, so the envelope is conservative.
   4. It is **15 dB above the local floor (OS-CFAR)**, the median of the cells 3 to 32
      resolution cells either side. Near strong reflections, the floor is their leakage
      rather than the receiver noise.
   5. It is inside the shown range and has a return loss **below 35 dB**, the event floor.

   The position is refined by parabolic interpolation between bins. An event is a **fault**
   when its return loss is below `thresholdDb`.
6. **Trace**: peak-hold decimation to at most 4 096 values.

Steps 5.1 to 5.4 came out of validation. A first version with only 5.1 and 5.5 found every
real reflection, but at 16 001 points and full range it reported **2 100 phantom events**,
all of them noise that the loss compensation had lifted by up to 127 dB at 1 km. With the
global noise threshold, one phantom remained at 1 055 m: the wrapped leakage of the port
connector, lifted by that same 127 dB. The circular leakage test removed it. The CFAR test
removed the last phantom, which appeared only with very noisy sweeps (σ = 0.01) and a
rectangular window. A phantom fault is the costly kind of error, because it sends a
technician up a mast, so the validation below counts phantoms separately.

### Windows

| window | peak side lobe | main lobe | use |
|---|---|---|---|
| `rect` | −13 dB | narrowest | Separating two close reflections of similar size. Side lobes can hide small ones. |
| `hann` (default) | −31 dB | 2× wider | General use. |
| `kaiser` (β = 6) | about −44 dB | slightly wider than Hann | A small fault next to a large reflection, e.g. a jumper 1 m from a badly matched antenna. |

## Validation

`cargo run --release --example dtf-validate` (`make dtf-validate`) runs a sweep of simulated
feeders: 80 feeders × 3 windows × 3 sweep sizes (1 024 / 4 096 / 16 001 points) × 5 noise
levels. Each simulated feeder has a port connector (ρ 0.03 at 0.35 m), a jumper (ρ 0.05 at
1.5–2.5 m), in half of them a damaged connector (ρ 0.15–0.25), and the antenna (ρ 0.06–0.12
at 25–55 m). Cable loss is 6 dB/100 m at mid-band and grows with √f. The noise is complex
Gaussian per frequency point.

| noise σ (per point) | reflections | missed | phantom events | worst RL error |
|---|---|---|---|---|
| 0 | 2 511 | 0 | 0 | 0.30 dB |
| 0.0005 | 2 511 | 0 | 0 | 0.30 dB |
| 0.003 | 2 511 | 0 | 0 | 0.31 dB |
| 0.01 | 2 511 | 0 | 0 | 0.36 dB |
| 0.03 | 2 511 | 0 | 0 | 0.79 dB |

Configurations whose feeder is longer than 80 % of the range are skipped (aliasing, see
above). The unit tests also cover side-lobe suppression of a ρ = 0.9 reflection, the
long-range case at 16 001 points, the event cap, and the automatic choice of points.

**What the simulation does not model**:

- multiple reflections (re-reflections between two discontinuities, which create weaker
  echo peaks at the sum of distances);
- dispersion and connector phase;
- the analyzer's own calibration error (directivity, source match);
- temperature.

The S11 data must be **calibrated at the port** (open/short/load) by the hardware or its
driver. DTF cannot be better than that calibration. The first field test should measure a
feeder with a known fault position, for example a short jumper and an open termination at a
measured length, and compare.

## Cost

This is `examples/dsp-bench`, native x86-64 with one thread, and includes windowing, the
FFT, compensation, detection and the trace:

| points | FFT size | time |
|---|---|---|
| 1 024 | 4 096 | 0.36 ms |
| 4 096 | 16 384 | 1.3 ms |
| 16 001 | 65 536 | 4.6 ms |

A Cortex-A53-class core is typically several times slower. This was not measured here.
Even at 10× slower, a full-range 16 001-point profile stays around 50 ms of one DSP thread
at nice +10, once per measurement, shared by every client.
