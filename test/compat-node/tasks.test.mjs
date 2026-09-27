// Cloud Tasks through the official @google-cloud/tasks client, over an
// explicit plaintext channel to CLOUDBURROW_TASKS_ENDPOINT: no Node client
// reads an emulator variable for it.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { CloudTasksClient, protos } from '@google-cloud/tasks';
import { grpc } from 'google-gax';

import { grpcTarget, skipUnless, suffix } from './guard.mjs';

const skip = skipUnless('CLOUDBURROW_TASKS_ENDPOINT');

test('queue and task create, get and delete', { skip }, async () => {
  const client = new CloudTasksClient({
    ...grpcTarget(process.env.CLOUDBURROW_TASKS_ENDPOINT),
    sslCreds: grpc.credentials.createInsecure(),
  });
  const parent = `projects/${process.env.GOOGLE_CLOUD_PROJECT}/locations/us-central1`;
  const [queue] = await client.createQueue({ parent, queue: { name: `${parent}/queues/node-${suffix()}` } });
  try {
    const [gotQueue] = await client.getQueue({ name: queue.name });
    assert.equal(gotQueue.name, queue.name);
    const [task] = await client.createTask({
      parent: queue.name,
      task: {
        name: `${queue.name}/tasks/node-task`,
        httpRequest: {
          url: 'http://127.0.0.1:9/never-called',
          httpMethod: protos.google.cloud.tasks.v2.HttpMethod.POST,
        },
        // Far in the future, so the dispatcher leaves it alone.
        scheduleTime: { seconds: 4102444800 },
      },
    });
    const [gotTask] = await client.getTask({ name: task.name });
    assert.equal(gotTask.name, task.name);
    await client.deleteTask({ name: task.name });
    await assert.rejects(client.getTask({ name: task.name }), (err) => err.code === grpc.status.NOT_FOUND);
  } finally {
    await client.deleteQueue({ name: queue.name });
    await client.close();
  }
});
