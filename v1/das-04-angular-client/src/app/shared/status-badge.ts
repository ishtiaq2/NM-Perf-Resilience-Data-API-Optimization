import { ChangeDetectionStrategy, Component, input } from '@angular/core';

export type StatusKind = 'good' | 'warning' | 'serious' | 'critical' | 'neutral';

/** Status = icon + label, never color alone. */
@Component({
  selector: 'das-status',
  changeDetection: ChangeDetectionStrategy.OnPush,
  template: `
    <span class="status" [attr.title]="title() || null">
      @switch (kind()) {
        @case ('good') {
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="var(--good)"/><path d="M4.6 8.3l2.2 2.2 4.6-4.8" stroke="#fff" stroke-width="1.8" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>
        }
        @case ('warning') {
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.5l7 12.5H1z" fill="var(--warning)"/><path d="M8 6v4" stroke="#1a1a19" stroke-width="1.7" stroke-linecap="round"/><circle cx="8" cy="12" r="0.95" fill="#1a1a19"/></svg>
        }
        @case ('serious') {
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><rect x="2" y="2" width="12" height="12" rx="2" fill="var(--serious)"/><path d="M8 4.8v4.2" stroke="#fff" stroke-width="1.7" stroke-linecap="round"/><circle cx="8" cy="11.3" r="0.95" fill="#fff"/></svg>
        }
        @case ('critical') {
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="7" fill="var(--critical)"/><path d="M5.3 5.3l5.4 5.4M10.7 5.3l-5.4 5.4" stroke="#fff" stroke-width="1.8" stroke-linecap="round"/></svg>
        }
        @default {
          <svg width="14" height="14" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="var(--muted)" stroke-width="2"/></svg>
        }
      }
      <span>{{ label() }}</span>
    </span>
  `
})
export class StatusBadge {
  readonly kind = input<StatusKind>('neutral');
  readonly label = input('');
  readonly title = input('');
}

export function nodeStatusKind(s: string | undefined): StatusKind {
  return s === 'online' ? 'good' : s === 'degraded' ? 'warning' : s === 'offline' ? 'critical' : 'neutral';
}
