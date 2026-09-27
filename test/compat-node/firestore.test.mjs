// Firestore through the official @google-cloud/firestore client, configured by
// FIRESTORE_EMULATOR_HOST alone: the client reads it, opens a plaintext
// channel and sends the emulator's "owner" token instead of credentials.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { Firestore } from '@google-cloud/firestore';

import { skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('FIRESTORE_EMULATOR_HOST');

async function withCollection(prefix, fn) {
  const db = new Firestore({ projectId: process.env.GOOGLE_CLOUD_PROJECT });
  const coll = db.collection(`node-${prefix}-${suffix()}`);
  try {
    await fn(db, coll);
  } finally {
    for (const doc of await coll.listDocuments()) await doc.delete();
    await db.terminate();
  }
}

test('Firestore document CRUD, a query and a transaction', { skip }, async () => {
  await withCollection('crud', async (db, coll) => {
    const one = coll.doc('one');
    await one.set({ size: 3, name: 'first' });
    assert.deepEqual((await one.get()).data(), { size: 3, name: 'first' });

    // A query, which is the part a document store must actually get right.
    await coll.doc('two').set({ size: 9, name: 'second' });
    const big = await coll.where('size', '>', 5).get();
    assert.deepEqual(big.docs.map((d) => d.get('name')), ['second']);

    await db.runTransaction(async (tx) => {
      const snap = await tx.get(one);
      tx.update(one, { size: snap.get('size') + 39 });
    });
    assert.equal((await one.get()).get('size'), 42);

    await one.delete();
    assert.equal((await one.get()).exists, false);
  });
});

test('Firestore batched write, then a snapshot listener sees it', { skip }, async () => {
  await withCollection('listen', async (db, coll) => {
    const batch = db.batch();
    batch.set(coll.doc('a'), { n: 1 });
    batch.set(coll.doc('b'), { n: 2 });
    await batch.commit();

    // onSnapshot is the Listen stream, which a unary-only fake would not serve.
    const seen = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => { stop(); reject(new Error('no snapshot within 30s')); }, 30_000);
      const stop = coll.orderBy('n').onSnapshot((snap) => {
        if (snap.size < 2) return;
        clearTimeout(timer);
        stop();
        resolve(snap.docs.map((d) => [d.id, d.get('n')]));
      }, (err) => { clearTimeout(timer); reject(err); });
    });
    assert.deepEqual(seen, [['a', 1], ['b', 2]]);
  });
});
