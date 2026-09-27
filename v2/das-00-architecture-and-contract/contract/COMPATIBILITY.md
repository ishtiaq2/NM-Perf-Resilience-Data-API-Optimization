# Compatibility rules: many backends, one API

The programme ships several backend implementations over time: the Node hotfix,
the gateway with split services, the Go edge server, and the Rust engineering-tools
service. They run side by side during migration. The frontend must never notice which
one answers.

## 1. One origin

The browser talks to **one** host and port. Everything behind it (nginx, the Go
edge, several Node processes) is routed by path. There is no CORS, no second
certificate and no second login.

| Path | Hotfix (01) | Gateway (02) | Go edge (03) | Gateway + Rust tools (02 + 05) |
|---|---|---|---|---|
| `/` (Angular app) | existing backend | nginx (static, gzip_static) | Go edge (embedded or disk) | nginx |
| `/api/heartbeat` | Node process | core-api | Go edge | core-api |
| `/api/volatile-data` | Node process | telemetry-service | Go edge | telemetry-service |
| `/api/spectrum*` | Node process | spectrum-service | Go edge | das-engtools (Rust) |
| `/api/dtf` | - | - | - | das-engtools (Rust) |
| `/api/capabilities` | Node process | core-api | Go edge | das-engtools (merges core-api's) |
| `/api/ws/telemetry` | - | telemetry-service | Go edge | telemetry-service |
| `/api/ws/spectrum` | - | spectrum-service | Go edge | das-engtools (Rust) |
| `/api/nodes/*/config` | Node process | core-api | legacy Node app (`-config legacy`) or the edge (`-config local`) | core-api |
| everything else | Node process | core-api | proxied to the legacy Node app (strangler fig) | core-api |

das-05 can also run without the gateway: in front of the unchanged Node app (it forwards
what it does not implement, WebSocket upgrades included), or behind it (the Node app pipes
the engineering routes to it). In every setup `/api/capabilities` is merged, so it
describes the whole origin.

During a migration the Go edge can leave whole groups to the legacy app
(`-delegate volatile,spectrum,ws`). Its `/api/capabilities` then advertises, for those
groups, what the legacy app itself advertises (nothing for a shipped release). A client
therefore never opts into a feature that the process actually answering lacks.

## 2. Never break a shipped client

* Existing URLs, response shapes, status codes and semantics are frozen.
  `/api/spectrum` still returns `points: [{frequency, power}]`, and the tests
  compare new against old byte for byte.
* New behaviour is opt-in: new endpoints (`/api/spectrum/latest`), new query
  parameters (`since`, `maxPoints`, `waitMs`), content negotiation (`Accept`,
  `Accept-Encoding`) and conditional requests (`If-None-Match`, `If-Match`).
* Additional JSON fields may appear at any time, so clients must ignore unknown fields.
  Fields are never removed or retyped within `das-v1`.

## 3. Discover, don't assume

`GET /api/capabilities` lists the opt-in features. A new frontend:

1. `404` means a legacy backend: poll the legacy endpoints and use the tolerant heartbeat.
2. `features.etag` means it sends `If-None-Match`.
3. `features.volatileDelta` means it polls `?since=<rev>`.
4. `features.wsTelemetry` / `wsSpectrum` means it uses the WebSocket channels and falls
   back to polling if the socket cannot be opened (proxy, firewall).
5. `features.dtf` means the Distance-to-Fault screen can be shown (`/api/dtf`).

## 4. Validators survive nothing

ETags contain a per-boot identifier. After a restart, failover or version switch no old
ETag can match, so a client can never keep stale data because of a 304.

Revisions start at a random per-boot base between 2^40 and 2^52 (like a TCP initial
sequence number). A revision kept from a previous boot is therefore older than the
server's history, or newer than its current revision. Either way the delta request
(`since`, `rev`) is answered with a full snapshot, never with a delta against data the
client does not hold. Clients must treat revisions as opaque integers: they increase within one boot, and a
client only sends back values the server gave it. It never computes a revision.

## 5. Conformance is tested, not promised

`conformance/run.js` runs the same checks against any base URL. Every backend
project runs it in CI:

```sh
node conformance/run.js --base http://127.0.0.1:8080
```

## 6. Versioning

* Additive changes keep `das-v1`.
* A breaking change needs a new major version `das-v2`, served in parallel under
  new paths (`/api/v2/...`, `das.v2` protobuf package) for at least one full release.
* Protobuf changes are checked with `buf breaking` against the previous release tag.

## 7. The NOC uplink is a separate contract

The Master Unit's connection **to the NOC** (das-06) is not part of `das-v1`: the browser
never sees it. It is `das-noc.v1`, an outbound WebSocket from the device, specified in
das-06 `docs/PROTOCOL.md` with the same rules: additive changes stay in v1, unknown
messages and fields are ignored, and a breaking change gets a new subprotocol served in
parallel. On the device it is implemented by a sidecar (`agent/noc-agent.js`) that reads
`/api/volatile-data` like the UI does, so it needs nothing new from the backends above.
