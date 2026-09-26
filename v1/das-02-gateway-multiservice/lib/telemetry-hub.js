'use strict';
/**
 * WebSocket push for volatile_data (/api/ws/telemetry). Protocol: contract/asyncapi.yaml.
 *
 *  - One delta message is serialised per published revision and the same string
 *    is sent to every client that is up to date: O(1) serialisation per change,
 *    no matter how many engineers are connected.
 *  - Slow client (socket buffer above the high-water mark): skip it, and once it
 *    drains send ONE combined catch-up delta (or a snapshot if history no longer
 *    covers it). Memory per client stays bounded.
 *  - Resume after reconnect: {type:"sub", rev:N} returns the missing delta.
 *  - Liveness: "hb" after heartbeatMs of silence; protocol ping/pong every 30 s
 *    terminates dead peers (e.g. a laptop that went to sleep).
 *  - permessage-deflate (level 1, small window) cuts telemetry traffic ~4x.
 *
 * ES2019 / CommonJS. Depends on `ws` (pure JS, no native add-ons required).
 */

var WebSocket = require('ws');
var wsUtil = require('./ws-util');

/**
 * @param {object} o
 * @param {import('./volatile-store').VolatileStore} o.store
 * @param {string} o.server server name for the hello message
 * @param {function} o.log
 * @param {number} [o.heartbeatMs=5000]
 * @param {number} [o.highWaterMark=524288] bytes buffered before a client counts as slow
 * @param {number} [o.maxClients=200]
 * @param {string[]} [o.allowedOrigins]
 */
function TelemetryHub(o) {
  var self = this;
  this.store = o.store;
  this.serverName = o.server;
  this.log = o.log || function () {};
  this.heartbeatMs = o.heartbeatMs || 5000;
  this.highWaterMark = o.highWaterMark || 512 * 1024;
  this.maxClients = o.maxClients || 200;
  this.allowedOrigins = o.allowedOrigins || [];
  this.clients = new Set();
  this.stats = { connections: 0, deltasSent: 0, snapshotsSent: 0, catchUps: 0, skippedSlow: 0, bytesSent: 0, rejected: 0 };
  // permessage-deflate: telemetry JSON compresses ~4x. Level 1 and a 4 kB window
  // keep CPU and memory per connection small; zlib runs in the libuv pool, not on
  // the event loop. Small messages (hb, pong) are sent uncompressed.
  this.wss = new WebSocket.Server({
    noServer: true,
    clientTracking: false,
    maxPayload: 16 * 1024,
    perMessageDeflate: o.compress === false ? false : {
      zlibDeflateOptions: { level: 1, memLevel: 6 },
      serverMaxWindowBits: 12,
      threshold: 1024,
      concurrencyLimit: 2
    }
  });
  this.store.on('publish', function (ev) { self._onPublish(ev); });
  this.tickTimer = setInterval(function () { self._tick(); }, 250);
  this.pingTimer = setInterval(function () { self._ping(); }, 30000);
  if (this.tickTimer.unref) { this.tickTimer.unref(); this.pingTimer.unref(); }
}

TelemetryHub.prototype.handleUpgrade = function (req, socket, head) {
  var self = this;
  if (!wsUtil.originAllowed(req, this.allowedOrigins)) { this.stats.rejected++; return wsUtil.reject(socket, 403, 'Forbidden'); }
  if (this.clients.size >= this.maxClients) { this.stats.rejected++; return wsUtil.reject(socket, 503, 'Service Unavailable'); }
  this.wss.handleUpgrade(req, socket, head, function (ws) { self._onConnection(ws); });
};

