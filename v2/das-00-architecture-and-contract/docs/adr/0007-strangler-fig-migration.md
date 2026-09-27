# ADR 0007: Migrate by strangler fig, one endpoint group per step

- Status: accepted
- Date: 2026-09-26

## Context

A big-bang rewrite of the device backend is too risky for field systems, and the team
must keep shipping.

## Decision

- The edge goes **in front** of the existing app (`-legacy`). Every path it does not
  serve is proxied to it, with the browser's `Host`, and authentication is checked
  against it.
- Endpoint groups move one at a time with a flag (`-delegate volatile,spectrum,ws`,
  `-config legacy|local`).
- `/api/capabilities` merges what the edge serves with what the legacy app advertises, so
  clients never opt into a feature the answering process lacks.
- Each step passes the conformance suite, including the heartbeat SLO.

## Consequences

- Measured against the shipped behaviour: steps 1–4 pass (6/11/14/18 checks, 0 failures).
  Step 1 alone ends the false alarms.
- Two processes run during the migration (edge + legacy), which costs some memory until
  the Node app is retired.
- Every step is reversible with one flag.
