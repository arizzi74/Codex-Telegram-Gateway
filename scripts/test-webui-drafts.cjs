#!/usr/bin/env node
'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { webcrypto } = require('node:crypto');
const source = fs.readFileSync(path.join(__dirname, '../internal/admin/static/webui-drafts.js'), 'utf8');
const sessionID = '11111111-2222-4333-8444-555555555555';
const storageKey = 'codex-webui-encrypted-text-draft-v1';
const storage = () => { const values = new Map(); return { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, String(value)), removeItem: key => values.delete(key), values }; };
const failure = status => Object.assign(Error('HTTP ' + status), { status });
const deferred = () => { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; };

function setup(previous = {}) {
  const localStorage = previous.localStorage || storage(), sessionStorage = previous.sessionStorage || storage();
  const rows = previous.rows || new Map(), calls = [], messages = [];
  let identity = 'account-one', heldPut = null, putArrived = null, cryptoGate = null;
  const crypto = { getRandomValues: bytes => webcrypto.getRandomValues(bytes), randomUUID: () => webcrypto.randomUUID(), subtle: {
    importKey: (...args) => webcrypto.subtle.importKey(...args),
    encrypt: async (...args) => { if (cryptoGate) await cryptoGate.promise; return webcrypto.subtle.encrypt(...args); },
    decrypt: (...args) => webcrypto.subtle.decrypt(...args)
  } };
  const window = {};
  vm.runInNewContext(source, { window, localStorage, sessionStorage, crypto, TextEncoder, TextDecoder, Uint8Array, AbortController, setTimeout, clearTimeout, atob, btoa, Date });
  const request = async (url, opts) => {
    const id = url.split('/').at(-1), account = identity, body = opts.body && JSON.parse(opts.body);
    calls.push({ id, method: opts.method, body, account });
    if (opts.method === 'PUT' && heldPut) { putArrived.resolve(); await heldPut.promise; }
    if (!account) throw failure(401);
    const prior = rows.get(id);
    if (prior && prior.owner !== account) throw failure(404);
    if (opts.method === 'GET') { if (!prior || prior.deleted) throw failure(404); return { ciphertext: prior.ciphertext, revision: prior.revision, expires_at: '2020-01-01T00:00:00Z' }; }
    if (opts.method === 'DELETE') { rows.set(id, { owner: account, deleted: true, revision: body.revision }); return null; }
    if (prior?.deleted || (prior && prior.revision >= body.revision)) throw failure(409);
    rows.set(id, { owner: account, ciphertext: body.ciphertext, revision: body.revision, deleted: false });
    return { revision: body.revision, expires_at: '2020-01-01T00:00:00Z' }; // Deliberate gateway/browser clock skew.
  };
  const helper = window.CodexDraftRecovery.create({ request, getIdentity: () => identity, onStatus: message => messages.push(message) });
  return { helper, rows, calls, messages, localStorage, sessionStorage,
    identity(value) { identity = value; },
    holdPut() { heldPut = deferred(); putArrived = deferred(); return { entered: putArrived.promise, release: () => heldPut.resolve() }; },
    holdEncryption() { cryptoGate = deferred(); return () => cryptoGate.resolve(); }
  };
}

