import { ChangeDetectionStrategy, Component, ElementRef, OnDestroy, afterNextRender, effect, input, output, signal, viewChild } from '@angular/core';
import { SpectrumTrace } from '../../core/spectrum-frame';

interface Hover { x: number; freqHz: number; dbm: number; holdDbm: number | null; left: number; top: number }

const PAD = { l: 52, r: 14, t: 12, b: 28 };

function niceStep(span: number, target: number): number {
  const raw = span / target;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  return [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? raw;
}

/**
 * Canvas renderer for spectrum traces. A spectrum has thousands of points and
 * updates several times per second: canvas (not SVG/DOM) keeps that cheap, and
 * drawing is coalesced to at most one frame per display refresh.
 */
@Component({
  selector: 'das-spectrum-canvas',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <div class="wrap">
      <canvas #cv (pointermove)="onMove($event)" (pointerleave)="hover.set(null)" role="img" aria-label="Spectrum trace: power in dBm over frequency"></canvas>
      @if (hover(); as h) {
        <div class="tip" [style.left.px]="h.left" [style.top.px]="h.top">
          <b class="num">{{ h.dbm.toFixed(2) }} dBm</b>
          <div><span class="key k1"></span>trace @ <span class="num">{{ (h.freqHz / 1e6).toFixed(3) }}</span> MHz</div>
          @if (h.holdDbm !== null) { <div><span class="key k2"></span>max hold <span class="num">{{ h.holdDbm.toFixed(2) }}</span> dBm</div> }
        </div>
      }
    </div>
  `,
  styles: [`
    :host { display: block; }
    .wrap { position: relative; }
    canvas { display: block; width: 100%; height: 360px; border-radius: 8px; background: var(--plot-bg); touch-action: none; }
    .tip { position: absolute; pointer-events: none; background: var(--surface); border: 1px solid var(--line); border-radius: 8px; padding: 6px 10px; font-size: 12px; box-shadow: var(--shadow); white-space: nowrap; }
    .tip b { font-size: 14px; }
    .key { display: inline-block; width: 14px; height: 2px; vertical-align: middle; margin-right: 6px; }
    .k1 { background: var(--series-1); } .k2 { background: var(--series-2); }
  `]
})
export class SpectrumCanvas implements OnDestroy {
  readonly trace = input<SpectrumTrace | null>(null);
  readonly hold = input<Float32Array | null>(null);
  readonly refLevel = input(-20);
  readonly rangeDb = input(90);
  /** Plot width in device pixels: the server decimates to this many points. */
  readonly widthPx = output<number>();

  private readonly canvas = viewChild.required<ElementRef<HTMLCanvasElement>>('cv');
  protected readonly hover = signal<Hover | null>(null);
  private raf = 0;
  private ro?: ResizeObserver;
  private hoverX: number | null = null;

  constructor() {
    effect(() => {
      // Track inputs; draw once per animation frame with the latest data.
      this.trace(); this.hold(); this.refLevel(); this.rangeDb();
      this.schedule();
    });
    afterNextRender(() => {
      const cv = this.canvas().nativeElement;
      this.ro = new ResizeObserver(() => {
        const dpr = window.devicePixelRatio || 1;
        cv.width = Math.round(cv.clientWidth * dpr);
        cv.height = Math.round(cv.clientHeight * dpr);
        this.widthPx.emit(Math.max(64, Math.round((cv.clientWidth - PAD.l - PAD.r) * dpr)));
        this.schedule();
      });
      this.ro.observe(cv);
    });
  }

  ngOnDestroy(): void { this.ro?.disconnect(); cancelAnimationFrame(this.raf); }

  private schedule(): void {
    if (this.raf) return;
    this.raf = requestAnimationFrame(() => { this.raf = 0; this.draw(); });
  }

  protected onMove(ev: PointerEvent): void {
    const t = this.trace();
    const cv = this.canvas().nativeElement;
    const rect = cv.getBoundingClientRect();
    const x = ev.clientX - rect.left;
    const plotW = rect.width - PAD.l - PAD.r;
    if (!t || x < PAD.l || x > PAD.l + plotW || !t.power.length) { this.hover.set(null); this.hoverX = null; this.schedule(); return; }
    const i = Math.min(t.power.length - 1, Math.max(0, Math.round(((x - PAD.l) / plotW) * (t.power.length - 1))));
    const hold = this.hold();
    this.hoverX = x;
    const left = x + 170 > rect.width ? x - 180 : x + 12;
    this.hover.set({ x, freqHz: t.startHz + i * t.stepHz, dbm: t.power[i], holdDbm: hold && hold.length === t.power.length ? hold[i] : null, left, top: 14 });
    this.schedule();
  }

  private draw(): void {
    const cv = this.canvas().nativeElement;
    const ctx = cv.getContext('2d');
    if (!ctx || !cv.width) return;
    const css = getComputedStyle(cv);
    const color = (name: string) => css.getPropertyValue(name).trim();
    const dpr = window.devicePixelRatio || 1;
    const W = cv.width / dpr, H = cv.height / dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    const pw = W - PAD.l - PAD.r, ph = H - PAD.t - PAD.b;
    const top = this.refLevel(), bottom = top - this.rangeDb();
    const y = (dbm: number) => PAD.t + ((top - dbm) / (top - bottom)) * ph;

    // Grid + axes (hairlines, recessive)
    ctx.lineWidth = 1;
    ctx.strokeStyle = color('--plot-grid');
    ctx.fillStyle = color('--plot-label');
    ctx.font = '11px ' + color('--font');
    ctx.textAlign = 'right';
    ctx.textBaseline = 'middle';
    for (let v = top; v >= bottom; v -= 10) {
      const yy = Math.round(y(v)) + 0.5;
      ctx.beginPath(); ctx.moveTo(PAD.l, yy); ctx.lineTo(PAD.l + pw, yy); ctx.stroke();
      ctx.fillText(String(v), PAD.l - 8, yy);
    }
    const t = this.trace();
    if (t && t.power.length > 1) {
      const f0 = t.startHz, f1 = t.startHz + (t.power.length - 1) * t.stepHz;
      const step = niceStep((f1 - f0) / 1e6, 8) * 1e6;
      ctx.textAlign = 'center';
      ctx.textBaseline = 'top';
      for (let f = Math.ceil(f0 / step) * step; f <= f1; f += step) {
        const xx = Math.round(PAD.l + ((f - f0) / (f1 - f0)) * pw) + 0.5;
        ctx.beginPath(); ctx.moveTo(xx, PAD.t); ctx.lineTo(xx, PAD.t + ph); ctx.stroke();
        ctx.fillText(`${Math.round(f / 1e6)} MHz`, xx, PAD.t + ph + 8);
      }
      const x = (i: number) => PAD.l + (i / (t.power.length - 1)) * pw;
      ctx.save();
      ctx.beginPath(); ctx.rect(PAD.l, PAD.t, pw, ph); ctx.clip();
      const hold = this.hold();
      if (hold && hold.length === t.power.length) this.path(ctx, hold, x, y, color('--series-2'));
      this.path(ctx, t.power, x, y, color('--series-1'));
      if (this.hoverX !== null) {
        ctx.strokeStyle = color('--plot-axis');
        ctx.beginPath(); ctx.moveTo(Math.round(this.hoverX) + 0.5, PAD.t); ctx.lineTo(Math.round(this.hoverX) + 0.5, PAD.t + ph); ctx.stroke();
      }
      ctx.restore();
    }
    ctx.strokeStyle = color('--plot-axis');
    ctx.beginPath(); ctx.moveTo(PAD.l, PAD.t + ph + 0.5); ctx.lineTo(PAD.l + pw, PAD.t + ph + 0.5); ctx.stroke();
  }

  private path(ctx: CanvasRenderingContext2D, p: Float32Array, x: (i: number) => number, y: (v: number) => number, stroke: string): void {
    ctx.strokeStyle = stroke;
    ctx.lineWidth = 1.5;
    ctx.lineJoin = 'round';
    ctx.beginPath();
    let pen = false;
    for (let i = 0; i < p.length; i++) {
      const v = p[i];
      if (v !== v) { pen = false; continue; } // NaN gap
      if (pen) ctx.lineTo(x(i), y(v)); else { ctx.moveTo(x(i), y(v)); pen = true; }
    }
    ctx.stroke();
  }
}
