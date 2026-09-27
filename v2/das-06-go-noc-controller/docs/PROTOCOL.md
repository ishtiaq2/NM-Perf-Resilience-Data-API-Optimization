# das-noc.v1: the Master Unit uplink protocol

This is the normative description of how a Master Unit reports to the NOC. Two
independent implementations follow it: the NOC (`internal/proto`, `internal/uplink`,
`internal/fleet`, Go) and the device agent (`agent/noc-agent.js`, Node.js). The Go test
suite checks the canonical hash against values produced by the JavaScript implementation,
and `scripts/agent-demo.sh` checks the two end to end.

MUST, SHOULD and MAY are used as in RFC 2119.

## 1. Design in one paragraph

The Master Unit dials out (no inbound port at the venue) and keeps one WebSocket open. It
sends its **summary** (a small JSON object: name, node counts, worst temperature, …) once,
then only the fields that changed (**deltas**), chained by revision numbers and checked with
a **hash**. It sends **alarm events** numbered by a sequence number and keeps them until the
NOC **acknowledges** them. It streams **node-level detail** only while an operator is looking
at the site. The NOC keeps no state that the device cannot resend: after a NOC restart,
every device sends its full summary and its full active-alarm list, and the fleet view is
complete again.

## 2. Transport and authentication

- **Endpoint:** `GET /uplink/v1`, WebSocket (RFC 6455), over TLS (`wss://`) in production.
- **Subprotocol:** the device MUST offer `das-noc.v1`. The NOC selects it; a connection
  without it is closed with 1008.
- **Token:** the device presents its site token either as `Authorization: Bearer <token>`
  or as a second offered subprotocol `bearer.<token>` (for WebSocket client APIs that cannot
  set headers, such as the browser-style `WebSocket` in Node.js 22). The NOC never echoes
  the token subprotocol.
- **Identity comes from the token, not from the device.** The token carries the site id and
  the tenant (customer); what the device says in its messages cannot change either.
- **No `Origin` header:** the uplink refuses requests that carry one, so a web page cannot
  open an uplink even with a stolen token in hand.
- **Frames:** text frames, one JSON object each, at most 1 MiB. Binary frames are not used.

### Handshake responses

| Status | Meaning | Device action |
|---|---|---|
| 101 | Upgraded | Send `hello` within 10 s. |
| 401 | Missing, malformed, wrongly signed or revoked token | Retry slowly (minutes); alert locally. It will not fix itself. |
| 426 | Not a WebSocket upgrade request | Fix the client. |
| 503 + `Retry-After: N` | Admission control (too many handshakes per second), capacity, or NOC restarting | Wait `N` seconds (randomised by the NOC, 1 to 15 s by default), then retry. |

### Tokens

```
v1.<kind>.<subject>.<tenant>.<signature>
v1.site.S00042.airport-01.Zk3…   (a Master Unit)
v1.user.alice.*.Qm9…            (NOC staff: "*" = every tenant)
v1.user.bob.airport-01.c2F…     (a customer's user: one tenant)
```

`kind` is `site` or `user`; `subject` and `tenant` are 1 to 64 characters of
`[A-Za-z0-9_-]` (`tenant` may be `*` for users only). The signature is
HMAC-SHA256(secret, `v1.<kind>.<subject>.<tenant>`), base64url without padding. Tokens are
issued with `noc token -kind site -subject S00042 -tenant airport-01`. The NOC accepts the
current secret and, during a rotation, the previous one; revoked subjects are refused
(docs/OPERATIONS.md).

## 3. Messages

Every message is a JSON object with a type field `t`. Unknown types MUST be ignored by both
sides, and unknown fields MUST be ignored: that is how the protocol grows without a version
bump.

