import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { Router } from '@angular/router';
import { NodeTelemetry } from '../../core/api.types';
import { TelemetryFeed } from '../../core/telemetry-feed.service';
import { StatusBadge, StatusKind, nodeStatusKind } from '../../shared/status-badge';

interface Row {
  id: number;
  name: string;
  status: string;
  statusKind: StatusKind;
  chainHop: string;
  temp: number | null;
  dlAvg: number | null;
  ulAvg: number | null;
  rx: number | null;
  vswrMax: number | null;
  alarms: number;
  alarmKind: StatusKind;
  alarmText: string;
  uptime: string;
}

const SEVERITY_RANK: Record<string, number> = { minor: 1, major: 2, critical: 3 };

function avg(xs: number[]): number | null { return xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : null; }

function uptime(n: NodeTelemetry, now: number): string {
  const s = n.bootAt ? (now - n.bootAt) / 1000 : n.uptimeS;
  if (s == null || !isFinite(s)) return '–';
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

function toRow(n: NodeTelemetry, now: number): Row {
  const bands = (n.bands ?? []).filter((b) => b.enabled);
  const alarms = n.alarms ?? [];
  const worst = alarms.reduce((w, a) => Math.max(w, SEVERITY_RANK[a.severity] ?? 0), 0);
  return {
    id: n.id,
    name: n.name,
    status: n.status,
    statusKind: nodeStatusKind(n.status),
    chainHop: n.chain != null ? `${n.chain}·${n.hop}` : '–',
    temp: n.temperatureC ?? null,
    dlAvg: avg(bands.map((b) => b.dlOutDbm)),
    ulAvg: avg(bands.map((b) => b.ulInDbm)),
    rx: n.optical?.rxDbm ?? null,
    vswrMax: bands.length ? Math.max(...bands.map((b) => b.vswr)) : null,
    alarms: alarms.length,
    alarmKind: worst === 3 ? 'critical' : worst === 2 ? 'serious' : worst === 1 ? 'warning' : 'neutral',
    alarmText: alarms.map((a) => a.code).join(', '),
    uptime: uptime(n, now)
  };
}

@Component({
  selector: 'das-dashboard',
  imports: [StatusBadge],
  templateUrl: './dashboard.html',
  styleUrl: './dashboard.css',
  changeDetection: ChangeDetectionStrategy.OnPush
})
export class Dashboard {
  private readonly feed = inject(TelemetryFeed);
  private readonly router = inject(Router);

  protected readonly query = signal('');
  protected readonly statusFilter = signal<'all' | 'online' | 'degraded' | 'offline'>('all');
  protected readonly alarmsOnly = signal(false);
  protected readonly changed = computed(() => this.feed.state().changed);
  protected readonly loading = computed(() => this.feed.state().rev < 0);

  private readonly allRows = computed(() => {
    const now = Date.now();
    const rows: Row[] = [];
    for (const n of this.feed.state().nodes.values()) rows.push(toRow(n, now));
    return rows.sort((a, b) => a.id - b.id);
  });

  protected readonly rows = computed(() => {
    const q = this.query().trim().toLowerCase();
    const st = this.statusFilter();
    const al = this.alarmsOnly();
    return this.allRows().filter((r) => (!q || r.name.toLowerCase().includes(q)) && (st === 'all' || r.status === st) && (!al || r.alarms > 0));
  });

  protected readonly summary = computed(() => {
    const rows = this.allRows();
    const by = (s: string) => rows.filter((r) => r.status === s).length;
    return {
      total: rows.length,
      online: by('online'),
      degraded: by('degraded'),
      offline: by('offline'),
      alarms: rows.reduce((s, r) => s + r.alarms, 0)
    };
  });

  protected open(id: number): void { void this.router.navigate(['/nodes', id]); }
  protected fmt(v: number | null, digits = 1): string { return v == null ? '–' : v.toFixed(digits); }
}
