/**
 * Every successful response proves the server is alive. Feeding that into the
 * ConnectionMonitorService means a slow heartbeat alone can no longer raise
 * "server is dead" while other data is still arriving.
 *
 * Register (NgModule apps):
 *   providers: [{ provide: HTTP_INTERCEPTORS, useClass: LivenessInterceptor, multi: true }]
 * Standalone apps:
 *   provideHttpClient(withInterceptorsFromDi()) + the same provider.
 *
 * Angular 14+.
 */
import { Injectable } from '@angular/core';
import { HttpEvent, HttpHandler, HttpInterceptor, HttpRequest, HttpResponse } from '@angular/common/http';
import { Observable } from 'rxjs';
import { tap } from 'rxjs/operators';
import { ConnectionMonitorService, SKIP_LIVENESS } from './connection-monitor.service';

@Injectable()
export class LivenessInterceptor implements HttpInterceptor {
  constructor(private readonly monitor: ConnectionMonitorService) {}

  intercept(req: HttpRequest<unknown>, next: HttpHandler): Observable<HttpEvent<unknown>> {
    if (req.context.get(SKIP_LIVENESS)) return next.handle(req);
    return next.handle(req).pipe(
      tap({
        next: (ev) => { if (ev instanceof HttpResponse) this.monitor.noteActivity(); },
        // 304 and 4xx are answers too: the server is alive.
        error: (err) => { if (err && typeof err.status === 'number' && err.status > 0 && err.status < 500) this.monitor.noteActivity(); }
      })
    );
  }
}
