'use strict';
/**
 * Demo of option C with the shipped behaviour: ../das-01-hotfix-node in --legacy mode
 * (the release with the bug) plus engtools-forward.js, i.e. the only change to the
 * Node.js app is mounting that module. Used by scripts/node-in-front.sh.
 *
 *   DAS_ENGTOOLS_SOCKET=/tmp/engtools.sock PORT=8080 DAS_MODE=legacy node examples/node-in-front/demo-das01.js
 */
var path = require('path');
var app = require(path.join(__dirname, '..', '..', '..', 'das-01-hotfix-node', 'src', 'server.js'));
var engtools = require('./engtools-forward')({ socketPath: process.env.DAS_ENGTOOLS_SOCKET || '/run/das/engtools.sock' });

var inst = app.start();
var server = inst.server;
var appHandler = server.listeners('request')[0];
server.removeAllListeners('request');
server.on('request', function (req, res) { if (!engtools.handle(req, res)) appHandler(req, res); });
server.on('upgrade', function (req, socket, head) { if (!engtools.upgrade(req, socket, head)) socket.destroy(); });
inst.ready.catch(function (err) { console.error(err); process.exit(1); });
process.on('SIGTERM', function () { engtools.close(); inst.close().then(function () { process.exit(0); }); });
process.on('SIGINT', function () { engtools.close(); inst.close().then(function () { process.exit(0); }); });
