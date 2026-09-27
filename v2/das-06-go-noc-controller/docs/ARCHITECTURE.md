# Architecture

## What the NOC is for

The NOC is the control plane of a fleet of DAS installations: thousands of Master Units in
stadiums, airports, hospitals and metro stations, each with up to a few hundred Remote
Nodes. NOC staff need one live picture of the fleet (which sites have problems, what
alarms are active, who is handling them) and must be able to drill into any site down to
the Remote Node table. Customers see the same for their own sites only.

It is **not** a historian (time series go to Prometheus or a TSDB), a configuration or
firmware push tool, or a replacement for the Master Unit's local web UI. A command channel
towards the devices would fit the same connection later (section "Extending").

## Shape

```
 venue (x 5 000)                                    cloud: one NOC process per fleet shard
┌──────────────────────────────────────┐          ┌───────────────────────────────────────────────┐
│ Remote Nodes ─ Master Unit           │          │  uplink        fleet store        dashboards  │
│   Node.js backend (unchanged)        │  wss://  │  (goroutine    (per-site state,   (writer per │
│     └─ /api/volatile-data            │ ───────▶ │   per device,   aggregates,        dashboard, │──▶ NOC page, customers
│   noc-agent (sidecar, outbound only) │ outbound │   admission     alarm log)         pull-based │
└──────────────────────────────────────┘  only    │   control)                         conflation)│──▶ REST API
                                                  │                  registry.json               │──▶ /metrics (Prometheus)
                                                  └───────────────────────────────────────────────┘
```

| Package | Responsibility |
|---|---|
| `internal/wsock` | WebSocket (RFC 6455) client and server on `net/http`: handshake, framing, ping/pong, close, size limits, permessage-deflate for dashboards. About 700 lines, so the NOC stays on the standard library. |
| `internal/proto` | The das-noc.v1 messages, the canonical summary hash, and the signed tokens. |
| `internal/uplink` | The device endpoint: authentication, admission control, sessions, keep-alive, message dispatch. |
| `internal/fleet` | The fleet state: sites, alarms, detail, the tenant and fleet aggregates, the alarm event log, the views, the registry file. |
| `internal/dash` | Dashboard push over WebSocket: subscriptions, conflation, alarm batches, gaps. |
| `internal/api` | HTTP routing, REST, security headers, internal metrics (JSON and Prometheus), pprof. |
| `internal/web` | The NOC page (plain HTML, CSS and JavaScript, embedded in the binary). |
| `internal/sim` | Simulated Master Units and dashboards, for tests and `fleetsim`. |
| `agent/noc-agent.js` | The device side, for the existing Node.js Master Unit software. |

## The devices are the source of truth

The NOC holds a **cache** of what the devices report, plus the few things only the NOC
knows (who acknowledged which alarm, which sites exist according to the inventory). The
cache can always be rebuilt: after a NOC restart every device sends its full summary and
its full list of active alarms, and the view is complete again. That single rule removes a
database from the design:

- a restart is an ordinary event (measured: 5 000 sites back in 32 s, docs/SCALE.md);
- there is no replication or failover protocol to get wrong; Kubernetes restarts the pod;
- the registry file (`<data-dir>/registry.json`, about 200 bytes per site, written every
  30 s and on shutdown, fsync + rename) only keeps what the devices cannot resend.

## Uplink: one goroutine per device

A handshake is checked in this order, cheapest first, and nothing is allocated for a device
before it passes: NOC shutting down → not a WebSocket → token (HMAC-SHA256, about a
microsecond) → **admission control** (a token bucket, 200 handshakes/s with a burst of
400) → capacity. A refused device gets `503` with a **randomised** `Retry-After`, so a fleet
that was cut off at the same moment does not come back at the same moment.

After the upgrade, the HTTP handler hands the connection to a new goroutine and returns.
Otherwise the HTTP server's per-connection state (a 4 KiB read buffer, a 4 KiB write
buffer, the parsed request) would live as long as the device stays connected, for days.
Measured at 5 000 devices, this cut the live heap from 86 MB to 33 MB and the RSS from
226 MB to 115 MB.

The session goroutine reads one message at a time and applies it to the store. It spends
almost all its life blocked in a read, which costs a goroutine stack (8 KiB here) and no
CPU. Keep-alive pings come from a `time.AfterFunc` timer per device, not from a second
goroutine. At 5 000 devices the process has 5 058 goroutines and 40 MB of stacks.

## Fleet store: incremental everything

Each site has its own mutex. Every change to a site goes through one function, `mutate`:

1. apply the change under the site lock (a delta, an alarm event, a disconnect);
2. derive the site's status (`critical`, `major`, `minor`, `warning`, `ok`, `stale`,
   `offline`) and its **contribution** (tenant, connected, status, alarm counts);
3. if the contribution changed, subtract the old one from the fleet and tenant aggregates
   and add the new one: the fleet totals are never recomputed by scanning 5 000 sites;
4. append the resulting alarm events to the event log, **encoded to JSON once**;
5. release the lock, then notify the dashboard hub. Callbacks never run under a lock.

