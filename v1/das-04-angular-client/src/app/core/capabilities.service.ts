import { HttpClient, HttpErrorResponse } from '@angular/common/http';
import { Injectable, inject, signal } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Capabilities } from './api.types';

/**
 * Progressive enhancement: ask the backend what it supports instead of
 * assuming. A 404 means the shipped (legacy) backend, which gets plain polling.
 */
@Injectable({ providedIn: 'root' })
export class CapabilitiesService {
  private readonly http = inject(HttpClient);
  private pending?: Promise<Capabilities | null>;

  readonly caps = signal<Capabilities | null>(null);
  readonly legacy = signal(false);

  load(): Promise<Capabilities | null> {
    if (!this.pending) {
      this.pending = firstValueFrom(this.http.get<Capabilities>('/api/capabilities')).then(
        (c) => {
          this.caps.set(c);
          return c;
        },
        (err: unknown) => {
          if (err instanceof HttpErrorResponse && err.status === 404) this.legacy.set(true);
          else this.pending = undefined; // transient error: try again next time
          return null;
        }
      );
    }
    return this.pending;
  }
}
