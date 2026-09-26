'use strict';
/**
 * Node configuration store with optimistic concurrency control.
 *
 * Several engineers can have the same Remote Node open. Every read returns an
 * ETag (bootId + version). A write sent with If-Match succeeds only if nobody
 * changed the node in between; otherwise the server answers 412 and the UI asks
 * the engineer to reload. Nobody silently overwrites a colleague's gain change.
 *
 * In-memory for the PoC. On the device this wraps the existing configuration
 * backend (file, database or hardware registers).
 *
 * ES2019 / CommonJS (Node 12+).
 */

var BANDS = ['B28-700', 'B20-800', 'B8-900', 'B3-1800', 'B1-2100', 'B7-2600'];

function defaultConfig(id) {
  var bands = {};
  for (var i = 0; i < BANDS.length; i++) bands[BANDS[i]] = true;
  return { name: 'RN-' + ('00' + id).slice(-3), dlGainDb: 25, ulGainDb: 15, bandsEnabled: bands, tempAlarmC: 70, notes: '' };
}

function validate(cfg) {
  var errors = [];
  if (!cfg || typeof cfg !== 'object' || Array.isArray(cfg)) return ['body must be a JSON object'];
  function num(k, lo, hi) {
    if (typeof cfg[k] !== 'number' || !isFinite(cfg[k]) || cfg[k] < lo || cfg[k] > hi) errors.push(k + ' must be a number in [' + lo + ', ' + hi + ']');
  }
  if (typeof cfg.name !== 'string' || !cfg.name.length || cfg.name.length > 32) errors.push('name must be a string of 1-32 characters');
  num('dlGainDb', 0, 40);
  num('ulGainDb', 0, 30);
  num('tempAlarmC', 40, 90);
  if (typeof cfg.notes !== 'string' || cfg.notes.length > 500) errors.push('notes must be a string of at most 500 characters');
  if (!cfg.bandsEnabled || typeof cfg.bandsEnabled !== 'object') errors.push('bandsEnabled must be an object');
  else {
    var ks = Object.keys(cfg.bandsEnabled);
    for (var i = 0; i < ks.length; i++) {
      if (BANDS.indexOf(ks[i]) < 0) errors.push('unknown band ' + ks[i]);
      else if (typeof cfg.bandsEnabled[ks[i]] !== 'boolean') errors.push('bandsEnabled.' + ks[i] + ' must be boolean');
    }
  }
  return errors;
}

/**
 * @param {object} opts
 * @param {string} opts.bootId
 * @param {number} opts.nodes number of nodes (ids 1..nodes)
 */
function ConfigStore(opts) {
  this.bootId = opts.bootId;
  this.count = opts.nodes;
  this.items = new Map();
}

ConfigStore.prototype.has = function (id) { return id >= 1 && id <= this.count; };

ConfigStore.prototype.get = function (id) {
  if (!this.has(id)) return null;
  var it = this.items.get(id);
  if (!it) { it = { version: 1, config: defaultConfig(id), updatedAt: Date.now() }; this.items.set(id, it); }
  return { nodeId: id, version: it.version, updatedAt: it.updatedAt, config: it.config };
};

ConfigStore.prototype.etag = function (id) {
  var it = this.get(id);
  return it ? '"cfg-' + this.bootId + '-' + id + '-' + it.version + '"' : null;
};

/**
 * @param {number} id
 * @param {object} config full replacement
 * @param {boolean|null} precondition result of ifMatch(): true, false or null (header absent)
 * @returns {{status:number, body:object}}
 */
ConfigStore.prototype.put = function (id, config, precondition) {
  if (!this.has(id)) return { status: 404, body: { error: 'NOT_FOUND', message: 'unknown node ' + id } };
  if (precondition === false) {
    var cur = this.get(id);
    return { status: 412, body: { error: 'PRECONDITION_FAILED', message: 'node ' + id + ' was changed by someone else (now version ' + cur.version + '); reload and re-apply', current: cur } };
  }
  var errors = validate(config);
  if (errors.length) return { status: 422, body: { error: 'VALIDATION_FAILED', errors: errors } };
  var it = this.items.get(id) || { version: 1, config: defaultConfig(id), updatedAt: Date.now() };
  it.version++;
  it.config = { name: config.name, dlGainDb: config.dlGainDb, ulGainDb: config.ulGainDb, bandsEnabled: Object.assign({}, config.bandsEnabled), tempAlarmC: config.tempAlarmC, notes: config.notes };
  it.updatedAt = Date.now();
  this.items.set(id, it);
  return { status: 200, body: this.get(id) };
};

module.exports = { ConfigStore: ConfigStore, validate: validate, defaultConfig: defaultConfig, BANDS: BANDS };
