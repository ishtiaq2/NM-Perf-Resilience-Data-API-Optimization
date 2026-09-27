# Operations

## Deploying

**Kubernetes** (`deploy/k8s`, one namespace, kustomize):

```sh
make image VERSION=0.6.0 && docker push registry.example.com/das-noc:0.6.0   # or your CI
kubectl create namespace das-noc
kubectl -n das-noc create secret generic das-noc-secret --from-literal=current="$(head -c 32 /dev/urandom | base64)"
kubectl apply -k deploy/k8s
```

One replica, `strategy: Recreate`, a 1 GiB volume for the registry, probes on the internal
port, `GOMEMLIMIT=400MiB` under a 512 MiB limit, non-root, read-only root filesystem, no
capabilities. The ingress terminates TLS and must allow long-lived WebSockets (the
manifest sets 3 600 s timeouts; the NOC's keep-alive traffic every 25 s keeps idle
connections open). `networkpolicy.yaml` lets only the ingress controller reach the public
port and only the monitoring namespace reach the internal one.

**A VM or bare metal:** `make dist` builds static binaries for linux/amd64 and
linux/arm64; `deploy/systemd/das-noc.service` is a sandboxed unit (raise `LimitNOFILE`:
every Master Unit is one descriptor). Put nginx, HAProxy or a cloud load balancer with TLS
in front, or give the NOC its certificate with `-tls-cert`/`-tls-key`.

**The Master Unit side:** `agent/noc-agent.js` with `deploy/device/das-noc-agent.service`
and its environment file (the NOC URL, the site token, the local API). It needs Node.js 22
(global `WebSocket`) or the `ws` package, which the das-02 gateway stack already has. It
makes outbound connections only.

## Configuration

Every flag can be given as an environment variable: `-admit-rate` is `DAS_NOC_ADMIT_RATE`.

| Flag | Default | |
|---|---|---|
| `-listen` | `:8080` | Devices, dashboards, REST, the NOC page. |
| `-internal-listen` | `127.0.0.1:9090` | `/metrics`, `/healthz`, `/debug/pprof/`. `:9090` in a container; never through the ingress. |
| `-secret-file` / `DAS_NOC_SECRET` | | Token signing secret, at least 16 bytes (use 32 random bytes). |
| `-secret-prev-file` | | The previous secret during a rotation; ignored if the file does not exist. |
| `-revoked-file` | | Revoked token subjects; re-read within 10 s of a change, and on SIGHUP. |
| `-inventory` | | JSON list of expected sites `[{id, tenant, name, venue, region}]` (`deploy/examples/inventory.json`). |
| `-data-dir` | | Registry file (known sites, last seen, acknowledgements). Without it, a restart forgets acknowledgements. |
| `-grace` | `90s` | Disconnected sites stay `stale` this long before `offline` + `SITE_UNREACHABLE`. |
| `-keepalive` | `25s` | Ping interval towards devices; a silent connection is closed after 2.5 ×. Keep it under the shortest NAT timeout on the venue side. |
| `-admit-rate` / `-admit-burst` | `200` / `400` | Device handshakes per second. Raise with CPU headroom; 200/s brings 5 000 sites back in about 30 s. |
| `-retry-after-max` | `15` | Refused devices retry after a random 1..N s. |
| `-max-sites` | `20000` | Concurrent device connections. |
| `-tls-cert` / `-tls-key` | | Serve TLS directly (without an ingress). |
| `-log` / `-log-level` | `text` / `info` | `json` in production. |

`GOMEMLIMIT` should be about 80 % of the container memory limit. `GOMAXPROCS` should match
the CPU request (Go 1.24 does not read cgroup CPU quotas; Go 1.25 does).

## Tokens

**Issue** a site token when a Master Unit is commissioned and store it on the unit only
(`/etc/default/das-noc-agent`, mode 0600):

```sh
DAS_NOC_SECRET_FILE=/path/to/secret noc token -kind site -subject S00042 -tenant airport-01
DAS_NOC_SECRET_FILE=/path/to/secret noc token -kind user -subject alice -tenant '*'          # NOC staff
DAS_NOC_SECRET_FILE=/path/to/secret noc token -kind user -subject airport-ops -tenant airport-01
```

The site id and the tenant are in the token, so a device cannot claim to be another site,
and moving a site to another customer means issuing a new token.

**Revoke** a lost, stolen or replaced Master Unit, or a person who left: add `site:S00042`
or `user:alice` to the revoked file (the ConfigMap in Kubernetes). The NOC reloads it within
10 s of the change (a ConfigMap update takes up to a minute to reach the pod), closes the
affected connections, and refuses their reconnects. REST calls check every request.

**Rotate** the secret (yearly, or at once if it may have leaked):

1. Put the new secret in `current` and the old one in `previous`; restart the NOC. Both are
   accepted now.
2. Re-issue every site and user token with the new secret and deliver them.
3. Remove `previous`; restart. Tokens signed with the old secret stop working.

**Known gap:** user tokens do not expire. That suits service accounts and a small NOC
team; for customer users it is not enough. Before giving customers access, put single
sign-on in front: an OIDC proxy at the ingress that authenticates the person, plus a small
service that mints a short-lived user token for their tenant (the token format would gain an
expiry field in a `v2`). Neither is implemented here.

