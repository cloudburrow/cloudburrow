// Secret Manager through the official @google-cloud/secret-manager client,
// over an explicit plaintext channel to CLOUDBURROW_SECRETMANAGER_ENDPOINT:
// no Node client reads an emulator variable for it.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { SecretManagerServiceClient } from '@google-cloud/secret-manager';
import { grpc } from 'google-gax';

import { grpcTarget, skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('CLOUDBURROW_SECRETMANAGER_ENDPOINT');

test('secret create, version add, access by number and latest', { skip }, async () => {
  const client = new SecretManagerServiceClient({
    ...grpcTarget(process.env.CLOUDBURROW_SECRETMANAGER_ENDPOINT),
    sslCreds: grpc.credentials.createInsecure(),
  });
  const [secret] = await client.createSecret({
    parent: `projects/${process.env.GOOGLE_CLOUD_PROJECT}`,
    secretId: `node-${suffix()}`,
    secret: { replication: { automatic: {} } },
  });
  try {
    await client.addSecretVersion({ parent: secret.name, payload: { data: Buffer.from('first') } });
    await client.addSecretVersion({ parent: secret.name, payload: { data: Buffer.from('second') } });
    const [latest] = await client.accessSecretVersion({ name: `${secret.name}/versions/latest` });
    assert.equal(Buffer.from(latest.payload.data).toString(), 'second');
    const [first] = await client.accessSecretVersion({ name: `${secret.name}/versions/1` });
    assert.equal(Buffer.from(first.payload.data).toString(), 'first');
  } finally {
    await client.deleteSecret({ name: secret.name });
    await client.close();
  }
});