(async () => {
  const a = setup();
  await a.helper.update({ [sessionID]: 'private draft' });
  assert.equal(a.calls.length, 0, 'Recovery is explicitly opt in');
  await a.helper.setEnabled(true);
  await a.helper.update({ [sessionID]: 'private draft with a secret', image: { data: 'must not persist' } });
  assert.equal(a.calls.length, 1);
  assert.equal(a.calls[0].method, 'PUT');
  const record = JSON.parse(a.sessionStorage.getItem(storageKey));
  assert.equal(Buffer.from(record.key, 'base64url').length, 32);
  assert.equal(a.sessionStorage.getItem(storageKey).includes('private draft'), false);
  assert.equal([...a.localStorage.values.values()].some(value => value.includes('private draft')), false);
  assert.equal(JSON.stringify([...a.rows.values()]).includes('private draft'), false, 'The server only receives ciphertext');
  assert.equal(a.calls[0].body.ciphertext.includes(record.key), false, 'Encryption key never sent to gateway');
  a.helper.expireAuth(); a.identity('');
  await a.helper.update({ [sessionID]: 'new private text while locked' });
  assert.equal(a.calls.length, 1);
  assert.ok(a.sessionStorage.getItem(storageKey), 'Expiry retains only encrypted-recovery metadata');
  const reload = setup(a);
  reload.helper.identityChanged();
  const recovered = await reload.helper.recover();
  assert.equal(recovered[sessionID], 'private draft with a secret', 'Same-tab reload and gateway clock skew do not lose recovery');
  assert.deepEqual(Object.keys(recovered), [sessionID], 'Only text drafts are restored');
  assert.equal(reload.calls.at(-1).method, 'GET', 'Merely offering restore does not consume recovery');
  await reload.helper.consume();
  assert.equal(reload.calls.at(-1).method, 'DELETE');
  assert.equal(reload.sessionStorage.getItem(storageKey), null);
  assert.equal(reload.rows.get(record.id).deleted, true);
  await reload.helper.update({ [sessionID]: 'new text' });
  const rotated = JSON.parse(reload.sessionStorage.getItem(storageKey));
  assert.notEqual(rotated.id, record.id); assert.notEqual(rotated.key, record.key);

  const encryptedRace = setup(); await encryptedRace.helper.setEnabled(true);
  const releaseEncryption = encryptedRace.holdEncryption();
  const updating = encryptedRace.helper.update({ [sessionID]: 'do not restore a sent prompt' });
  await new Promise(resolve => setTimeout(resolve, 5));
  const clearing = encryptedRace.helper.clear(); releaseEncryption();
  await Promise.all([updating, clearing]);
  assert.equal(encryptedRace.calls.filter(call => call.method === 'PUT').length, 0, 'Clearing while encryption runs fences the old plaintext');
  assert.equal(encryptedRace.sessionStorage.getItem(storageKey), null);

  const networkRace = setup(); await networkRace.helper.setEnabled(true);
  const held = networkRace.holdPut();
  const saving = networkRace.helper.update({ [sessionID]: 'uncertain submitted text' });
  await held.entered;
  const clearingNetwork = networkRace.helper.update({});
  assert.equal(networkRace.sessionStorage.getItem(storageKey), null, 'Local key clears immediately, even with an in-flight save');
  held.release(); await Promise.all([saving, clearingNetwork]);
  assert.deepEqual(networkRace.calls.map(call => call.method), ['PUT', 'DELETE']);
  assert.equal([...networkRace.rows.values()][0].deleted, true, 'A late upload cannot resurrect a cleared recovery copy');

  const switched = setup(); await switched.helper.setEnabled(true); await switched.helper.update({ [sessionID]: 'account-one secret' });
  const beforeSwitch = switched.calls.length; switched.identity('account-two'); switched.helper.identityChanged();
  assert.equal(switched.sessionStorage.getItem(storageKey), null, 'Cross-account login drops the old decryption key');
  assert.equal(await switched.helper.recover(), null);
  assert.equal(switched.calls.length, beforeSwitch, 'Different accounts never request the previous account recovery');

  const corrupted = setup(); await corrupted.helper.setEnabled(true); await corrupted.helper.update({ [sessionID]: 'sensitive' });
  const row = [...corrupted.rows.values()][0]; row.ciphertext = Buffer.alloc(50, 42).toString('base64url');
  assert.equal(await corrupted.helper.recover(), null, 'Authenticated encryption rejects modified ciphertext');
  await corrupted.helper.setEnabled(false); assert.equal(corrupted.sessionStorage.getItem(storageKey), null);
  assert.equal(corrupted.helper.enabled(), false);
  console.log('Encrypted text draft recovery checks passed.');
})().catch(error => { console.error(error); process.exitCode = 1; });
