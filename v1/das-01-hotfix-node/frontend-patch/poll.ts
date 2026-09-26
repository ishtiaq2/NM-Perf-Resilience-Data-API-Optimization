/**
 * Polling helpers that never pile up requests on the device.
 *
 *  - pollSequential: the next request starts `intervalMs` after the previous
 *    one COMPLETED (never overlapping, unlike interval() + switchMap/mergeMap).
 *  - Pauses while the browser tab is hidden (an engineer with 5 tabs open no
 *    longer costs the device 5x).
 *  - Errors back off exponentially instead of hammering a busy server.
 *
 * Usage:
 *   this.data$ = pollSequential(() => this.http.get<VolatileData>('/api/volatile-data'), 2000);
 *
 * RxJS 7.4+ (Angular 13+).
 */
import { EMPTY, Observable, defer, fromEvent, of, timer } from 'rxjs';
import { distinctUntilChanged, map, repeat, retry, startWith, switchMap } from 'rxjs/operators';

export interface PollOptions {
  pauseWhenHidden?: boolean;
  maxBackoffMs?: number;
}

function visibility$(enabled: boolean): Observable<boolean> {
  if (!enabled || typeof document === 'undefined') return of(true);
  return fromEvent(document, 'visibilitychange').pipe(
    startWith(null),
    map(() => document.visibilityState !== 'hidden'),
    distinctUntilChanged()
  );
}

export function pollSequential<T>(request: () => Observable<T>, intervalMs: number, opts: PollOptions = {}): Observable<T> {
  const maxBackoff = opts.maxBackoffMs ?? 30000;
  return visibility$(opts.pauseWhenHidden !== false).pipe(
    switchMap((visible) =>
      visible
        ? defer(request).pipe(
            repeat({ delay: intervalMs }),
            retry({ delay: (_err, attempt) => timer(Math.min(maxBackoff, 1000 * 2 ** Math.min(attempt, 5))) })
          )
        : EMPTY
    )
  );
}
