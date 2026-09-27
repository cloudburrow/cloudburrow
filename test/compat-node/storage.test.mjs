// Cloud Storage through the official @google-cloud/storage client, pointed at
// STORAGE_EMULATOR_HOST by apiEndpoint.
//
// The variable itself does not work with the Node client as `cloudburrow env`
// exports it. The client uses it verbatim as the JSON API's base URL
// (storage.js: `baseUrl = EMULATOR_HOST || ...`), which would then need
// /storage/v1, and also as the upload endpoint, which must not have it. So,
// as the client's own comment advises ("Use apiEndpoint instead"), the test
// passes it as apiEndpoint and takes it out of the environment, where the
// client would otherwise still read it.

import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

import { Storage } from '@google-cloud/storage';

import { skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('STORAGE_EMULATOR_HOST');
const endpoint = process.env.STORAGE_EMULATOR_HOST;
delete process.env.STORAGE_EMULATOR_HOST;

function client(options = {}) {
  return new Storage({ projectId: process.env.GOOGLE_CLOUD_PROJECT, apiEndpoint: endpoint, ...options });
}

async function withBucket(prefix, fn) {
  const storage = client();
  const [bucket] = await storage.createBucket(`node-${prefix}-${suffix()}`);
  try {
    await fn(storage, bucket);
  } finally {
    await bucket.deleteFiles({ force: true, versions: true });
    await bucket.delete();
  }
  const [exists] = await bucket.exists();
  assert.equal(exists, false);
}

test('bucket and object CRUD with a resumable upload', { skip }, async () => {
  await withBucket('crud', async (storage, bucket) => {
    const [meta] = await storage.bucket(bucket.name).getMetadata();
    assert.equal(meta.name, bucket.name);
    const [buckets] = await storage.getBuckets();
    assert.ok(buckets.some((b) => b.name === bucket.name));

    await bucket.file('small.txt').save('hello from node', { resumable: false, contentType: 'text/plain' });
    const [small] = await bucket.file('small.txt').download();
    assert.equal(small.toString(), 'hello from node');

    // resumable: true with a chunk size forces the resumable protocol in
    // several requests, which a small object would otherwise never use.
    const data = randomBytes(512 * 1024 + 123);
    await bucket.file('big.bin').save(data, { resumable: true, chunkSize: 256 * 1024 });
    const [big] = await bucket.file('big.bin').download();
    assert.ok(big.equals(data));

    const [files] = await bucket.getFiles();
    assert.deepEqual(files.map((f) => f.name).sort(), ['big.bin', 'small.txt']);

    await bucket.file('small.txt').delete();
    const [exists] = await bucket.file('small.txt').exists();
    assert.equal(exists, false);
  });
});

test('mediaLink names the emulator host', { skip }, async () => {
  await withBucket('link', async (_storage, bucket) => {
    await bucket.file('linked.txt').save('linked', { resumable: false });
    const [meta] = await bucket.file('linked.txt').getMetadata();
    assert.equal(new URL(meta.mediaLink).host, new URL(endpoint).host);
    const [got] = await bucket.file('linked.txt').download();
    assert.equal(got.toString(), 'linked');
  });
});

test('ifGenerationMatch 0 creates once, and a ranged read is inclusive', { skip }, async () => {
  await withBucket('gen0', async (_storage, bucket) => {
    const file = bucket.file('once.txt');
    await file.save('0123456789', { resumable: false, preconditionOpts: { ifGenerationMatch: 0 } });
    await assert.rejects(
      bucket.file('once.txt').save('again', { resumable: false, preconditionOpts: { ifGenerationMatch: 0 } }),
      (err) => err.code === 412,
    );
    const [ranged] = await file.download({ start: 2, end: 5 });
    assert.equal(ranged.toString(), '2345');
    const [meta] = await file.getMetadata();
    assert.equal(meta.size, '10');
    assert.ok(Number(meta.generation) > 0);
  });
});

// The key and account to sign with: the ones a standalone server was given,
// or else the ADC fixture, whose key `cloudburrow up` registers (#577).
function signingIdentity() {
  if (process.env.CLOUDBURROW_TEST_SIGNING_KEY) {
    return {
      private_key: readFileSync(process.env.CLOUDBURROW_TEST_SIGNING_KEY, 'utf8'),
      client_email: process.env.CLOUDBURROW_TEST_SIGNING_EMAIL,
    };
  }
  const fixture = JSON.parse(readFileSync(process.env.GOOGLE_APPLICATION_CREDENTIALS, 'utf8'));
  return { private_key: fixture.private_key, client_email: fixture.client_email };
}

test('a V4 signed URL reads the object, and a tampered one is 403', { skip }, async () => {
  const signer = client({ credentials: signingIdentity() });
  await withBucket('signed', async (_storage, bucket) => {
    await bucket.file('secret.txt').save('classified', { resumable: false });
    const [url] = await signer.bucket(bucket.name).file('secret.txt').getSignedUrl({
      version: 'v4',
      action: 'read',
      expires: Date.now() + 5 * 60 * 1000,
    });
    assert.equal(new URL(url).host, new URL(endpoint).host);
    const ok = await fetch(url);
    assert.equal(ok.status, 200);
    assert.equal(await ok.text(), 'classified');
    const bad = await fetch(url.replace('X-Goog-Signature=', 'X-Goog-Signature=00'));
    assert.equal(bad.status, 403);
  });
});