| Direction | `t` | Fields |
|---|---|---|
| device → NOC | `hello` | `boot`, `fw`, `agent`, `caps` |
| NOC → device | `welcome` | `site`, `tenant`, `session`, `keepaliveS`, `have` (optional) |
| device → NOC | `summary` | `rev`, `hash`, `s` |
| device → NOC | `delta` | `base`, `rev`, `hash`, `s` |
| device → NOC | `alarms` | `ev`, or `full: true` + `upTo` + `ev` |
| NOC → device | `ack` | `seq` |
| NOC → device | `resync` | `what`: `summary`, `alarms` or `detail`; `reason` |
| NOC → device | `detail-sub` | `on`, `intervalMs` |
| device → NOC | `detail` | `rev`, and `full: true` or `base`; `nodes`, `gone` |
| device → NOC | `ping` | `seq` |
| NOC → device | `pong` | `seq` |

### 3.1 hello and welcome

```json
{"t":"hello","boot":"9f1c2a7be04d","fw":"3.1.4","agent":"noc-agent.js/1","caps":["detail"]}
{"t":"welcome","site":"S00042","tenant":"airport-01","session":"k3v9x0","keepaliveS":25}
```

`boot` identifies one run of the device's sequence space. It MUST change whenever the
device loses its revision counter, its alarm sequence counter or its outbox (a reboot, an
agent restart), and SHOULD be random (12 hex digits are plenty).

`have` is present when the NOC still holds state from the **same boot**:

```json
{"t":"welcome","site":"S00042","tenant":"airport-01","session":"k3va01","keepaliveS":25,
 "have":{"boot":"9f1c2a7be04d","rev":57,"hash":"4ff86457e6641c6a","ackSeq":1204}}
```

After `welcome` the device MUST bring the NOC up to date:

1. **No `have`, or `have.boot` differs** (NOC restarted, or the device rebooted): send a
   full `summary`, then the full active-alarm list (`alarms` with `full: true`).
2. **Same boot:**
   - if `have.rev` or `have.hash` differs from its own, send a full `summary`;
   - drop acknowledged events (`seq <= have.ackSeq`) from the outbox and resend the rest;
     if the oldest remaining is not `have.ackSeq + 1` (events were lost, for example the
     outbox overflowed), send the full active-alarm list instead.

### 3.2 summary and delta

```json
{"t":"summary","rev":57,"hash":"0cc469c8e64bfdee","s":{"name":"Terminal 2","venue":"Airport North",
 "region":"north","fw":"3.1.4","nodes":300,"online":298,"offline":2,"degraded":1,
 "maxTempC":47.5,"minRxDbm":-8.3,"maxVswr":1.31}}
{"t":"delta","base":57,"rev":58,"hash":"4ff86457e6641c6a","s":{"online":299,"offline":1,"maxTempC":48}}
```

- `s` in a `summary` is the complete summary: a JSON object whose values may be any JSON
  except `null`.
- `s` in a `delta` holds only the top-level fields that changed; `null` removes a field.
  (So a summary never holds `null`: leave unknown values out.)
- `rev` increases with every summary or delta; `base` is the revision the delta applies to.
- `hash` is the canonical hash (section 4) of the complete summary **after** the message.
- The NOC applies a delta only if `base` equals its current revision. Otherwise, or if its
  own hash of the result differs from `hash`, it answers `resync` with `what: "summary"`,
  and the device MUST send a full `summary`.
- Fields the NOC interprets (lists, sorting, problem ranking): `name`, `venue`, `region`,
  `nodes`, `online`, `offline`, `degraded`, `maxTempC`, `minRxDbm`, `maxVswr`. Other fields
  (`fw`, for instance) are stored, hashed and shown, but not aggregated; the software version
  the NOC displays is the one from `hello`.
- A device SHOULD round measured values (0.5 °C, 0.1 dB, 0.01 VSWR) before comparing, so
  that noise does not produce a delta every few seconds.

### 3.3 alarms

An alarm is identified within its site by a stable `id` (for example `n17:TEMP_HIGH`: node
17, code `TEMP_HIGH`). Each raise and each clear is an event:

```json
{"t":"alarms","ev":[
 {"seq":1205,"id":"n17:TEMP_HIGH","node":17,"code":"TEMP_HIGH","sev":"major","state":"raised","at":1790500000123},
 {"seq":1206,"id":"n4:PSU_FAIL","node":4,"code":"PSU_FAIL","sev":"critical","state":"raised","at":1790500000456,
  "text":"mains failure, on battery"}]}
{"t":"ack","seq":1206}
```

