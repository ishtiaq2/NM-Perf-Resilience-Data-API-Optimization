import { HttpClient, HttpResponse } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { Subscription, defer, timer } from 'rxjs';
import { repeat, retry } from 'rxjs/operators';
import { LegacySpectrum } from './api.types';
import { CapabilitiesService } from './capabilities.service';
import { ConnectionMonitorService } from './connection/connection-monitor.service';
import { DSPC_CONTENT_TYPE, SpectrumTrace, decodeFrame, peakDecimate } from './spectrum-frame';

export type SpectrumTransport = 'idle' | 'websocket' | 'http-binary' | 'http-legacy';

export interface SpectrumSelection {
  nodeId: number;
  port: number;
  /** Canvas width in device pixels: the server decimates to this (peak detector). */
  maxPoints: number;
}

/**
 * Spectrum traces for the analyzer view. Only active while the view is open.
 *   WebSocket binary push (gateway / Go edge) -> HTTP binary long-poll (hotfix) -> legacy JSON polling
 */
@Injectable({ providedIn: 'root' })
export class SpectrumFeed {
  private readonly http = inject(HttpClient);
  private readonly caps = inject(CapabilitiesService);
  private readonly monitor = inject(ConnectionMonitorService);
  private ws?: WebSocket;
  private sub?: Subscription;
  private active = false;
  private sel: SpectrumSelection = { nodeId: 1, port: 1, maxPoints: 1600 };
  private lastFrameAt = 0;

  readonly trace = signal<SpectrumTrace | null>(null);
  readonly transport = signal<SpectrumTransport>('idle');
  readonly stats = signal({ frames: 0, bytes: 0, updatesPerSec: 0 });

  start(sel: SpectrumSelection): void {
    this.sel = { ...sel };
    this.active = true;
    void this.caps.load().then(() => this.open());
  }

  /** Change analyzer or canvas width. The WebSocket re-subscribes in place; HTTP restarts its loop. */
  update(sel: SpectrumSelection): void {
    const changedAnalyzer = sel.nodeId !== this.sel.nodeId || sel.port !== this.sel.port;
    this.sel = { ...sel };
    if (!this.active) return;
    if (changedAnalyzer) this.trace.set(null);
    if (this.ws && this.ws.readyState === WebSocket.OPEN) this.subscribe(this.ws);
    else if (this.sub) this.open();
  }

  stop(): void {
    this.active = false;
    this.ws?.close();
    this.ws = undefined;
    this.sub?.unsubscribe();
    this.sub = undefined;
    this.transport.set('idle');
  }

  private accept(t: SpectrumTrace): void {
    const now = performance.now();
    const dt = this.lastFrameAt ? now - this.lastFrameAt : 0;
    this.lastFrameAt = now;
    const s = this.stats();
    const ups = dt > 0 ? 0.8 * s.updatesPerSec + 0.2 * (1000 / dt) : s.updatesPerSec;
    this.stats.set({ frames: s.frames + 1, bytes: s.bytes + t.wireBytes, updatesPerSec: ups });
    this.trace.set(t);
    this.monitor.noteActivity();
  }

  private open(): void {
    this.ws?.close();
    this.sub?.unsubscribe();
    const f = this.caps.caps()?.features ?? {};
    if (f.wsSpectrum && typeof WebSocket !== 'undefined') this.openWs();
    else if (f.spectrumBinary) this.pollBinary();
    else this.pollLegacy();
  }

  private subscribe(ws: WebSocket): void {
    ws.send(JSON.stringify({ type: 'sub', nodeId: this.sel.nodeId, port: this.sel.port, maxPoints: this.sel.maxPoints }));
  }

  private openWs(): void {
    const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/api/ws/spectrum');
    ws.binaryType = 'arraybuffer';
    this.ws = ws;
    let opened = false;
    ws.onopen = () => { opened = true; this.transport.set('websocket'); this.subscribe(ws); };
    ws.onmessage = (ev: MessageEvent<ArrayBuffer | string>) => {
      if (typeof ev.data === 'string') { this.monitor.noteActivity(); return; } // hello / subscribed / hb
      const t = decodeFrame(ev.data);
      if (t.nodeId === this.sel.nodeId && t.port === this.sel.port) this.accept(t);
    };
    ws.onclose = () => {
      if (this.ws !== ws || !this.active) return;
      this.ws = undefined;
      if (!opened) { this.pollBinary(); return; } // WebSocket blocked: fall back to HTTP
      setTimeout(() => { if (this.active) this.openWs(); }, 1000);
    };
  }

  private pollBinary(): void {
    this.transport.set('http-binary');
    let lastSweep = -1;
    this.sub = defer(() => this.http.get(
      `/api/spectrum/latest?nodeId=${this.sel.nodeId}&port=${this.sel.port}&maxPoints=${this.sel.maxPoints}&waitMs=5000`,
      { headers: { Accept: DSPC_CONTENT_TYPE }, responseType: 'arraybuffer', observe: 'response' }
    )).pipe(
      repeat(),
      retry({ delay: (_e, n) => timer(Math.min(10000, 500 * 2 ** Math.min(n, 5))) })
    ).subscribe((res: HttpResponse<ArrayBuffer>) => {
      if (!res.body) return;
      const t = decodeFrame(res.body);
      if (t.sweepId !== lastSweep) { lastSweep = t.sweepId; this.accept(t); } // 304 -> cached copy of the same sweep
    });
  }

  private pollLegacy(): void {
    this.transport.set('http-legacy');
    this.sub = defer(() => this.http.get<LegacySpectrum>(`/api/spectrum?nodeId=${this.sel.nodeId}&port=${this.sel.port}`, { observe: 'response' })).pipe(
      repeat({ delay: 200 }),
      retry({ delay: (_e, n) => timer(Math.min(10000, 500 * 2 ** Math.min(n, 5))) })
    ).subscribe((res) => {
      const j = res.body;
      if (!j || !j.points.length) return;
      const n = j.points.length;
      const power = new Float32Array(n);
      for (let i = 0; i < n; i++) power[i] = j.points[i].power;
      const stepHz = n > 1 ? (j.points[n - 1].frequency - j.points[0].frequency) / (n - 1) : 0;
      const approxBytes = Number(res.headers.get('Content-Length')) || n * 40;
      this.accept(peakDecimate({ sweepId: j.sweepId, nodeId: j.nodeId, port: j.port, startHz: j.points[0].frequency, stepHz, timestampMs: j.timestamp, decimated: false, power, wireBytes: approxBytes }, this.sel.maxPoints));
    });
  }
}
