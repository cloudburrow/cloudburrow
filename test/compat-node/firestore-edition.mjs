// The Firebase Web SDK's Write stream on Standard and Enterprise databases
// (#1738, #1744), shared by firestore-edition.test.mjs (its Node build,
// over gRPC) and firestore-edition-webchannel.test.mjs (its browser build,
// over WebChannel). Each test file runs in a process of its own, as the two
// builds cannot share one: each registers itself as firebase/app's
// Firestore. Not a test file itself.

import assert from 'node:assert/strict';

import { deleteApp, initializeApp } from 'firebase/app';

import { suffix } from './guard.mjs';

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

function firestoreFor(sdk, settings, project, database) {
  const app = initializeApp({ projectId: project, apiKey: 'fake-api-key' }, `edition-${database}-${suffix()}`);
  const db = sdk.initializeFirestore(app, settings, database);
  const [host, port] = process.env.FIRESTORE_EMULATOR_HOST.split(':');
  sdk.connectFirestoreEmulator(db, host, Number(port));
  return { db, close: async () => { await sdk.terminate(db); await deleteApp(app); } };
}

const valuesOf = ({
  BsonObjectId, BsonTimestamp, Decimal128Value, Int32Value, MaxKey, MinKey, RegexValue,
}) => [
  ['int32', () => new Int32Value(7), 'Value type INT is not allowed'],
  ['objectId', () => new BsonObjectId('507f1f77bcf86cd799439011'), 'Value type OBJECT_ID is not allowed'],
  ['decimal128', () => new Decimal128Value('1.25'), 'Value type DECIMAL128 is not allowed'],
  ['bsonTimestamp', () => new BsonTimestamp(1700000000, 3), 'Value type REQUEST_TIMESTAMP is not allowed'],
  ['regex', () => new RegexValue('^a', 'i'), 'Value type REGEX is not allowed'],
  ['minKey', () => MinKey.instance(), 'Value type MIN_KEY is not allowed'],
  ['maxKey', () => MaxKey.instance(), 'Value type MAX_KEY is not allowed'],
  ['inMap', () => ({ a: new Int32Value(7) }), 'Value type INT is not allowed'],
];

// writeStream writes each value through sdk's Write stream: refused on
// (default), a Standard database, stored on an Enterprise one.
export async function writeStream(sdk, settings = {}) {
  const { doc, getDoc, setDoc } = sdk;
  const project = process.env.GOOGLE_CLOUD_PROJECT;
  const entId = `edition-fb-${suffix()}`.toLowerCase().replace(/[^a-z0-9-]/g, '-').slice(0, 60);
  const drop = await enterpriseDatabase(project, entId);
  const std = firestoreFor(sdk, settings, project, '(default)');
  const ent = firestoreFor(sdk, settings, project, entId);
  const coll = `fb-edition-${suffix()}`;
  try {
    for (const [name, make, msg] of valuesOf(sdk)) {
      await assert.rejects(setDoc(doc(std.db, coll, name), { v: make() }), (err) => {
        assert.equal(err.code, 'invalid-argument', `Standard ${name}: ${err}`);
        assert.match(err.message, new RegExp(msg), `Standard ${name}`);
        return true;
      });
      assert.equal((await getDoc(doc(std.db, coll, name))).exists(), false, `Standard ${name} was stored`);
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
}