## TLS, and client certificates for devices

TLS terminates at the ingress. For defence in depth, devices can also authenticate with a
client certificate (mTLS) issued by your own device CA. Use a **separate hostname** for the
uplink, because browsers must not be asked for a certificate:

- `uplink.noc.example.com`: path `/uplink/v1` only, client certificate required
  (ingress-nginx: `nginx.ingress.kubernetes.io/auth-tls-secret`,
  `auth-tls-verify-client: "on"`); the token is still checked by the NOC.
- `noc.example.com`: the NOC page, REST and dashboards; no client certificate.

## Capacity and sharding

Measured on one core (docs/SCALE.md): 5 000 sites at 14 % CPU and 127 MB RSS in steady
state (143 MB at most), 17 % CPU and 199 MB RSS during a 20 000-alarm storm, and the whole
fleet back 32 s after a restart; 10 000 sites at 22 % CPU and 232 MB. Memory grows by roughly 20 KB per site (goroutine stack, buffers, state),
plus the alarm log.

When one process is no longer enough, or when the blast radius of a restart should be
smaller, **shard**: one NOC deployment per region or per group of customers, each with its
own hostname, secret and site tokens. A site belongs to exactly one shard (its token says
where it may connect). A fleet-wide view is then a page that reads each shard's REST API;
it is not included here.

## Upgrades and restarts

`kubectl rollout restart` (or a new image) stops the NOC: it closes every uplink with 1012
("restart"), saves the registry and exits within a second or two; the new process starts,
and devices come back through admission control. Sites show `stale` for up to the grace
period meanwhile, so a routine restart raises no alarms. Dashboards reconnect by
themselves.

## Monitoring

`/metrics` on the internal port is in Prometheus text format (`?format=json` for the
same document as JSON). Useful alerts:

| Alert | Expression (PromQL) |
|---|---|
| NOC down | `up{job="das-noc"} == 0` |
| Many sites lost at once (network event, ingress problem) | `(max_over_time(das_noc_fleet_connected[10m]) - das_noc_fleet_connected) > 0.05 * das_noc_fleet_sites` |
| Sites unreachable (after the grace period) | `das_noc_sites{status="offline"} > 0` |
| Devices with bad or revoked tokens knocking | `increase(das_noc_uplink_rejected_total{reason="auth"}[15m]) > 10` |
| Cloned device token (two devices, one identity) | `increase(das_noc_uplink_replaced_total[15m]) > 5` |
| Device software computing wrong summaries | `increase(das_noc_fleet_hash_mismatch_total[1h]) > 0` |
| Dashboards falling behind the alarm log | `increase(das_noc_dashboard_gaps_total[15m]) > 0` |
| Memory near the limit | `das_noc_process_rss_mb > 450` |

Grafana panels worth having: connected sites, sites by status, active alarms by severity,
device messages per second (`rate(das_noc_uplink_messages_total[1m])`), alarm events per
second, CPU (`rate(das_noc_process_cpu_seconds_total[1m])`), RSS and live heap, GC pause p99
(`das_noc_process_gc_pause_p99_ms`).

## Profiling

```sh
kubectl -n das-noc port-forward svc/das-noc-internal 9090
go tool pprof http://127.0.0.1:9090/debug/pprof/heap
go tool pprof http://127.0.0.1:9090/debug/pprof/profile?seconds=30
curl -s 'http://127.0.0.1:9090/debug/pprof/goroutine?debug=1' | head -50
```

## Logs

JSON lines on stdout (`-log json`). Events: `noc_listening`, `registry_restored`,
`inventory_loaded`, `fleet` (a one-line summary every minute: sites, connected, critical,
offline, alarms, dashboards), `revoked_list_loaded`, `registry_save_failed`,
`listener_failed`, `shutting_down`. Per-device events are not logged at info level: at
5 000 sites they would be noise; the metrics and the site views carry them.

## Backups

`registry.json` holds what the devices cannot resend: the inventory of sites ever seen,
last-seen times, and alarm acknowledgements. Losing it loses acknowledgements (alarms show
as unacknowledged again); nothing else. A volume snapshot a day is plenty.

## Runbook

| Symptom | Look at | Likely cause |
|---|---|---|
| Every site `stale` at once | NOC restarts (`noc_listening` in the log), ingress logs | NOC restart, ingress reload that dropped connections, network event between the venues and the cloud |
| One site flapping connected/disconnected | `das_noc_uplink_replaced_total`, the site's `remote` address | Two Master Units with the same token (a cloned configuration), or a flaky venue uplink |
| A site stays `offline` | Is the agent running on the Master Unit? Its log (`journalctl -u das-noc-agent`) | Token revoked or wrong secret (401), outbound 443 blocked at the venue, DNS |
| `hashMismatch` grows for one software version | Site views, grouped by `fw` | A device-side bug in the summary or the canonical hash |
| Latency of alarm updates rises | CPU, `das_noc_dashboard_wakeups_total`, dashboard count | Too many dashboards or sites for one core: raise the CPU request or shard |
