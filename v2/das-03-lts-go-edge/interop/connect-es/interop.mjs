// Interop check: the official Connect-ES client, generated from contract/proto,
// streams to das-edge's ingest endpoint (bidirectional, HTTP/2 without TLS, JSON codec).
//
//   ../../bin/das-edge -nodes 0 -ingest 127.0.0.1:9090 -listen 127.0.0.1:8080 &
//   npm install && npm run generate && node interop.mjs http://127.0.0.1:9090 http://127.0.0.1:8080
import assert from 'node:assert/strict';
import { create } from '@bufbuild/protobuf';
import { createClient } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-node';
import { AlarmSeverity, NodeStatus } from './gen/das/v1/common_pb.js';
import { NodeIngestService, ReportRequestSchema, ReportResponse_Action } from './gen/das/v1/ingest_pb.js';

const ingestBase = process.argv[2] || 'http://127.0.0.1:9090';
const apiBase = process.argv[3] || 'http://127.0.0.1:8080';
const client = createClient(NodeIngestService, createConnectTransport({ baseUrl: ingestBase, httpVersion: '2', useBinaryFormat: false }));

const id = 901;
const state = {
  id, name: 'RN-901', status: NodeStatus.ONLINE, fw: '4.2.1', bootAtMs: 1790000000000n,
  temperatureC: 41.5, fanRpm: 5300, psuVoltageV: 48, chain: 57, hop: 5,
  optical: { rxDbm: -7.25, txDbm: 1.5, laserBiasMa: 31 },
  bands: [{ name: 'B3-1800', enabled: true, dlOutDbm: 30.5, ulInDbm: -95, dlGainDb: 25, ulGainDb: 15, vswr: 1.25 }],
  metrics: { m00: 12.5 },
  alarms: [{ code: 'TEMP_HIGH', severity: AlarmSeverity.MINOR, sinceMs: 1790000000000n }]
};

async function* reports() {
  yield create(ReportRequestSchema, { nodeId: id, seq: 1n, full: true, state, reportedAtMs: BigInt(Date.now()) });
  // A delta against a base the master never saw must be answered with RESYNC.
  yield create(ReportRequestSchema, { nodeId: id, seq: 2n, baseHash: 12345n, stateHash: 1n, changedPaths: ['temperature_c'], state: { temperatureC: 42 } });
}

const answers = [];
for await (const res of client.report(reports())) answers.push(res);
assert.equal(answers.length, 2, 'one response per report');
assert.equal(answers[0].action, ReportResponse_Action.OK);
assert.equal(answers[0].ackedSeq, 1n);
assert.equal(answers[1].action, ReportResponse_Action.RESYNC);
console.log(`ok  bidirectional stream: ${answers.map((a) => ReportResponse_Action[a.action]).join(', ')}`);

// The full report is visible through the REST API, mapped to the das-v1 JSON model.
await new Promise((r) => setTimeout(r, 1500)); // next publish
const snap = await (await fetch(`${apiBase}/api/volatile-data`)).json();
const n = snap.nodes[String(id)];
assert.ok(n, `node ${id} not in /api/volatile-data`);
assert.equal(n.status, 'online');
assert.equal(n.temperatureC, 41.5);
assert.equal(n.bands[0].vswr, 1.25);
assert.equal(n.alarms[0].severity, 'minor');
assert.equal(n.bootAt, 1790000000000);
console.log(`ok  node ${id} published: ${n.name}, ${n.temperatureC} °C, alarm ${n.alarms[0].code}/${n.alarms[0].severity}`);
