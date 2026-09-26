# ADR 0004: Change detection by field hygiene, deadbands and a canonical state hash

- Status: accepted
- Date: 2026-09-26

## Context

Every Remote Node report looks like a change because of sequence numbers, timestamps,
uptime counters and analog noise, so "push only if changed" would still push everything.

## Decision

- **Field hygiene:** per-report counters are not part of the state; the boot time
  replaces uptime.
- **Deadbands** per analog field (0.5 °C, 1 dB DL, 0.3 dB optical, …; see
  contract/ingest-protocol.md). A value is republished only when it moves by more than
  its deadband. Alarms and status always pass through.
- **Canonical encoding + FNV-1a 64 hash** of the normalised state. It is used by node
  deltas (`base_hash`/`state_hash`) and for cheap equality checks.
- Revisions start at a **random per-boot base**, so a revision from before a reboot can
  never be mistaken for a current one.

## Consequences

- Measured (03): 300 reports/s become ~11 real changes/s; node → master traffic is 24×
  smaller.
- Deadband values are product decisions, to be agreed with the RF/operations team and
  kept well below alarm thresholds.
- Canonical encoding rules must be identical on node and master: they are part of the
  contract.
