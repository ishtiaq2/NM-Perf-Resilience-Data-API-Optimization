import { ApplicationConfig, provideBrowserGlobalErrorListeners, provideZonelessChangeDetection } from '@angular/core';
import { HTTP_INTERCEPTORS, provideHttpClient, withFetch, withInterceptorsFromDi } from '@angular/common/http';
import { provideRouter, withComponentInputBinding } from '@angular/router';
import { routes } from './app.routes';
import { EtagCacheInterceptor } from './core/connection/etag-cache.interceptor';
import { LivenessInterceptor } from './core/connection/liveness.interceptor';
import { CONNECTION_MONITOR_CONFIG } from './core/connection/connection-monitor.service';

export const appConfig: ApplicationConfig = {
  providers: [
    provideBrowserGlobalErrorListeners(),
    // Zoneless: signals drive change detection, so 1-10 data updates per second
    // re-render only the components (and table rows) whose data changed.
    provideZonelessChangeDetection(),
    provideRouter(routes, withComponentInputBinding()),
    provideHttpClient(withFetch(), withInterceptorsFromDi()),
    // The same interceptors ship as the optional frontend patch of the hotfix (das-01).
    { provide: HTTP_INTERCEPTORS, useClass: EtagCacheInterceptor, multi: true },
    { provide: HTTP_INTERCEPTORS, useClass: LivenessInterceptor, multi: true },
    { provide: CONNECTION_MONITOR_CONFIG, useValue: { intervalMs: 2000, timeoutMs: 5000, offlineAfterMisses: 3, offlineAfterSilenceMs: 15000 } }
  ]
};
