import { TestBed } from '@angular/core/testing';
import { provideZonelessChangeDetection } from '@angular/core';
import { provideHttpClient } from '@angular/common/http';
import { HttpTestingController, provideHttpClientTesting } from '@angular/common/http/testing';
import { CONNECTION_MONITOR_CONFIG, ConnectionMonitorService, ConnectionState } from './connection-monitor.service';

describe('ConnectionMonitorService (tolerant heartbeat)', () => {
  let http: HttpTestingController;
  let monitor: ConnectionMonitorService;
  const states: ConnectionState[] = [];

  beforeEach(() => {
    vi.useFakeTimers();
    TestBed.configureTestingModule({
      providers: [
        provideZonelessChangeDetection(),
        provideHttpClient(),
        provideHttpClientTesting(),
        { provide: CONNECTION_MONITOR_CONFIG, useValue: { intervalMs: 1000, timeoutMs: 500, offlineAfterMisses: 3, offlineAfterSilenceMs: 3000, degradedLatencyMs: 400 } }
      ]
    });
    http = TestBed.inject(HttpTestingController);
    monitor = TestBed.inject(ConnectionMonitorService);
    states.length = 0;
    monitor.state$.subscribe((s) => states.push(s));
    monitor.start();
    vi.advanceTimersByTime(0); // first probe fires immediately (timer(0, interval))
  });

  afterEach(() => { monitor.ngOnDestroy(); vi.useRealTimers(); });

  function tick(ms: number): void { vi.advanceTimersByTime(ms); }

  it('stays online while heartbeats are answered', () => {
    http.expectOne('/api/heartbeat').flush({ status: 'ok', server: 'test' });
    tick(1000);
    http.expectOne('/api/heartbeat').flush({ status: 'ok' });
    expect(monitor.state).toBe('online');
    expect(states).toEqual(['online']);
  });

  it('one missed heartbeat is "degraded", never "offline"', () => {
    http.expectOne('/api/heartbeat'); // never answered
    tick(600); // timeout
    expect(monitor.state).toBe('degraded');
    tick(400);
    http.expectOne('/api/heartbeat').flush({ status: 'ok' });
    expect(monitor.state).toBe('online');
  });

  it('declares offline only after 3 misses AND a silent period', () => {
    for (let i = 0; i < 4; i++) { http.match('/api/heartbeat'); tick(1000); }
    http.match('/api/heartbeat');
    expect(monitor.state).toBe('offline');
  });

  it('any other successful response keeps the server alive', () => {
    for (let i = 0; i < 4; i++) {
      http.match('/api/heartbeat');
      monitor.noteActivity(); // e.g. telemetry arrived over WebSocket
      tick(1000);
    }
    expect(monitor.state).not.toBe('offline');
  });

  it('a server that reports "degraded" is shown as busy, not dead', () => {
    http.expectOne('/api/heartbeat').flush({ status: 'degraded', services: { spectrum: 'down' } });
    expect(monitor.state).toBe('degraded');
    expect(monitor.lastHeartbeat$.value?.services?.['spectrum']).toBe('down');
  });
});
