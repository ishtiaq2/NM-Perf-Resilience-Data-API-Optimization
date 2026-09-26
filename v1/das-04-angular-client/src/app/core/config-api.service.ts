import { HttpClient, HttpErrorResponse } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { NodeConfig, NodeConfigEnvelope } from './api.types';

export type SaveResult =
  | { kind: 'saved'; envelope: NodeConfigEnvelope; etag: string | null }
  | { kind: 'conflict'; current: NodeConfigEnvelope | null; message: string }
  | { kind: 'invalid'; errors: string[] }
  | { kind: 'error'; message: string };

/**
 * Remote Node configuration with optimistic concurrency: the write carries the
 * ETag of the version the engineer edited (If-Match). If a colleague changed the
 * node in the meantime the server answers 412, so nothing is silently overwritten.
 */
@Injectable({ providedIn: 'root' })
export class ConfigApi {
  private readonly http = inject(HttpClient);

  async load(nodeId: number): Promise<{ envelope: NodeConfigEnvelope; etag: string | null }> {
    const res = await firstValueFrom(this.http.get<NodeConfigEnvelope>(`/api/nodes/${nodeId}/config`, { observe: 'response' }));
    return { envelope: res.body as NodeConfigEnvelope, etag: res.headers.get('ETag') };
  }

  async save(nodeId: number, config: NodeConfig, etag: string | null): Promise<SaveResult> {
    try {
      const headers: Record<string, string> = etag ? { 'If-Match': etag } : {};
      const res = await firstValueFrom(this.http.put<NodeConfigEnvelope>(`/api/nodes/${nodeId}/config`, config, { headers, observe: 'response' }));
      return { kind: 'saved', envelope: res.body as NodeConfigEnvelope, etag: res.headers.get('ETag') };
    } catch (e) {
      if (e instanceof HttpErrorResponse) {
        if (e.status === 412) return { kind: 'conflict', current: e.error?.current ?? null, message: e.error?.message ?? 'Changed by someone else' };
        if (e.status === 422) return { kind: 'invalid', errors: e.error?.errors ?? ['Invalid configuration'] };
        return { kind: 'error', message: `${e.status} ${e.statusText}` };
      }
      return { kind: 'error', message: String(e) };
    }
  }
}
