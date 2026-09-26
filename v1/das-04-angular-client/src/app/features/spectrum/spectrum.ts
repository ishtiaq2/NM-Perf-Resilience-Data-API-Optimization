import { ChangeDetectionStrategy, Component, OnDestroy, OnInit, computed, effect, inject, signal, untracked } from '@angular/core';
import { SpectrumFeed } from '../../core/spectrum-feed.service';
import { SpectrumTrace } from '../../core/spectrum-frame';
import { SpectrumCanvas } from './spectrum-canvas';

interface Peak { freqMhz: number; dbm: number }

/** Top-N local maxima, at least `minSep` bins apart (the table view of the chart). */
export function findPeaks(t: SpectrumTrace, n = 5, minSepFraction = 0.01): Peak[] {
  const p = t.power;
  const idx: number[] = [];
  for (let i = 1; i < p.length - 1; i++) if (p[i] >= p[i - 1] && p[i] >= p[i + 1] && p[i] === p[i]) idx.push(i);
  idx.sort((a, b) => p[b] - p[a]);
  const minSep = Math.max(1, Math.round(p.length * minSepFraction));
  const out: number[] = [];
  for (const i of idx) {
    if (out.every((j) => Math.abs(j - i) >= minSep)) out.push(i);
    if (out.length === n) break;
  }
  return out.map((i) => ({ freqMhz: (t.startHz + i * t.stepHz) / 1e6, dbm: p[i] }));
}

@Component({
  selector: 'das-spectrum',
  imports: [SpectrumCanvas],
  templateUrl: './spectrum.html',
  styleUrl: './spectrum.css',
  changeDetection: ChangeDetectionStrategy.OnPush
})
export class SpectrumPage implements OnInit, OnDestroy {
  protected readonly feed = inject(SpectrumFeed);
  protected readonly nodeId = signal(1);
  protected readonly port = signal(1);
  protected readonly refLevel = signal(-20);
  protected readonly maxHoldOn = signal(false);
  protected readonly paused = signal(false);
  private readonly maxPoints = signal(1600);
  private widthTimer?: ReturnType<typeof setTimeout>;

  /** Frozen copy while paused. */
  private readonly frozen = signal<SpectrumTrace | null>(null);
  protected readonly shown = computed(() => (this.paused() ? this.frozen() : this.feed.trace()));
  protected readonly hold = signal<Float32Array | null>(null);
  protected readonly peaks = computed(() => { const t = this.shown(); return t ? findPeaks(t) : []; });
  protected readonly kbPerUpdate = computed(() => { const t = this.shown(); return t ? t.wireBytes / 1024 : 0; });

  constructor() {
    // Max hold: running maximum per bin, reset when the trace geometry changes.
    effect(() => {
      const t = this.feed.trace();
      const on = this.maxHoldOn();
      untracked(() => {
        if (!on || !t) { this.hold.set(null); return; }
        const h = this.hold();
        if (!h || h.length !== t.power.length) { this.hold.set(Float32Array.from(t.power)); return; }
        const next = new Float32Array(h.length);
        for (let i = 0; i < h.length; i++) next[i] = t.power[i] > h[i] || h[i] !== h[i] ? t.power[i] : h[i];
        this.hold.set(next);
      });
    });
  }

  ngOnInit(): void { this.feed.start(this.selection()); }
  ngOnDestroy(): void { this.feed.stop(); clearTimeout(this.widthTimer); }

  private selection() { return { nodeId: this.nodeId(), port: this.port(), maxPoints: this.maxPoints() }; }

  protected setNode(v: string): void { const n = Math.max(1, Math.min(999, parseInt(v, 10) || 1)); this.nodeId.set(n); this.hold.set(null); this.feed.update(this.selection()); }
  protected setPort(v: string): void { this.port.set(parseInt(v, 10) || 1); this.hold.set(null); this.feed.update(this.selection()); }
  protected togglePause(): void { this.frozen.set(this.feed.trace()); this.paused.update((p) => !p); }
  protected onWidth(px: number): void {
    // The server decimates to the canvas width; debounce while the window is being resized.
    clearTimeout(this.widthTimer);
    this.widthTimer = setTimeout(() => {
      if (Math.abs(px - this.maxPoints()) < 32) return;
      this.maxPoints.set(px);
      this.feed.update(this.selection());
    }, 300);
  }
}
