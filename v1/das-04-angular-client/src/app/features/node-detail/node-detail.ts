import { ChangeDetectionStrategy, Component, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { RouterLink } from '@angular/router';
import { NodeConfig } from '../../core/api.types';
import { ConfigApi } from '../../core/config-api.service';
import { TelemetryFeed } from '../../core/telemetry-feed.service';
import { StatusBadge, nodeStatusKind } from '../../shared/status-badge';

type Notice = { kind: 'ok' | 'conflict' | 'error'; text: string; errors?: string[] } | null;

@Component({
  selector: 'das-node-detail',
  imports: [RouterLink, StatusBadge],
  templateUrl: './node-detail.html',
  styleUrl: './node-detail.css',
  changeDetection: ChangeDetectionStrategy.OnPush
})
export class NodeDetail {
  private readonly feed = inject(TelemetryFeed);
  private readonly api = inject(ConfigApi);

  /** Route parameter (withComponentInputBinding). */
  readonly id = input.required<string>();
  protected readonly nodeId = computed(() => Number(this.id()));
  protected readonly node = computed(() => this.feed.state().nodes.get(this.nodeId()) ?? null);
  protected readonly statusKind = computed(() => nodeStatusKind(this.node()?.status));

  protected readonly form = signal<NodeConfig | null>(null);
  protected readonly version = signal(0);
  protected readonly notice = signal<Notice>(null);
  protected readonly saving = signal(false);
  private etag: string | null = null;

  constructor() {
    effect(() => {
      const id = this.nodeId();
      untracked(() => void this.reload(id));
    });
  }

  protected async reload(id = this.nodeId()): Promise<void> {
    const { envelope, etag } = await this.api.load(id);
    this.etag = etag;
    this.version.set(envelope.version);
    this.form.set(structuredClone(envelope.config));
  }

  protected patch<K extends keyof NodeConfig>(key: K, value: NodeConfig[K]): void {
    const f = this.form();
    if (f) this.form.set({ ...f, [key]: value });
  }

  protected toggleBand(name: string, on: boolean): void {
    const f = this.form();
    if (f) this.form.set({ ...f, bandsEnabled: { ...f.bandsEnabled, [name]: on } });
  }

  protected bandNames(): string[] { return Object.keys(this.form()?.bandsEnabled ?? {}); }

  protected async save(): Promise<void> {
    const f = this.form();
    if (!f) return;
    this.saving.set(true);
    const r = await this.api.save(this.nodeId(), f, this.etag);
    this.saving.set(false);
    if (r.kind === 'saved') {
      this.etag = r.etag;
      this.version.set(r.envelope.version);
      this.notice.set({ kind: 'ok', text: `Saved: version ${r.envelope.version}.` });
    } else if (r.kind === 'conflict') {
      this.notice.set({ kind: 'conflict', text: `Not saved: someone else changed this node (now version ${r.current?.version ?? '?'}). Reload to see their change, then re-apply yours.` });
    } else if (r.kind === 'invalid') {
      this.notice.set({ kind: 'error', text: 'Not saved: the configuration is invalid.', errors: r.errors });
    } else {
      this.notice.set({ kind: 'error', text: `Not saved: ${r.message}` });
    }
  }

  /** Demo: a colleague saves a change on the same node without our knowledge. */
  protected async simulateColleague(): Promise<void> {
    const { envelope, etag } = await this.api.load(this.nodeId());
    const cfg = { ...envelope.config, notes: `Changed by a colleague at ${new Date().toLocaleTimeString()}` };
    await this.api.save(this.nodeId(), cfg, etag);
    this.notice.set({ kind: 'ok', text: 'A colleague just saved a change. Now try saving yours.' });
  }

  protected fmt(v: number | undefined, d = 1): string { return v == null ? '–' : v.toFixed(d); }
}
