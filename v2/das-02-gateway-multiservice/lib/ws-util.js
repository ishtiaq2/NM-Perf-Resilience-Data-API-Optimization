'use strict';
/**
 * WebSocket helpers: cross-site WebSocket hijacking (CSWSH) protection and a
 * small JSON-message guard.
 *
 * Browsers send an Origin header on every WebSocket handshake and ALWAYS attach
 * the session cookie, even when a foreign page opens the socket. Accept a
 * handshake only when Origin is absent (non-browser tool) or matches the host
 * the page was served from (or an explicit allow-list).
 *
 * ES2019 / CommonJS.
 */

function originAllowed(req, allowList) {
  var origin = req.headers.origin;
  if (!origin) return true;
  var host = req.headers['x-forwarded-host'] || req.headers.host;
  try {
    if (new URL(origin).host === host) return true;
  } catch (e) {
    return false;
  }
  return (allowList || []).indexOf(origin) >= 0;
}

function reject(socket, status, reason) {
  socket.write('HTTP/1.1 ' + status + ' ' + reason + '\r\nConnection: close\r\nContent-Length: 0\r\n\r\n');
  socket.destroy();
}

/** Parse a client text message; small and JSON only. */
function parseMessage(data, isBinary, maxBytes) {
  if (isBinary) return null;
  var s = typeof data === 'string' ? data : data.toString('utf8');
  if (s.length > (maxBytes || 4096)) return null;
  try {
    var m = JSON.parse(s);
    return m && typeof m === 'object' && typeof m.type === 'string' ? m : null;
  } catch (e) {
    return null;
  }
}

module.exports = { originAllowed: originAllowed, reject: reject, parseMessage: parseMessage };
