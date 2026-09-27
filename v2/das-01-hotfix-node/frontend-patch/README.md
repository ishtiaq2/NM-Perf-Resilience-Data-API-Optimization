# Optional frontend patch (Angular 14+)

The backend hotfix works **without** any frontend change. These four files are for the
next frontend release and remove the remaining failure modes on the client side.

| File | What it fixes |
|---|---|
| `connection-monitor.service.ts` | Replaces "one timed-out heartbeat = server is dead" with `online / degraded / offline` and hysteresis. Declares `offline` only after 3 consecutive misses **and** 15 s without any successful response. |
| `liveness.interceptor.ts` | Any successful API response counts as proof of life, so a busy but alive server is never declared dead. |
| `etag-cache.interceptor.ts` | Sends `If-None-Match` for polled endpoints and turns `304` into the cached `200`. It works even where the browser does not cache, for example over HTTPS with the device's self-signed certificate. |
| `poll.ts` | `pollSequential()`: requests never overlap, polling pauses in hidden tabs, and errors back off exponentially. |

## Wiring (NgModule app)

```ts
import { HTTP_INTERCEPTORS } from '@angular/common/http';
import { LivenessInterceptor } from './connection/liveness.interceptor';
import { EtagCacheInterceptor } from './connection/etag-cache.interceptor';
import { CONNECTION_MONITOR_CONFIG } from './connection/connection-monitor.service';

providers: [
  { provide: HTTP_INTERCEPTORS, useClass: EtagCacheInterceptor, multi: true },
  { provide: HTTP_INTERCEPTORS, useClass: LivenessInterceptor, multi: true },
  { provide: CONNECTION_MONITOR_CONFIG, useValue: { intervalMs: 2000, timeoutMs: 5000 } }
]
```

Standalone apps use `provideHttpClient(withInterceptorsFromDi())` with the same providers.

```ts
// app.component.ts
constructor(public conn: ConnectionMonitorService) { conn.start(); }
```
```html
<div class="banner offline" *ngIf="(conn.state$ | async) === 'offline'">Connection to the Master Node lost - retrying…</div>
<div class="banner degraded" *ngIf="(conn.state$ | async) === 'degraded'">Master Node is busy - data may be delayed</div>
```

```ts
// dashboard.component.ts - replaces interval(2000).pipe(switchMap(() => http.get(...)))
this.volatile$ = pollSequential(() => this.http.get<VolatileData>('/api/volatile-data'), 2000);
```

These files are the same code the Angular reference client (`das-04-angular-client`) uses.
It compiles them in its build, so they are type-checked against a current Angular version.
