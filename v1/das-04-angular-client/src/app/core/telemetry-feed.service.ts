import { HttpClient } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Subscription } from 'rxjs';
import { TelemetryMessage, VolatileDelta, VolatileSnapshot } from './api.types';
import { CapabilitiesService } from './capabilities.service';
import { ConnectionMonitorService } from './connection/connection-monitor.service';
import { pollSequential } from './connection/poll';
import { EMPTY_STATE, TelemetryState, applyDelta, applySnapshot, isDelta } from './telemetry-model';

export type TelemetryTransport = 'connecting' | 'websocket' | 'http-delta' | 'http-poll';

/**
 * volatile_data for the whole app, as a signal. Picks the best transport the
 * backend offers:
 *   WebSocket push (gateway / Go edge)  ->  HTTP ?since= deltas (hotfix)  ->  full polling (legacy)
 * On a WebSocket drop it reconnects with back-off and resumes from the last
 * revision. If WebSockets cannot be opened at all (proxy, firewall) it falls
 * back to polling. Components only ever see `state()`.
 */
@Injectable({ providedIn: 'root' })
export class TelemetryFeed {
  private readonly http = inject(HttpClient);
  private readonly caps = inject(CapabilitiesService);
  private readonly monitor = inject(ConnectionMonitorService);
  private ws?: WebSocket;
  private pollSub?: Subscription;
  private started = false;
  private failures = 0;
  private everOpened = false;

  readonly state = signal<TelemetryState>(EMPTY_STATE);
  readonly transport = signal<TelemetryTransport>('connecting');
  readonly lastUpdateAt = signal(0);
  readonly received = signal({ messages: 0, chars: 0, resyncs: 0 });

  start(): void {
    if (this.started) return;
    this.started = true;
    void this.caps.load().then((c) => {
      if (c?.features.wsTelemetry && typeof WebSocket !== 'undefined') this.connect();
      else this.poll(!!c?.features.volatileDelta);
    });
  }

  private count(chars: number, resync = false): void {
    const r = this.received();
    this.received.set({ messages: r.messages + 1, chars: r.chars + chars, resyncs: r.resyncs + (resync ? 1 : 0) });
  }

  private connect(): void {
    this.transport.set('connecting');
    const url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/ws/telemetry';
    const ws = new WebSocket(url);
    this.ws = ws;
    ws.onopen = () => {
      this.everOpened = true;
      this.failures = 0;
      this.transport.set('websocket');
      const rev = this.state().rev;
      ws.send(JSON.stringify({ type: 'sub', rev: rev >= 0 ? rev : null })); // resume after a reconnect
    };
    ws.onmessage = (ev: MessageEvent<string>) => {
      this.monitor.noteActivity(); // any message proves the server is alive
      const m = JSON.parse(ev.data) as TelemetryMessage;
      if (m.type === 'snapshot') {
        this.state.set(applySnapshot(m));
        this.lastUpdateAt.set(Date.now());
        this.count(ev.data.length);
      } else if (m.type === 'delta') {
        const next = applyDelta(this.state(), m);
        if (next) {
          this.state.set(next);
          this.lastUpdateAt.set(Date.now());
          this.count(ev.data.length);
        } else {
          // Missed something: ask for exactly what is missing.
          ws.send(JSON.stringify({ type: 'sub', rev: this.state().rev }));
          this.count(ev.data.length, true);
        }
      }
    };
    ws.onclose = () => {
      if (this.ws !== ws || !this.started) return;
      this.ws = undefined;
      this.failures++;
      if (!this.everOpened && this.failures >= 3) {
        this.poll(true); // WebSocket blocked somewhere on the path: degrade gracefully
        return;
      }
      this.transport.set('connecting');
      setTimeout(() => this.connect(), Math.min(15000, 500 * 2 ** Math.min(this.failures, 5)));
    };
  }

  private poll(delta: boolean): void {
    this.transport.set(delta ? 'http-delta' : 'http-poll');
    this.pollSub?.unsubscribe();
    this.pollSub = pollSequential(() => {
      const rev = this.state().rev;
      const url = delta && rev >= 0 ? `/api/volatile-data?since=${rev}` : '/api/volatile-data';
      return this.http.get<VolatileSnapshot | VolatileDelta>(url);
    }, 2000).subscribe((body) => {
      const next = isDelta(body) ? applyDelta(this.state(), body) : applySnapshot(body);
      if (next && next !== this.state()) {
        this.state.set(next);
        this.lastUpdateAt.set(Date.now());
      }
      this.count(0, !next);
      if (!next) this.state.set({ ...this.state(), rev: -1 }); // next poll fetches a snapshot
    });
  }
}
