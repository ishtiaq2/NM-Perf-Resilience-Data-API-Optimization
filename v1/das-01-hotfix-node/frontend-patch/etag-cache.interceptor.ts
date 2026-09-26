/**
 * Explicit ETag revalidation for polled GET endpoints.
 *
 * Browsers only revalidate automatically when the response lands in their HTTP
 * cache, and Chrome does not cache at all on HTTPS with an untrusted
 * (self-signed) certificate, which is common on devices. This interceptor stores
 * the last body + ETag per URL, sends If-None-Match, and turns a 304 into the
 * cached 200 response. Components see no difference, the device sends ~100
 * bytes instead of the full payload, and Angular skips re-parsing it.
 *
 * Only URLs matching ETAG_CACHE_URLS are handled (default: volatile-data and
 * the new spectrum endpoint).
 *
 * Angular 14+.
 */
import { Inject, Injectable, InjectionToken, Optional } from '@angular/core';
import { HttpErrorResponse, HttpEvent, HttpHandler, HttpInterceptor, HttpRequest, HttpResponse } from '@angular/common/http';
import { Observable, of, throwError } from 'rxjs';
import { catchError, tap } from 'rxjs/operators';

export const ETAG_CACHE_URLS = new InjectionToken<RegExp[]>('ETAG_CACHE_URLS');

const DEFAULT_URLS = [/\/api\/volatile-data(\?|$)/, /\/api\/spectrum\/latest(\?|$)/, /\/api\/nodes\/\d+\/config$/];
const MAX_ENTRIES = 32;

@Injectable()
export class EtagCacheInterceptor implements HttpInterceptor {
  private readonly cache = new Map<string, { etag: string; response: HttpResponse<unknown> }>();
  private readonly urls: RegExp[];

  constructor(@Optional() @Inject(ETAG_CACHE_URLS) urls: RegExp[] | null) {
    this.urls = urls || DEFAULT_URLS;
  }

  intercept(req: HttpRequest<unknown>, next: HttpHandler): Observable<HttpEvent<unknown>> {
    if (req.method !== 'GET' || !this.urls.some((re) => re.test(req.urlWithParams))) return next.handle(req);
    // Key includes Accept: JSON and binary variants of the same URL are different resources.
    const key = req.urlWithParams + '|' + (req.headers.get('Accept') || '') + '|' + req.responseType;
    const hit = this.cache.get(key);
    const outgoing = hit ? req.clone({ setHeaders: { 'If-None-Match': hit.etag } }) : req;

    return next.handle(outgoing).pipe(
      tap((ev) => {
        if (ev instanceof HttpResponse && ev.status === 200) {
          const etag = ev.headers.get('ETag');
          if (etag) this.remember(key, etag, ev);
        }
      }),
      catchError((err: unknown) => {
        if (hit && err instanceof HttpErrorResponse && err.status === 304) {
          // Not modified: hand the component the previous body as a normal 200.
          return of(hit.response.clone());
        }
        return throwError(() => err);
      })
    );
  }

  private remember(key: string, etag: string, response: HttpResponse<unknown>): void {
    this.cache.delete(key);
    this.cache.set(key, { etag, response });
    if (this.cache.size > MAX_ENTRIES) this.cache.delete(this.cache.keys().next().value as string);
  }
}
