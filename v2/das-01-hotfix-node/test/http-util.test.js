'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const h = require('../src/shared/http-util');

const req = (headers, url = '/') => ({ headers, url });

test('If-None-Match uses weak comparison and understands lists and *', () => {
  const tag = '"vd-abc-7"';
  assert.equal(h.ifNoneMatch(req({ 'if-none-match': '"vd-abc-7"' }), tag), true);
  assert.equal(h.ifNoneMatch(req({ 'if-none-match': 'W/"vd-abc-7"' }), tag), true);
  assert.equal(h.ifNoneMatch(req({ 'if-none-match': '"x", "vd-abc-7"' }), tag), true);
  assert.equal(h.ifNoneMatch(req({ 'if-none-match': '*' }), tag), true);
  assert.equal(h.ifNoneMatch(req({ 'if-none-match': '"vd-abc-6"' }), tag), false);
  assert.equal(h.ifNoneMatch(req({}), tag), false);
});

test('If-Match uses strong comparison and reports absence as null', () => {
  const tag = '"cfg-1-3"';
  assert.equal(h.ifMatch(req({}), tag), null);
  assert.equal(h.ifMatch(req({ 'if-match': '"cfg-1-3"' }), tag), true);
  assert.equal(h.ifMatch(req({ 'if-match': 'W/"cfg-1-3"' }), tag), false);
  assert.equal(h.ifMatch(req({ 'if-match': '"cfg-1-2"' }), tag), false);
});

test('Accept-Encoding honours q=0', () => {
  assert.equal(h.acceptsEncoding(req({ 'accept-encoding': 'gzip, deflate, br' }), 'gzip'), true);
  assert.equal(h.acceptsEncoding(req({ 'accept-encoding': 'br;q=1, gzip;q=0' }), 'gzip'), false);
  assert.equal(h.acceptsEncoding(req({ 'accept-encoding': '*' }), 'gzip'), true);
  assert.equal(h.acceptsEncoding(req({}), 'gzip'), false);
});

test('Accept media type detection', () => {
  assert.equal(h.acceptsMediaType(req({ accept: 'application/vnd.das.spectrum, application/json;q=0.5' }), 'application/vnd.das.spectrum'), true);
  assert.equal(h.acceptsMediaType(req({ accept: 'application/json' }), 'application/vnd.das.spectrum'), false);
});

test('query and path parsing', () => {
  assert.deepEqual(h.queryOf(req({}, '/api/x?nodeId=3&port=1&name=a%20b&flag')), { nodeId: '3', port: '1', name: 'a b', flag: '' });
  assert.equal(h.pathOf(req({}, '/api/x?y=1')), '/api/x');
});
