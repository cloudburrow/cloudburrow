// Pub/Sub through the official @google-cloud/pubsub client, configured by
// PUBSUB_EMULATOR_HOST alone: the client reads it and opens a plaintext
// channel with no credentials.

import assert from 'node:assert/strict';
import { once } from 'node:events';
import { test } from 'node:test';

import { PubSub, v1 } from '@google-cloud/pubsub';

import { skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('PUBSUB_EMULATOR_HOST');
const project = () => process.env.GOOGLE_CLOUD_PROJECT;

async function withSubscription(fn) {
  const pubsub = new PubSub({ projectId: project() });
  const s = suffix();
  const [topic] = await pubsub.createTopic(`node-topic-${s}`);
  const [subscription] = await topic.createSubscription(`node-sub-${s}`);
  try {
    await fn(pubsub, topic, subscription);
  } finally {
    await subscription.close();
    await subscription.delete();
    await topic.delete();
    await pubsub.close();
  }
}

test('publish, then receive and ack by streaming pull', { skip }, async () => {
  await withSubscription(async (_pubsub, topic, subscription) => {
    const id = await topic.publishMessage({ data: Buffer.from('ping'), attributes: { origin: 'node' } });
    assert.ok(id);
    const timeout = AbortSignal.timeout(30_000);
    const [message] = await Promise.race([
      once(subscription, 'message', { signal: timeout }),
      once(subscription, 'error', { signal: timeout }).then(([err]) => { throw err; }),
    ]);
    assert.equal(message.id, id);
    assert.equal(message.data.toString(), 'ping');
    assert.equal(message.attributes.origin, 'node');
    message.ack();
  });
});

test('publish, pull and acknowledge, with no redelivery after the ack', { skip }, async () => {
  await withSubscription(async (pubsub, topic, subscription) => {
    // The generated client the handwritten one wraps: a unary pull and an
    // acknowledge the test can wait for. It reads PUBSUB_EMULATOR_HOST the
    // same way, through the options the handwritten client derived from it.
    const raw = new v1.SubscriberClient({ projectId: project(), ...pubsub.options });
    try {
      const id = await topic.publishMessage({ data: Buffer.from('pong'), attributes: { origin: 'node' } });
      const [pulled] = await raw.pull({ subscription: subscription.name, maxMessages: 1 }, { timeout: 30_000 });
      assert.equal(pulled.receivedMessages.length, 1);
      const [received] = pulled.receivedMessages;
      assert.equal(received.message.messageId, id);
      assert.equal(Buffer.from(received.message.data).toString(), 'pong');
      assert.equal(received.message.attributes.origin, 'node');
      await raw.acknowledge({ subscription: subscription.name, ackIds: [received.ackId] });

      const [again] = await raw.pull({ subscription: subscription.name, maxMessages: 1, returnImmediately: true });
      assert.equal(again.receivedMessages.length, 0);
    } finally {
      await raw.close();
    }
  });
});
