'use strict';
(() => {
  const storageKey = 'codex-webui-encrypted-text-draft-v1';
  const preferenceKey = 'codex-webui-text-draft-recovery-enabled';
  const endpoint = '/tgw/api/v1/webui/drafts/';
  const ttl = 30 * 60 * 1000, maxBytes = 256 * 1024;
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
  const base64url = /^[A-Za-z0-9_-]+$/;
  const encoder = new TextEncoder(), decoder = new TextDecoder('utf-8', { fatal: true });
  function encode(bytes) { let raw = ''; for (let i = 0; i < bytes.length; i += 8192) raw += String.fromCharCode(...bytes.subarray(i, i + 8192)); return btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, ''); }
  function decode(value) { if (typeof value !== 'string' || !base64url.test(value)) throw Error('Invalid recovery data.'); const raw = atob(value.replace(/-/g, '+').replace(/_/g, '/')); return Uint8Array.from(raw, c => c.charCodeAt(0)); }
  function read(storage, key) { try { return globalThis[storage].getItem(key); } catch (_) { return null; } }
  function remove(storage, key) { try { globalThis[storage].removeItem(key); } catch (_) { /* No persistent key remains available to this page. */ } }
  function normalize(snapshot) {
    if (!snapshot || typeof snapshot !== 'object' || Array.isArray(snapshot)) return {};
    const result = Object.create(null);
    for (const [id, text] of Object.entries(snapshot)) {
      if (!uuid.test(id) || typeof text !== 'string' || !text.trim()) continue;
      result[id] = text;
    }
    return result;
  }
  window.CodexDraftRecovery = { create({ request, getIdentity, onStatus = () => {} }) {
    let enabled = read('localStorage', preferenceKey) === 'true';
    let authenticated = !!getIdentity(), generation = 0, chain = Promise.resolve(), metadata = null;
    const pending = new Set();
    const owner = () => { const value = getIdentity(); return typeof value === 'string' && value.length > 0 && value.length <= 256 ? value : ''; };
    const supported = () => !!globalThis.crypto?.subtle && !!globalThis.crypto?.getRandomValues && !!globalThis.crypto?.randomUUID;
    const status = message => { try { onStatus(message); } catch (_) { /* Status rendering cannot change recovery state. */ } };
    function forget() { if (metadata) metadata.key = ''; metadata = null; remove('sessionStorage', storageKey); }
    function invalidate() { generation++; for (const job of pending) job.snapshot = null; }
    function current(ticket, identity) { return enabled && authenticated && ticket === generation && owner() === identity; }
    function persist(value) {
      try { sessionStorage.setItem(storageKey, JSON.stringify(value)); }
      catch (_) { forget(); throw Error('This browser cannot retain an encrypted recovery key for this tab.'); }
    }
    function stored() {
      if (metadata) return metadata;
      try {
        const value = JSON.parse(read('sessionStorage', storageKey) || 'null');
        if (!value) return null;
        if (value.v !== 1 || !uuid.test(value.id) || typeof value.owner !== 'string' || !Number.isSafeInteger(value.revision) || value.revision < 0 || !Number.isFinite(value.expiresAt) || value.expiresAt <= Date.now() || decode(value.key).length !== 32) { forget(); return null; }
        metadata = value; return value;
      } catch (_) { forget(); return null; }
    }
    function serialize(action) { const result = chain.then(action); chain = result.catch(() => {}); return result; }
    async function send(id, method, body) {
      const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 15000);
      try { return await request(endpoint + id, { method, cache: 'no-store', signal: controller.signal, ...(body ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) } : {}) }); }
      finally { clearTimeout(timer); }
    }
    function deletionRecord(value) { return value ? { id: value.id, owner: value.owner, revision: Math.max(1, Math.min(Number.MAX_SAFE_INTEGER, value.revision + 1)) } : null; }
    function clear() {
      const prior = deletionRecord(stored());
      invalidate(); forget();
      return serialize(async () => {
        if (!prior || !authenticated || owner() !== prior.owner) return;
        try { await send(prior.id, 'DELETE', { revision: prior.revision }); }
        catch (error) { if (error.status !== 401 && error.status !== 404) status('The local recovery key was cleared. Its encrypted server copy expires within 30 minutes.'); }
      });
    }
    function update(snapshot) {
      if (!enabled || !authenticated || !owner()) return Promise.resolve();
      const clean = normalize(snapshot);
      if (!Object.keys(clean).length) return clear();
      if (!supported()) { status('Encrypted text draft recovery is unavailable in this browser.'); return Promise.resolve(); }
      invalidate();
      const identity = owner(), ticket = generation, job = { snapshot: clean };
      pending.add(job);
      return serialize(async () => {
        try {
          if (!job.snapshot || !current(ticket, identity)) return;
          let record = stored();
          if (record && record.owner !== identity) { forget(); record = null; }
          if (!record) {
            record = { v: 1, id: crypto.randomUUID(), key: encode(crypto.getRandomValues(new Uint8Array(32))), owner: identity, revision: 0, expiresAt: Date.now() + ttl };
            metadata = record; persist(record);
          }
          const bytes = encoder.encode(JSON.stringify({ v: 1, drafts: job.snapshot }));
          job.snapshot = null;
          if (bytes.length + 28 > maxBytes) { bytes.fill(0); throw Error('These text drafts exceed the encrypted recovery size limit.'); }
          const nonce = crypto.getRandomValues(new Uint8Array(12));
          let encrypted;
          try {
            const keyBytes = decode(record.key);
            let key; try { key = await crypto.subtle.importKey('raw', keyBytes, 'AES-GCM', false, ['encrypt']); } finally { keyBytes.fill(0); }
            if (!current(ticket, identity)) return;
            encrypted = new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: nonce, additionalData: encoder.encode(record.id + ':' + identity) }, key, bytes));
          } finally { bytes.fill(0); }
          if (!current(ticket, identity) || metadata !== record) return;
          const envelope = new Uint8Array(nonce.length + encrypted.length); envelope.set(nonce); envelope.set(encrypted, nonce.length);
          const revision = record.revision + 1;
          if (!Number.isSafeInteger(revision)) throw Error('Start a new text draft recovery copy.');
          record.revision = revision; record.expiresAt = Date.now() + ttl; persist(record);
          const result = await send(record.id, 'PUT', { ciphertext: encode(envelope), revision });
          if (!current(ticket, identity) || metadata !== record) return;
          if (result?.revision !== revision || !Number.isFinite(Date.parse(result?.expires_at))) throw Error('Unexpected encrypted recovery response.');
          // Browser and gateway clocks may differ. The server enforces expiry;
          // this local deadline only bounds retention of this tab's key.
          record.expiresAt = Date.now() + ttl; persist(record); status('Encrypted text draft recovery is on. Images are not saved.');
        } catch (error) { if (current(ticket, identity)) status(error.status === 401 ? 'Sign in again to continue encrypted text draft recovery.' : (error.message || 'Could not save encrypted text drafts.')); }
        finally { job.snapshot = null; pending.delete(job); }
      });
    }
    function recover() {
      const identity = owner(), ticket = generation;
      return serialize(async () => {
        if (!current(ticket, identity) || !supported()) return null;
        const record = stored();
        if (!record) return null;
        if (record.owner !== identity) { forget(); return null; }
        let bytes;
        try {
          const result = await send(record.id, 'GET');
          if (!current(ticket, identity) || metadata !== record) return null;
          if (typeof result?.ciphertext !== 'string' || result.ciphertext.length > Math.ceil(maxBytes * 4 / 3) || !Number.isSafeInteger(result.revision) || result.revision < 1 || !Number.isFinite(Date.parse(result.expires_at))) throw Error('Invalid encrypted recovery copy.');
          const envelope = decode(result.ciphertext); if (envelope.length <= 28 || envelope.length > maxBytes) throw Error('Invalid encrypted recovery copy.');
          const keyBytes = decode(record.key);
          let key; try { key = await crypto.subtle.importKey('raw', keyBytes, 'AES-GCM', false, ['decrypt']); } finally { keyBytes.fill(0); }
          if (!current(ticket, identity)) return null;
          bytes = new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: envelope.subarray(0, 12), additionalData: encoder.encode(record.id + ':' + identity) }, key, envelope.subarray(12)));
          if (!current(ticket, identity)) return null;
          const plain = JSON.parse(decoder.decode(bytes));
          if (plain?.v !== 1) throw Error('Unsupported encrypted text draft format.');
          record.revision = result.revision; record.expiresAt = Date.now() + ttl; persist(record);
          const drafts = normalize(plain.drafts);
          return Object.keys(drafts).length ? drafts : null;
        } catch (error) {
          if (current(ticket, identity)) { if (error.status === 404) { forget(); return null; } status(error.status === 401 ? 'Sign in to recover text drafts.' : 'Could not decrypt this tab’s text draft recovery copy.'); }
          return null;
        } finally { if (bytes) bytes.fill(0); }
      });
    }
    return {
      enabled: () => enabled,
      setEnabled(value) {
        enabled = !!value;
        try { localStorage.setItem(preferenceKey, String(enabled)); } catch (_) { /* Optional preference may be ephemeral. */ }
        if (!enabled) return clear();
        status('Encrypted text draft recovery is on. Images are not saved.'); return Promise.resolve();
      },
      update, recover, clear, consume: clear,
      expireAuth() { authenticated = false; invalidate(); },
      identityChanged() { invalidate(); authenticated = !!owner(); const record = stored(); if (record && owner() && record.owner !== owner()) forget(); }
    };
  } };
})();