- `seq` starts at 1 for each `boot` and increases by exactly 1 per event.
- `sev` is `critical`, `major`, `minor` or `warning`; `state` is `raised` or `cleared`;
  `at` is the device clock in ms since the Unix epoch; `node` is 0 or absent for the Master
  Unit itself; `text` is optional.
- The device keeps events in an **outbox** until an `ack` with `seq` ≥ theirs arrives. `ack`
  is cumulative.
- The NOC ignores events with `seq` ≤ what it has stored (retransmissions after a reconnect).
  If the next event is not the next number (a gap), it stops, acknowledges what it has, and
  sends `resync` with `what: "alarms"`.
- **Full list:** `{"t":"alarms","full":true,"upTo":1206,"ev":[…every active alarm, state
  "raised"…]}` replaces the NOC's active set for the site. `upTo` is the device's current
  `seq`; alarms missing from the list are cleared. The NOC acknowledges `upTo`. A device
  SHOULD send its events sorted by `seq`.
- Operator acknowledgements (someone at the NOC has seen the alarm) are NOC-side only and are
  not sent to the device. They survive NOC restarts and are forgotten when the alarm clears.

### 3.4 detail (drill-down)

Node-level data (the Remote Node table) is only streamed while someone looks at the site:

```json
{"t":"detail-sub","on":true,"intervalMs":1000}
{"t":"detail","rev":1,"full":true,"nodes":[{"id":1,"name":"RN-001","type":"Stratus","chain":1,"hop":1,
 "status":"online","tempC":45.3,"rxDbm":-7.1,"txDbm":1.7,"vswr":1.08,"psuV":47.86,"fw":"4.2.1"}, …]}
{"t":"detail","base":1,"rev":2,"nodes":[{"id":17,…}],"gone":[212]}
{"t":"detail-sub","on":false}
```

- On `detail-sub` with `on: true`, the device sends a full snapshot, then at most one
  message per `intervalMs` with the nodes that changed (`nodes`) and the ids that
  disappeared (`gone`), chained by `base`/`rev`. With `on: false` it stops.
- Each node object MUST have an integer `id`; the other fields are free-form and are shown
  as they are.
- A broken chain gets `resync` with `what: "detail"`: send a full snapshot.
- After a reconnect, the NOC sends `detail-sub` again if someone is still watching.

### 3.5 keep-alive and closing

- The NOC sends a WebSocket **ping** every `keepaliveS` seconds (25 by default; the first one
  after a random 50 to 100 % of the interval, so a reconnected fleet does not ping in step).
  A device MUST answer with a pong (WebSocket libraries do this automatically).
- The NOC closes a connection from which it has received nothing for 2.5 × `keepaliveS`.
- A device MAY send `{"t":"ping","seq":n}`; the NOC answers `{"t":"pong","seq":n}`. This is for
  client APIs that hide WebSocket pings: without it, a device behind a NAT that silently
  dropped the connection could not notice. A device SHOULD reconnect when it has received
  nothing for 2.5 × `keepaliveS`.
- Close codes from the NOC: **1012** (NOC restarting: reconnect, with jitter), **4000**
  (replaced by a newer connection with the same site token), **1008** (protocol violation,
  or the token was revoked).
- **Reconnecting:** exponential backoff with full jitter, from 1 s up to 60 s, and at least
  the `Retry-After` of a 503. A fleet reconnecting in lockstep is what admission control is
  for, but the jitter is what spreads it.

## 4. Canonical hash

Both sides compute the same 64-bit hash of the complete summary, so that a single wrong
delta is detected instead of silently drifting.

1. **Canonical JSON** of the summary object:
   - object keys sorted ascending; no whitespace anywhere;
   - strings as `JSON.stringify` writes them: `\"`, `\\`, `\b`, `\f`, `\n`, `\r`, `\t`, other
     control characters below U+0020 as `\u00xx` (lowercase hex), everything else,
     including U+2028, U+2029, `<`, `>` and `&`, as the character itself;
   - numbers as JavaScript's `Number.prototype.toString`: the shortest digits that round-trip,
     exponent form below 1e-6 and from 1e21 up (`1e-7`, `1e+21`, `1.5e-10`), `-0` as `0`;
   - `true`, `false`, `null`, and arrays in their order.
