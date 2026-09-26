import { ChangeDetectionStrategy, Component, computed, inject, signal } from '@angular/core';
import { toSignal } from '@angular/core/rxjs-interop';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { ConnectionMonitorService } from './core/connection/connection-monitor.service';
import { TelemetryFeed } from './core/telemetry-feed.service';
import { CapabilitiesService } from './core/capabilities.service';
import { SpectrumFeed } from './core/spectrum-feed.service';
import { StatusBadge, StatusKind } from './shared/status-badge';

@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive, StatusBadge],
  templateUrl: './app.html',
  styleUrl: './app.css',
  changeDetection: ChangeDetectionStrategy.OnPush
})
export class App {
  private readonly monitor = inject(ConnectionMonitorService);
  protected readonly telemetry = inject(TelemetryFeed);
  protected readonly spectrum = inject(SpectrumFeed);
  protected readonly caps = inject(CapabilitiesService);

  protected readonly conn = toSignal(this.monitor.state$, { initialValue: 'online' as const });
  protected readonly heartbeat = toSignal(this.monitor.lastHeartbeat$, { initialValue: null });
  protected readonly now = signal(Date.now());

  protected readonly connKind = computed<StatusKind>(() => ({ online: 'good', degraded: 'warning', offline: 'critical' } as const)[this.conn()]);
  protected readonly connLabel = computed(() => ({ online: 'Connected', degraded: 'Busy', offline: 'Disconnected' })[this.conn()]);
  protected readonly server = computed(() => this.heartbeat()?.server ?? '');
  protected readonly unhealthy = computed(() => {
    const s = this.heartbeat()?.services ?? {};
    return Object.entries(s).filter(([, v]) => v !== 'ok').map(([k, v]) => `${k} ${v}`);
  });
  protected readonly dataAge = computed(() => {
    const t = this.telemetry.lastUpdateAt();
    return t ? Math.max(0, Math.round((this.now() - t) / 1000)) : null;
  });
  protected readonly transportLabel = computed(() => ({
    connecting: 'connecting…', websocket: 'WebSocket push', 'http-delta': 'HTTP deltas (ETag)', 'http-poll': 'HTTP polling (legacy backend)'
  })[this.telemetry.transport()]);

  constructor() {
    this.monitor.start();
    this.telemetry.start();
    setInterval(() => this.now.set(Date.now()), 1000);
  }
}
