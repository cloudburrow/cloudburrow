# Python

Every block on this page is run by the Python compatibility suite
(`test/compat-python/test_examples.py`), against a live instance, with only
what `eval "$(cloudburrow env)"` exports. If a block here stops working, CI
fails.

The client versions are those pinned in
[`test/compat-python/requirements.lock`](../../test/compat-python/requirements.lock).

## Cloud Storage

`STORAGE_EMULATOR_HOST` is all the client needs. Python needs the scheme in it,
which `cloudburrow env` includes.

```python
import uuid
from google.cloud import storage

client = storage.Client()
bucket = client.create_bucket(f"example-{uuid.uuid4().hex[:12]}")
bucket.blob("hello.txt").upload_from_string("hello")
assert bucket.blob("hello.txt").download_as_text() == "hello"
bucket.delete(force=True)
```

## Pub/Sub

`PUBSUB_EMULATOR_HOST` is all the client needs.

```python
import os, uuid
from google.cloud import pubsub_v1

project = os.environ["GOOGLE_CLOUD_PROJECT"]
publisher, subscriber = pubsub_v1.PublisherClient(), pubsub_v1.SubscriberClient()
topic = publisher.topic_path(project, f"example-{uuid.uuid4().hex[:12]}")
sub = subscriber.subscription_path(project, f"example-{uuid.uuid4().hex[:12]}")
publisher.create_topic(name=topic)
subscriber.create_subscription(name=sub, topic=topic)

publisher.publish(topic, b"hello").result(timeout=30)
messages = subscriber.pull(subscription=sub, max_messages=1, timeout=30).received_messages
assert messages[0].message.data == b"hello"
subscriber.acknowledge(subscription=sub, ack_ids=[messages[0].ack_id])

subscriber.delete_subscription(subscription=sub)
publisher.delete_topic(topic=topic)
```

## Cloud Tasks

No Python client reads an emulator variable for Cloud Tasks. Give it a
plaintext gRPC channel to `CLOUDBURROW_TASKS_ENDPOINT`. `client_options`
alone is not enough, since the channel it builds uses TLS.

```python
import os, uuid, grpc
from google.cloud import tasks_v2
from google.cloud.tasks_v2.services.cloud_tasks.transports import CloudTasksGrpcTransport

channel = grpc.insecure_channel(os.environ["CLOUDBURROW_TASKS_ENDPOINT"])
client = tasks_v2.CloudTasksClient(transport=CloudTasksGrpcTransport(channel=channel))

parent = f"projects/{os.environ['GOOGLE_CLOUD_PROJECT']}/locations/us-central1"
queue = client.create_queue(parent=parent, queue={"name": f"{parent}/queues/example-{uuid.uuid4().hex[:12]}"})
assert client.get_queue(name=queue.name).name == queue.name
client.delete_queue(name=queue.name)
```

## Secret Manager

The same, with `CLOUDBURROW_SECRETMANAGER_ENDPOINT`.

```python
import os, uuid, grpc
from google.cloud import secretmanager
from google.cloud.secretmanager_v1.services.secret_manager_service.transports import SecretManagerServiceGrpcTransport

channel = grpc.insecure_channel(os.environ["CLOUDBURROW_SECRETMANAGER_ENDPOINT"])
client = secretmanager.SecretManagerServiceClient(transport=SecretManagerServiceGrpcTransport(channel=channel))

secret = client.create_secret(parent=f"projects/{os.environ['GOOGLE_CLOUD_PROJECT']}",
                              secret_id=f"example-{uuid.uuid4().hex[:12]}",
                              secret={"replication": {"automatic": {}}})
client.add_secret_version(parent=secret.name, payload={"data": b"s3cret"})
assert client.access_secret_version(name=f"{secret.name}/versions/latest").payload.data == b"s3cret"
client.delete_secret(name=secret.name)
```