2. **FNV-1a 64** over the UTF-8 bytes (offset basis `cbf29ce484222325`, prime
   `100000001b3`), written as 16 lowercase hex digits.

Portability rules: keys SHOULD be ASCII (JavaScript sorts by UTF-16 code units and Go by
UTF-8 bytes; the two orders differ only for characters above U+FFFF), and strings MUST be
valid Unicode (no lone surrogates).

Test vectors (checked by `TestProtocolDocExamples` and `TestCanonicalMatchesNodeJS`):

| Summary | Hash |
|---|---|
| `{}` | `08f44b07b5901a25` |
| `{"b":[1,2,{"d":null,"c":"é"}],"a":1e21,"z":0.1}` → canonical `{"a":1e+21,"b":[1,2,{"c":"é","d":null}],"z":0.1}` | `9e3cfb1f27fec5aa` |
| the `summary` example in 3.2 | `0cc469c8e64bfdee` |
| the same after the `delta` example | `4ff86457e6641c6a` |

The reference implementation is 10 lines of JavaScript (`canonical()` and `fnv1a64()` in
`agent/noc-agent.js`). FNV-1a is not a cryptographic hash and does not need to be: it detects
drift between two honest parties; authenticity comes from the token and TLS.

## 5. NOC-side guarantees and limits

- **Resync throttle:** at most one `resync` of each kind per site per 30 s, so a device whose
  hash disagrees for a structural reason cannot put the two into a loop.
- **One connection per site:** a second connection with the same site token takes over
  (the NOC closes the older one with 4000). Messages arriving on the old one are dropped.
- **Stale and offline:** a site that disconnects is `stale` for 90 s (`-grace`), then
  `offline`, and the NOC raises its own critical alarm `SITE_UNREACHABLE`, which clears when
  the site connects again. A NOC restart therefore does not produce a flood of alarms.
- **Limits:** 1 MiB per message; `hello` within 10 s; handshakes admitted at 200/s with a
  burst of 400 by default; 20 000 concurrent devices by default (`-max-sites`).

## 6. A complete session

```
device                                                  NOC
  GET /uplink/v1  Sec-WebSocket-Protocol: das-noc.v1, bearer.v1.site.S00042.airport-01.…
                                                        101 Switching Protocols (das-noc.v1)
  {"t":"hello","boot":"9f1c2a7be04d","fw":"3.1.4",…}
                                                        {"t":"welcome","site":"S00042",…}   (no "have": first contact)
  {"t":"summary","rev":1,"hash":"0cc469c8e64bfdee","s":{…}}
  {"t":"alarms","full":true,"upTo":0,"ev":[]}          (no active alarms; nothing to acknowledge yet)
  … 5 s later, a node overheats …
  {"t":"delta","base":1,"rev":2,"hash":"…","s":{"maxTempC":49.5}}
  {"t":"alarms","ev":[{"seq":1,"id":"n17:TEMP_HIGH",…,"state":"raised"}]}
                                                        {"t":"ack","seq":1}
  … an operator opens the site …
                                                        {"t":"detail-sub","on":true,"intervalMs":1000}
  {"t":"detail","rev":1,"full":true,"nodes":[…300 nodes…]}
  {"t":"detail","base":1,"rev":2,"nodes":[{"id":17,…}]}
  … the link drops; the device reconnects with the same boot …
  {"t":"hello","boot":"9f1c2a7be04d",…}
                                                        {"t":"welcome",…,"have":{"boot":"9f1c2a7be04d","rev":2,"hash":"…","ackSeq":1}}
  (the outbox is empty and rev/hash match: nothing to send)
```

## 7. Versioning

- Additive changes (new message types, new optional fields, new summary fields) stay in
  `das-noc.v1`: receivers ignore what they do not know.
- A change that breaks an existing rule becomes `das-noc.v2`. The NOC can accept both
  subprotocols during a migration and answer each device in the version it selected.
- `hello.caps` announces optional device features (today: `detail`).