TelemetryHub.prototype._onConnection = function (ws) {
  var self = this;
  var c = { ws: ws, rev: -1, subscribed: false, lagging: false, lastSend: 0, alive: true };
  this.clients.add(c);
  this.stats.connections++;
  this._send(c, JSON.stringify({ type: 'hello', api: 'das-v1', server: this.serverName, topic: 'volatile', heartbeatMs: this.heartbeatMs, time: Date.now() }));
  ws.on('message', function (data, isBinary) {
    var m = wsUtil.parseMessage(data, isBinary);
    if (!m) return self._send(c, JSON.stringify({ type: 'error', code: 'BAD_REQUEST', message: 'expected a JSON object with a "type"' }));
    if (m.type === 'sub') self._subscribe(c, m.rev);
    else if (m.type === 'ping') self._send(c, JSON.stringify({ type: 'pong', id: m.id, time: Date.now() }));
    else self._send(c, JSON.stringify({ type: 'error', code: 'BAD_REQUEST', message: 'unknown type ' + m.type }));
  });
  ws.on('pong', function () { c.alive = true; });
  ws.on('close', function () {
    self.clients.delete(c);
    if (ws._socket) self.stats.wireBytesClosed = (self.stats.wireBytesClosed || 0) + ws._socket.bytesWritten;
  });
  ws.on('error', function () { /* close follows */ });
};

TelemetryHub.prototype._send = function (c, text) {
  if (c.ws.readyState !== WebSocket.OPEN) return;
  c.ws.send(text);
  c.lastSend = Date.now();
  this.stats.bytesSent += text.length;
};

TelemetryHub.prototype._snapshotMessage = function () {
  var rev = this.store.rev;
  if (!this.snapMsg || this.snapMsg.rev !== rev) this.snapMsg = { rev: rev, text: '{"type":"snapshot",' + this.store.snapshotJson().slice(1) };
  return this.snapMsg.text;
};

/** Bring a client to the current revision with one message. */
TelemetryHub.prototype._catchUp = function (c, fromRev) {
  var delta = typeof fromRev === 'number' && fromRev >= 0 ? this.store.deltaJson(fromRev) : null;
  if (delta) { this._send(c, '{"type":"delta",' + delta.slice(1)); this.stats.catchUps++; } else { this._send(c, this._snapshotMessage()); this.stats.snapshotsSent++; }
  c.rev = this.store.rev;
  c.lagging = false;
};

TelemetryHub.prototype._subscribe = function (c, rev) {
  c.subscribed = true;
  this._catchUp(c, typeof rev === 'number' ? rev : null);
};

TelemetryHub.prototype._onPublish = function (ev) {
  var text = null;
  var self = this;
  this.clients.forEach(function (c) {
    if (!c.subscribed || c.ws.readyState !== WebSocket.OPEN) return;
    if (c.ws.bufferedAmount > self.highWaterMark) { c.lagging = true; self.stats.skippedSlow++; return; }
    if (c.rev === ev.base) {
      if (text === null) text = '{"type":"delta",' + self.store.publishDeltaJson(ev).slice(1);
      self._send(c, text);
      c.rev = ev.rev;
      self.stats.deltasSent++;
    } else {
      self._catchUp(c, c.rev);
    }
  });
};

TelemetryHub.prototype._tick = function () {
  var now = Date.now();
  var self = this;
  this.clients.forEach(function (c) {
    if (c.ws.readyState !== WebSocket.OPEN) return;
    if (c.lagging && c.ws.bufferedAmount < self.highWaterMark / 4) self._catchUp(c, c.rev);
    else if (now - c.lastSend >= self.heartbeatMs) self._send(c, '{"type":"hb","time":' + now + ',"rev":' + self.store.rev + '}');
  });
};

TelemetryHub.prototype._ping = function () {
  this.clients.forEach(function (c) {
    if (!c.alive) { c.ws.terminate(); return; }
    c.alive = false;
    try { c.ws.ping(); } catch (e) { /* closing */ }
  });
};

TelemetryHub.prototype.status = function () {
  var lagging = 0;
  var wire = this.stats.wireBytesClosed || 0;
  this.clients.forEach(function (c) {
    if (c.lagging) lagging++;
    if (c.ws._socket) wire += c.ws._socket.bytesWritten; // after compression, incl. frame headers
  });
  return { clients: this.clients.size, lagging: lagging, stats: Object.assign({}, this.stats, { wireBytes: wire }) };
};

TelemetryHub.prototype.close = function () {
  clearInterval(this.tickTimer);
  clearInterval(this.pingTimer);
  this.clients.forEach(function (c) { try { c.ws.close(1001, 'server shutting down'); } catch (e) { /* ignore */ } });
};

module.exports = { TelemetryHub: TelemetryHub };
