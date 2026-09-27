# Node.js

Every block on this page is run by the Node.js compatibility suite
(`test/compat-node/examples.test.mjs`), against a live instance, with only
what `eval "$(cloudburrow env)"` exports. If a block here stops working, CI
fails.

The client versions are those pinned in
[`test/compat-node/package-lock.json`](../../test/compat-node/package-lock.json).
Each block is an ES module (`.mjs`, or `"type": "module"`), and uses top-level
`await`.

## Cloud Storage

The Node client cannot use `STORAGE_EMULATOR_HOST` as `cloudburrow env`
exports it. It takes the variable verbatim as the JSON API's base URL, which
would then need `/storage/v1`, and as the upload URL, which must not have it.
Pass the value as `apiEndpoint` instead and take the variable out of the
environment, where the client would still read it. With a custom endpoint the
client sends no credentials.

Pass `resumable: false` to `save()` (or give `createWriteStream()` a
`chunkSize`). The client's default for both is a resumable upload sent as one
streamed request with `Content-Range: bytes 0-*/*`, and the builtin server
refuses that range with 400 `Invalid Content-Range` (see
[the Node.js section of compatibility.md](../compatibility.md#nodejs-client-libraries)).

```js
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { Storage } from '@google-cloud/storage';

const apiEndpoint = process.env.STORAGE_EMULATOR_HOST;
delete process.env.STORAGE_EMULATOR_HOST;
const storage = new Storage({ apiEndpoint, projectId: process.env.GOOGLE_CLOUD_PROJECT });

const [bucket] = await storage.createBucket(`example-${randomUUID()}`);
await bucket.file('hello.txt').save('hello', { resumable: false });
const [data] = await bucket.file('hello.txt').download();
assert.equal(data.toString(), 'hello');
await bucket.deleteFiles({ force: true });
await bucket.delete();
```

## Pub/Sub

`PUBSUB_EMULATOR_HOST` is all the client needs: it opens a plaintext channel
with no credentials.

```js
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { once } from 'node:events';
import { PubSub } from '@google-cloud/pubsub';

const pubsub = new PubSub({ projectId: process.env.GOOGLE_CLOUD_PROJECT });
const [topic] = await pubsub.createTopic(`example-${randomUUID()}`);
const [subscription] = await topic.createSubscription(`example-${randomUUID()}`);

await topic.publishMessage({ data: Buffer.from('hello') });
const [message] = await once(subscription, 'message', { signal: AbortSignal.timeout(30_000) });
assert.equal(message.data.toString(), 'hello');
message.ack();

await subscription.close();
await subscription.delete();
await topic.delete();
await pubsub.close();
```

## Cloud Tasks

No Node client reads an emulator variable for Cloud Tasks. Pass the host and
port of `CLOUDBURROW_TASKS_ENDPOINT` as `apiEndpoint` and `port`, with
plaintext credentials from `google-gax`, the gRPC layer every generated client
is built on.

```js
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { CloudTasksClient } from '@google-cloud/tasks';
import { grpc } from 'google-gax';

const endpoint = new URL(`http://${process.env.CLOUDBURROW_TASKS_ENDPOINT}`);
const client = new CloudTasksClient({
  apiEndpoint: endpoint.hostname,
  port: Number(endpoint.port),
  sslCreds: grpc.credentials.createInsecure(),
});

const parent = `projects/${process.env.GOOGLE_CLOUD_PROJECT}/locations/us-central1`;
const [queue] = await client.createQueue({ parent, queue: { name: `${parent}/queues/example-${randomUUID()}` } });
const [got] = await client.getQueue({ name: queue.name });
assert.equal(got.name, queue.name);
await client.deleteQueue({ name: queue.name });
await client.close();
```

## Secret Manager

The same, with `CLOUDBURROW_SECRETMANAGER_ENDPOINT`.

```js
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { SecretManagerServiceClient } from '@google-cloud/secret-manager';
import { grpc } from 'google-gax';

const endpoint = new URL(`http://${process.env.CLOUDBURROW_SECRETMANAGER_ENDPOINT}`);
const client = new SecretManagerServiceClient({
  apiEndpoint: endpoint.hostname,
  port: Number(endpoint.port),
  sslCreds: grpc.credentials.createInsecure(),
});

const [secret] = await client.createSecret({
  parent: `projects/${process.env.GOOGLE_CLOUD_PROJECT}`,
  secretId: `example-${randomUUID()}`,
  secret: { replication: { automatic: {} } },
});
await client.addSecretVersion({ parent: secret.name, payload: { data: Buffer.from('s3cret') } });
const [version] = await client.accessSecretVersion({ name: `${secret.name}/versions/latest` });
assert.equal(Buffer.from(version.payload.data).toString(), 's3cret');
await client.deleteSecret({ name: secret.name });
await client.close();
```

## Firestore

`FIRESTORE_EMULATOR_HOST` is all the client needs: it opens a plaintext
channel and sends the emulator's `owner` token instead of credentials.

```js
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { Firestore } from '@google-cloud/firestore';

const db = new Firestore({ projectId: process.env.GOOGLE_CLOUD_PROJECT });
const doc = db.collection(`example-${randomUUID()}`).doc('hello');
await doc.set({ greeting: 'hello' });
assert.equal((await doc.get()).get('greeting'), 'hello');
await doc.delete();
await db.terminate();
```
