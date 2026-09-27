'use strict';
/**
 * Small, dependency-free HTTP helpers (conditional requests, content
 * negotiation, JSON bodies). They work with plain `http` handlers and with
 * Express, because Express req/res extend the Node objects.
 *
 * ES2019 / CommonJS (Node 12+).
 */

/** Weak comparison per RFC 9110 section 13.1.2: If-None-Match uses weak comparison. */
function stripWeak(tag) { return tag.indexOf('W/') === 0 ? tag.slice(2) : tag; }

/**
 * @param {import('http').IncomingMessage} req
 * @param {string} etag quoted entity tag, e.g. "\"sp-1-42-legacy\""
 */
function ifNoneMatch(req, etag) {
  var h = req.headers['if-none-match'];
  if (!h || !etag) return false;
  if (h.trim() === '*') return true;
  var want = stripWeak(etag);
  var parts = h.split(',');
  for (var i = 0; i < parts.length; i++) if (stripWeak(parts[i].trim()) === want) return true;
  return false;
}

/**
 * Strong comparison for If-Match (RFC 9110 section 13.1.1). Weak tags never match.
 * @returns {boolean|null} null when the header is absent
 */
function ifMatch(req, etag) {
  var h = req.headers['if-match'];
  if (!h) return null;
  if (h.trim() === '*') return true;
  var parts = h.split(',');
  for (var i = 0; i < parts.length; i++) {
    var t = parts[i].trim();
    if (t.indexOf('W/') === 0) continue;
    if (t === etag) return true;
  }
  return false;
}

/** Parse an Accept-style header into [{value, q}] sorted by q descending. */
function parseQualityList(h) {
  if (!h) return [];
  var out = [];
  var parts = h.split(',');
  for (var i = 0; i < parts.length; i++) {
    var seg = parts[i].trim().split(';');
    var value = seg[0].trim().toLowerCase();
    if (!value) continue;
    var q = 1;
    for (var j = 1; j < seg.length; j++) {
      var kv = seg[j].trim().split('=');
      if (kv[0] === 'q') { q = parseFloat(kv[1]); if (!isFinite(q)) q = 0; }
    }
    out.push({ value: value, q: q, order: i });
  }
  out.sort(function (a, b) { return b.q - a.q || a.order - b.order; });
  return out;
}

function acceptsEncoding(req, enc) {
  var list = parseQualityList(req.headers['accept-encoding']);
  for (var i = 0; i < list.length; i++) {
    if (list[i].value === enc || list[i].value === '*') return list[i].q > 0;
  }
  return false;
}

/** True when the client explicitly asks for the given media type with q > 0. */
function acceptsMediaType(req, type) {
  var list = parseQualityList(req.headers.accept);
  for (var i = 0; i < list.length; i++) if (list[i].value === type) return list[i].q > 0;
  return false;
}

function setHeaders(res, headers) {
  if (!headers) return;
  var keys = Object.keys(headers);
  for (var i = 0; i < keys.length; i++) if (headers[keys[i]] != null) res.setHeader(keys[i], headers[keys[i]]);
}

/**
 * Send a pre-built body. The main thread only writes bytes; all encoding
 * work happened earlier (once per change, not once per request).
 */
function sendBuffer(res, status, body, headers) {
  setHeaders(res, headers);
  res.statusCode = status;
  if (body == null) { res.end(); return; }
  res.setHeader('Content-Length', body.length);
  res.end(body);
}

function sendJson(res, status, obj, headers) {
  var body = Buffer.from(JSON.stringify(obj));
  setHeaders(res, headers);
  if (!res.getHeader('Content-Type')) res.setHeader('Content-Type', 'application/json; charset=utf-8');
  sendBuffer(res, status, body);
}

function notModified(res, etag, headers) {
  setHeaders(res, headers);
  res.setHeader('ETag', etag);
  res.statusCode = 304;
  res.end();
}

/** Read a JSON request body with a size limit. */
function readJson(req, limitBytes) {
  var limit = limitBytes || 256 * 1024;
  return new Promise(function (resolve, reject) {
    if (req.body !== undefined && typeof req.body === 'object') return resolve(req.body); // body-parser already ran
    var chunks = [];
    var size = 0;
    req.on('data', function (c) {
      size += c.length;
      if (size > limit) { reject(Object.assign(new Error('payload too large'), { status: 413 })); req.destroy(); return; }
      chunks.push(c);
    });
    req.on('end', function () {
      if (!size) return resolve(undefined);
      try { resolve(JSON.parse(Buffer.concat(chunks).toString('utf8'))); } catch (e) { reject(Object.assign(new Error('invalid JSON'), { status: 400 })); }
    });
    req.on('error', reject);
  });
}

function queryOf(req) {
  var u = req.url || '/';
  var i = u.indexOf('?');
  var out = {};
  if (i < 0) return out;
  var qs = u.slice(i + 1).split('&');
  for (var k = 0; k < qs.length; k++) {
    if (!qs[k]) continue;
    var eq = qs[k].indexOf('=');
    var key = decodeURIComponent((eq < 0 ? qs[k] : qs[k].slice(0, eq)).replace(/\+/g, ' '));
    var val = eq < 0 ? '' : decodeURIComponent(qs[k].slice(eq + 1).replace(/\+/g, ' '));
    out[key] = val;
  }
  return out;
}

function pathOf(req) {
  var u = req.url || '/';
  var i = u.indexOf('?');
  return i < 0 ? u : u.slice(0, i);
}

module.exports = {
  ifNoneMatch: ifNoneMatch,
  ifMatch: ifMatch,
  parseQualityList: parseQualityList,
  acceptsEncoding: acceptsEncoding,
  acceptsMediaType: acceptsMediaType,
  sendBuffer: sendBuffer,
  sendJson: sendJson,
  notModified: notModified,
  readJson: readJson,
  queryOf: queryOf,
  pathOf: pathOf,
  setHeaders: setHeaders
};
