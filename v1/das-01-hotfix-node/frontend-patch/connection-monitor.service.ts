/**
 * Tolerant connection monitor (optional frontend part of the hotfix).
 *
 * Replaces "one timed-out heartbeat = server is dead" with a small state machine:
 *
 *   online   - heartbeat answered quickly
 *   degraded - heartbeat slow, server reports "degraded", or a few misses in a row
 *   offline  - N consecutive misses AND no successful API response at all for M seconds
 *
 * Any successful HTTP response counts as proof of life (fed by LivenessInterceptor),
 * so a busy-but-alive server can never be declared dead while data still arrives.
 * Heartbeats never overlap (exhaustMap), and while offline the probe backs off.
 *
 * Compatible with Angular 14+ / RxJS 7.4+. No other dependencies.
 */
import { Inject, Injectable, InjectionToken, NgZone, OnDestroy, Optional } from '@angular/core';
import { HttpClient, HttpContext, HttpContextToken } from '@angular/common/http';
import { BehaviorSubject, Observable, Subscription, of, timer } from 'rxjs';
import { catchError, distinctUntilChanged, exhaustMap, map, timeout } from 'rxjs/operators';

export type ConnectionState = 'online' | 'degraded' | 'offline';

export interface ConnectionMonitorConfig {
  heartbeatUrl: string;
  intervalMs: number;
  timeoutMs: number;
  /** Heartbeat slower than this marks the connection "degraded" (not dead). */
  degradedLatencyMs: number;
  /** Consecutive failed heartbeats before "offline" may be declared ... */
  offlineAfterMisses: number;
  /** ... and only if no successful response of any kind arrived for this long. */
  offlineAfterSilenceMs: number;
  /** Maximum probe interval while offline (exponential back-off). */
  maxBackoffMs: number;
}

export const CONNECTION_MONITOR_CONFIG = new InjectionToken<Partial<ConnectionMonitorConfig>>('CONNECTION_MONITOR_CONFIG');

/** Mark requests that must not count as liveness evidence (e.g. the heartbeat itself). */
export const SKIP_LIVENESS = new HttpContextToken<boolean>(() => false);

const DEFAULTS: ConnectionMonitorConfig = {
  heartbeatUrl: '/api/heartbeat',
  intervalMs: 2000,
  timeoutMs: 5000,
  degradedLatencyMs: 1500,
  offlineAfterMisses: 3,
  offlineAfterSilenceMs: 15000,
  maxBackoffMs: 30000
};

/** Heartbeat body as sent by the server (see contract/openapi.yaml). */
export interface HeartbeatBody {
  status?: 'ok' | 'degraded';
  server?: string;
  services?: Record<string, string>;
  [key: string]: unknown;
}

interface Probe { ok: boolean; latencyMs: number; body?: HeartbeatBody }

@Injectable({ providedIn: 'root' })
export class ConnectionMonitorService implements OnDestroy {
  private readonly cfg: ConnectionMonitorConfig;
  private readonly stateSubject = new BehaviorSubject<ConnectionState>('online');
  private sub?: Subscription;
  private misses = 0;
  private lastOkAt = Date.now();

  /** Emits only on changes. Bind the "server is dead" banner to `state$ === 'offline'`. */
  readonly state$: Observable<ConnectionState> = this.stateSubject.pipe(distinctUntilChanged());
  /** Last successful heartbeat body (server name, per-service health behind a gateway). */
  readonly lastHeartbeat$ = new BehaviorSubject<HeartbeatBody | null>(null);
  lastLatencyMs = 0;

  constructor(
    private readonly http: HttpClient,
    private readonly zone: NgZone,
    @Optional() @Inject(CONNECTION_MONITOR_CONFIG) cfg: Partial<ConnectionMonitorConfig> | null
  ) {
    this.cfg = { ...DEFAULTS, ...(cfg || {}) };
  }

  get state(): ConnectionState { return this.stateSubject.value; }

  /** Call once, e.g. from the root component. */
  start(): void {
    if (this.sub) return;
    this.schedule(this.cfg.intervalMs);
  }

  /** Called by LivenessInterceptor for every successful response. */
  noteActivity(): void {
    this.lastOkAt = Date.now();
    if (this.stateSubject.value === 'offline') {
      this.misses = 0;
      this.zone.run(() => this.stateSubject.next('degraded'));
      this.schedule(this.cfg.intervalMs);
    }
  }

  ngOnDestroy(): void { this.sub?.unsubscribe(); }

  private schedule(periodMs: number): void {
    this.sub?.unsubscribe();
    // Timers run outside Angular so the app is not re-checked every tick.
    this.zone.runOutsideAngular(() => {
      this.sub = timer(0, periodMs).pipe(exhaustMap(() => this.probe())).subscribe((p) => this.evaluate(p));
    });
  }

  private probe(): Observable<Probe> {
    const started = performance.now();
    return this.http.get<HeartbeatBody>(this.cfg.heartbeatUrl, {
      headers: { 'Cache-Control': 'no-cache' },
      context: new HttpContext().set(SKIP_LIVENESS, true)
    }).pipe(
      timeout(this.cfg.timeoutMs),
      map((body) => ({ ok: true, latencyMs: performance.now() - started, body })),
      catchError(() => of({ ok: false, latencyMs: performance.now() - started }))
    );
  }

  private evaluate(p: Probe): void {
    const prev = this.stateSubject.value;
    let next: ConnectionState;
    this.lastLatencyMs = p.latencyMs;
    if (p.ok) {
      this.misses = 0;
      this.lastOkAt = Date.now();
      const body = p.body || null;
      this.zone.run(() => this.lastHeartbeat$.next(body));
      next = (body && body.status === 'degraded') || p.latencyMs > this.cfg.degradedLatencyMs ? 'degraded' : 'online';
    } else {
      this.misses++;
      const silentFor = Date.now() - this.lastOkAt;
      next = this.misses >= this.cfg.offlineAfterMisses && silentFor >= this.cfg.offlineAfterSilenceMs ? 'offline' : 'degraded';
    }
    if (next !== prev) this.zone.run(() => this.stateSubject.next(next));
    // Back off while offline; return to the normal cadence afterwards.
    if (next === 'offline' && prev !== 'offline') this.schedule(Math.min(this.cfg.maxBackoffMs, this.cfg.intervalMs * 4));
    if (next !== 'offline' && prev === 'offline') this.schedule(this.cfg.intervalMs);
  }
}
