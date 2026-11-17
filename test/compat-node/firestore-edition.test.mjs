// The Write stream of the Firebase client SDK (#1738). The Firebase Web SDK
// (`firebase`, its Node build) writes through Firestore's Write stream, not
// Commit. Google's "Supported data types"
// (https://docs.cloud.google.com/firestore/docs/concepts/data-types) marks
// the BSON types "Supported only in Enterprise edition", and says a
// Standard database's array cannot hold an array: on `(default)`, a
// Standard database, each write is refused invalid-argument, as Commit
// refuses it (TestFirestoreEnterpriseOnlyValues), while an Enterprise
// database stores each and reads it back through the same SDK.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { deleteApp, initializeApp } from 'firebase/app';
import {
  BsonObjectId, BsonTimestamp, Decimal128Value, Int32Value, MaxKey, MinKey, RegexValue,
  connectFirestoreEmulator, doc, getDoc, getFirestore, setDoc, terminate,
} from 'firebase/firestore';

import { skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('FIRESTORE_EMULATOR_HOST');

// An Enterprise edition database, through the Admin API's REST surface
// the front serves; deleted when done.
async function enterpriseDatabase(project, id) {
  const base = `http://${process.env.FIRESTORE_EMULATOR_HOST}/v1/projects/${project}/databases`;
  const headers = { 'content-type': 'application/json', authorization: 'Bearer owner' };
  const res = await fetch(`${base}?databaseId=${id}`, {
    method: 'POST', headers,
    body: JSON.stringify({ locationId: 'nam5', type: 'FIRESTORE_NATIVE', databaseEdition: 'ENTERPRISE' }),
  });
  assert.equal(res.status, 200, `CreateDatabase: ${await res.text()}`);
  return async () => { await fetch(`${base}/${id}`, { method: 'DELETE', headers }); };
}

function firestoreFor(project, database) {
  const app = initializeApp({ projectId: project, apiKey: 'fake-api-key' }, `edition-${database}-${suffix()}`);
  const db = getFirestore(app, database);
  const [host, port] = process.env.FIRESTORE_EMULATOR_HOST.split(':');
  connectFirestoreEmulator(db, host, Number(port));
  return { db, close: async () => { await terminate(db); await deleteApp(app); } };
}

const values = [
  ['int32', () => new Int32Value(7), 'Value type INT is not allowed'],
  ['objectId', () => new BsonObjectId('507f1f77bcf86cd799439011'), 'Value type OBJECT_ID is not allowed'],
  ['decimal128', () => new Decimal128Value('1.25'), 'Value type DECIMAL128 is not allowed'],
  ['bsonTimestamp', () => new BsonTimestamp(1700000000, 3), 'Value type REQUEST_TIMESTAMP is not allowed'],
  ['regex', () => new RegexValue('^a', 'i'), 'Value type REGEX is not allowed'],
  ['minKey', () => MinKey.instance(), 'Value type MIN_KEY is not allowed'],
  ['maxKey', () => MaxKey.instance(), 'Value type MAX_KEY is not allowed'],
  ['inMap', () => ({ a: new Int32Value(7) }), 'Value type INT is not allowed'],
];

test('Firebase SDK Write stream: Standard refuses Enterprise-only values, Enterprise stores them', { skip }, async () => {
  const project = process.env.GOOGLE_CLOUD_PROJECT;
  const entId = `edition-fb-${suffix()}`.toLowerCase().replace(/[^a-z0-9-]/g, '-').slice(0, 60);
  const drop = await enterpriseDatabase(project, entId);
  const std = firestoreFor(project, '(default)');
  const ent = firestoreFor(project, entId);
  const coll = `fb-edition-${suffix()}`;
  try {
    for (const [name, make, msg] of values) {
      await assert.rejects(setDoc(doc(std.db, coll, name), { v: make() }), (err) => {
        assert.equal(err.code, 'invalid-argument', `Standard ${name}: ${err}`);
        assert.match(err.message, new RegExp(msg), `Standard ${name}`);
        return true;
      });
      const ref = doc(ent.db, coll, name);
      await setDoc(ref, { v: make() });
      const snap = await getDoc(ref);
      assert.ok(snap.exists(), `Enterprise ${name} was not stored`);
      assert.deepEqual(JSON.stringify(snap.get('v')), JSON.stringify(make()), `Enterprise ${name} read back`);
    }
    // A value Standard has is written as ever on the same stream.
    const ok = doc(std.db, coll, 'plain');
    await setDoc(ok, { v: 7, a: [1, 2] });
    assert.deepEqual((await getDoc(ok)).data(), { v: 7, a: [1, 2] });
  } finally {
    await std.close();
    await ent.close();
    await drop();
  }
});
