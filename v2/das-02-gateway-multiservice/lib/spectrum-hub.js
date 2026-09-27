'use strict';
/**
 * WebSocket push for the spectrum analyzer (/api/ws/spectrum). Protocol: contract/asyncapi.yaml.
 *
 *  - The client subscribes to one analyzer ({nodeId, port, span, points}) and
 *    sets maxPoints to its canvas width. Every completed sweep is pushed as one
 *    binary DSPC frame (~3 kB decimated instead of ~2 MB of JSON).
 *  - Frames are encoded once per sweep and variant (worker thread + per-sweep
 *    cache) and shared by every subscriber with the same variant.
 *  - Conflation: when a client's socket buffer is above the high-water mark,
 *    the frame is dropped for that client. The next sweep supersedes it, so a
 *    slow client sees a lower refresh rate, never growing lag or server memory.
 *  - A subscription keeps its sweep session alive; when the last subscriber
 *    leaves, the session idles out and the hardware stops sweeping.
 *
 * ES2019 / CommonJS. Depends on `ws`.
 */

var WebSocket = require('ws');
var wsUtil = require('./ws-util');
var decimate = require('./decimate');

function SpectrumHub(o) {
  var self = this;
  this.spectrum = o.spectrum;
  this.serverName = o.server;
  this.log = o.log || function () {};
  this.defaultPoints = o.defaultPoints || 50001;
  this.heartbeatMs = o.heartbeatMs || 5000;
  this.highWaterMark = o.highWaterMark || 256 * 1024;
  this.maxClients = o.maxClients || 32;
  this.allowedOrigins = o.allowedOrigins || [];
  this.clients = new Set();
  this.stats = { connections: 0, framesSent: 0, framesDropped: 0, bytesSent: 0, rejected: 0 };
  this.wss = new WebSocket.Server({ noServer: true, perMessageDeflate: false, maxPayload: 4096, clientTracking: false });
  this.spectrum.on('sweep', function (key, sweep) { self._onSweep(key, sweep); });
  this.tickTimer = setInterval(function () { self._tick(); }, 1000);
  this.pingTimer = setInterval(function () { self._ping(); }, 30000);
  if (this.tickTimer.unref) { this.tickTimer.unref(); this.pingTimer.unref(); }
}

SpectrumHub.prototype.handleUpgrade = function (req, socket, head) {
  var self = this;
  if (!wsUtil.originAllowed(req, this.allowedOrigins)) { this.stats.rejected++; return wsUtil.reject(socket, 403, 'Forbidden'); }
  if (this.clients.size >= this.maxClients) { this.stats.rejected++; return wsUtil.reject(socket, 503, 'Service Unavailable'); }
  this.wss.handleUpgrade(req, socket, head, function (ws) { self._onConnection(ws); });
};

SpectrumHub.prototype._text = function (c, obj) {
  if (c.ws.readyState !== WebSocket.OPEN) return;
  var s = JSON.stringify(obj);
  c.ws.send(s);
  c.lastSend = Date.now();
  this.stats.bytesSent += s.length;
};

SpectrumHub.prototype._onConnection = function (ws) {
  var self = this;
  var c = { ws: ws, session: null, variant: null, lastSend: 0, alive: true, lastSweepSent: 0 };
  this.clients.add(c);
  this.stats.connections++;
  this._text(c, { type: 'hello', api: 'das-v1', server: this.serverName, topic: 'spectrum', heartbeatMs: this.heartbeatMs, time: Date.now() });
  ws.on('message', function (data, isBinary) {
    var m = wsUtil.parseMessage(data, isBinary);
    if (!m) return self._text(c, { type: 'error', code: 'BAD_REQUEST', message: 'expected a JSON object with a "type"' });
    if (m.type === 'sub') self._subscribe(c, m);
    else if (m.type === 'unsub') { c.session = null; c.variant = null; }
    else self._text(c, { type: 'error', code: 'BAD_REQUEST', message: 'unknown type ' + m.type });
  });
  ws.on('pong', function () { c.alive = true; });
  ws.on('close', function () { self.clients.delete(c); });
  ws.on('error', function () { /* close follows */ });
};

SpectrumHub.prototype._subscribe = function (c, m) {
  var session;
  try {
    session = this.spectrum.session({ nodeId: m.nodeId, port: m.port, startHz: m.startHz, stopHz: m.stopHz, points: m.points || this.defaultPoints });
  } catch (e) {
    return this._text(c, { type: 'error', code: e.code === 'TOO_MANY_SESSIONS' ? e.code : 'BAD_REQUEST', message: e.message });
  }
  c.session = session;
  c.variant = { format: m.encoding === 'f32' ? 'binary-f32' : 'binary-i16', maxPoints: decimate.normaliseMaxPoints(m.maxPoints), gzip: false };
  c.lastSweepSent = 0;
  this._text(c, { type: 'subscribed', nodeId: session.params.nodeId, port: session.params.port, maxPoints: c.variant.maxPoints });
  if (session.latest) this._push(c, session.latest); // show something immediately
};

SpectrumHub.prototype._push = function (c, sweep) {
  var self = this;
  if (c.ws.readyState !== WebSocket.OPEN || !c.session) return;
  if (c.ws.bufferedAmount > this.highWaterMark) { this.stats.framesDropped++; return; } // conflate: newest sweep wins
  var session = c.session;
  var variant = c.variant;
  session.variant(sweep, variant).then(function (out) {
    // The client may have re-subscribed or left while the frame was encoded.
    if (c.session !== session || c.variant !== variant || c.ws.readyState !== WebSocket.OPEN) return;
    if (sweep.sweepId <= c.lastSweepSent) return;
    c.ws.send(out.body, { binary: true });
    c.lastSweepSent = sweep.sweepId;
    c.lastSend = Date.now();
    self.stats.framesSent++;
    self.stats.bytesSent += out.body.length;
  }, function (err) {
    self.stats.framesDropped++;
    if (err && err.code !== 'POOL_BUSY') self.log('spectrum_push_error', { error: String(err.message || err) });
  });
};

SpectrumHub.prototype._onSweep = function (key, sweep) {
  var self = this;
  this.clients.forEach(function (c) {
    if (!c.session || c.session.key !== key) return;
    c.session.touch(); // an open analyzer view keeps its session sweeping
    self._push(c, sweep);
  });
};

SpectrumHub.prototype._tick = function () {
  var now = Date.now();
  var self = this;
  this.clients.forEach(function (c) {
    if (c.session) c.session.touch();
    if (c.ws.readyState === WebSocket.OPEN && now - c.lastSend >= self.heartbeatMs) self._text(c, { type: 'hb', time: now });
  });
};

SpectrumHub.prototype._ping = function () {
  this.clients.forEach(function (c) {
    if (!c.alive) { c.ws.terminate(); return; }
    c.alive = false;
    try { c.ws.ping(); } catch (e) { /* closing */ }
  });
};

SpectrumHub.prototype.status = function () {
  var subscribed = 0;
  this.clients.forEach(function (c) { if (c.session) subscribed++; });
  return { clients: this.clients.size, subscribed: subscribed, stats: Object.assign({}, this.stats) };
};

SpectrumHub.prototype.close = function () {
  clearInterval(this.tickTimer);
  clearInterval(this.pingTimer);
  this.clients.forEach(function (c) { try { c.ws.close(1001, 'server shutting down'); } catch (e) { /* ignore */ } });
};

module.exports = { SpectrumHub: SpectrumHub };