The **overview** a dashboard shows (totals, per-tenant totals, the 25 most urgent sites)
is computed once per fleet version and per scope, and shared by every dashboard with that
scope. The problem board is a bounded selection, not a sort of the whole fleet.

The **alarm event log** is a ring of 50 000 events numbered `n`. Each dashboard has a
cursor into it. A dashboard that falls further behind than the ring gets a `gap` message
and reloads the active alarms; the fleet never waits for it.

## Dashboards: pull-based conflation

A change does not push data to dashboards: it only **wakes** the writer goroutine of the
dashboards that care (a one-slot channel, so a thousand changes are one wake-up). The
writer then reads the **current** state:

- the overview at most once a second, and only if the fleet version changed;
- the site under drill-down at most twice a second;
- new alarm events at once if the last batch is older than 50 ms, otherwise at the end of
  the 50 ms window; a backlog is drained in batches of up to 500 events, back-to-back (up to
  eight per wake-up, so overview and drill-down updates still get their turn).

The consequences are what makes 20 dashboards and a 20 000-alarm storm cheap:

- work is proportional to what dashboards **see**, not to what devices **send**;
- a slow dashboard skips intermediate states instead of queueing them, so memory per
  dashboard is bounded and a slow client cannot slow the fleet down
  (`TestSlowDashboardDoesNotSlowTheFleet`);
- an alarm batch is assembled from events that were encoded when they were logged, so 20
  dashboards cost 20 memory copies, not 20 × 20 000 JSON encodings.

Tenant scoping is applied when a dashboard reads (overview scope, event filter, site
access), so there is one code path for staff and customers. A site of another tenant
answers "not found", not "forbidden".

## Consistency checks

- **Summary:** revision chain plus canonical hash (docs/PROTOCOL.md). A broken chain or a
  hash mismatch triggers a full summary. The NOC counts mismatches per site
  (`hashMismatch`), which shows a buggy device software version at a glance.
- **Alarms:** sequence numbers, a cumulative ack, and the device-side outbox: an alarm is
  never lost by a reconnect, and never counted twice. A gap triggers the full active list.
- **Resync throttle:** at most one resync of each kind per site per 30 s.
- **Takeover:** two connections with the same site identity (a device that reconnected
  before its old socket timed out, or a cloned device) resolve to the newest, everywhere.
- The scale test checks the whole fleet three times: every device's own summary hash and
  alarm counts against what the NOC shows. Result: 0 differences out of 5 000, three times.

## Failure behaviour

| Event | What happens |
|---|---|
| NOC restart (deploy, crash, node drain) | Devices are closed with 1012, reconnect with jitter through admission control, and resend summary and alarms. Dashboards reconnect and reload. Sites are `stale`, not `offline`, meanwhile: no alarm flood. |
| A venue loses its uplink | The site is `stale` for 90 s, then `offline` with a critical `SITE_UNREACHABLE` alarm raised by the NOC. It clears on reconnect. Events that happened meanwhile are in the device's outbox and arrive then. |
| Device reboot | New `boot`: the NOC takes the full summary and alarm list; alarms that disappeared are cleared. |
| Half-open TCP (NAT timeout) | No frame for 62.5 s (2.5 × keep-alive): the NOC closes; the device notices the same way and reconnects. |
| Alarm storm (1 000 sites × 20 alarms in seconds) | Measured: p99 device-to-dashboard 674 ms (501 ms in an earlier run), 17 % of one core, 0 gaps, 0 events missing. |
| Slow or stalled dashboard | Skips states; if it falls 50 000 events behind: `gap` and reload. Writes time out after 10 s and the dashboard is dropped. |
| Buggy device (bad deltas, wrong hashes) | Throttled resyncs; the site keeps working; `hashMismatch` and `badMessages` show it. |
| Stolen or replaced Master Unit | Revoke `site:<id>`: the connection is closed within 10 s and reconnects are refused. |
| Memory pressure | `GOMEMLIMIT` makes the GC work harder before the container limit is reached. |

## Why a single process per shard

A NOC process holds the connections of its devices. Two processes behind one hostname would
each hold a random half of the fleet, and every dashboard would see half of it. Making that
work needs a shared state (a database or a message bus between instances), which costs more
in operations than it buys. A 32-second restart is acceptable for a NOC view, whose
underlying facts are never lost (the devices keep them). Scale beyond one process is
**sharding**: one NOC per region or per group of customers, each with its own hostname and
its own site tokens (docs/OPERATIONS.md). One process on one core handled 5 000 sites at
14 % CPU and 10 000 at 22 %, so a shard can be large.

## Extending

- **Commands to devices** (reboot a Remote Node, start a spectrum sweep on the das-05
  service): a new NOC→device message type with a request id and a device→NOC result. The
  connection, the authentication and the tenant checks are already there.
- **History:** Prometheus already scrapes fleet totals. For per-site history, export the
  alarm event log (it is ordered and numbered) to a TSDB or an event store.
- **Single sign-on** for people: see docs/OPERATIONS.md.
