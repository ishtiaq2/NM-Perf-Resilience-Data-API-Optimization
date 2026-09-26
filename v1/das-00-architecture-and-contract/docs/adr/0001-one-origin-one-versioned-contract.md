# ADR 0001: One origin and one versioned contract for every backend

- Status: accepted
- Date: 2026-09-26

## Context

We will run several backend implementations over time: the shipped Node app, the
hotfix, a gateway with split services and a Go edge. Often they run side by side during
a migration. The Angular frontend must not care which process answers, and customers
must not open extra ports or deal with CORS.

## Decision

- The browser talks to **one origin** (host, port, certificate, cookie). Any split into
  processes happens behind it (nginx or the edge's reverse proxy).
- All backends implement **das-v1**, written down as OpenAPI 3.1 (REST), AsyncAPI 3.1
  (WebSocket), protobuf (node ingest) and a binary frame spec.
- New behaviour is **opt-in and discoverable** (`GET /api/capabilities`). Existing URLs
  and shapes are frozen within v1.
- `conformance/run.js` is the executable contract, run against every backend in CI.

## Consequences

- Backends can be swapped or combined without a frontend release.
- The contract and the conformance suite are a shared asset. Changing them needs review
  (`buf breaking` for protos).
- A new capability has to be designed as additive. Breaking changes need `das-v2` served
  in parallel.
