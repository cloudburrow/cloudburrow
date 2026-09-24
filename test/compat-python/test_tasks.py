"""Cloud Tasks through the official google-cloud-tasks client.

No Python client reads an emulator variable for Cloud Tasks, so the channel
is built explicitly: plaintext gRPC to CLOUDBURROW_TASKS_ENDPOINT. That is the
recipe docs/credentials.md gives.
"""

import os

import grpc
from google.api_core.exceptions import NotFound
from google.cloud import tasks_v2
from google.cloud.tasks_v2.services.cloud_tasks.transports import CloudTasksGrpcTransport

from conftest import channel_target


def tasks_client():
    channel = grpc.insecure_channel(channel_target(os.environ["CLOUDBURROW_TASKS_ENDPOINT"]))
    return tasks_v2.CloudTasksClient(transport=CloudTasksGrpcTransport(channel=channel))


def test_queue_and_task_create_get_delete(project, suffix):
    client = tasks_client()
    parent = f"projects/{project}/locations/us-central1"
    queue = client.create_queue(parent=parent, queue={"name": f"{parent}/queues/py-{suffix}"})
    try:
        assert client.get_queue(name=queue.name).name == queue.name
        task = client.create_task(parent=queue.name, task={
            "name": f"{queue.name}/tasks/py-task",
            "http_request": {"url": "http://127.0.0.1:9/never-called", "http_method": tasks_v2.HttpMethod.POST},
            # Far in the future, so the dispatcher leaves it alone.
            "schedule_time": {"seconds": 4102444800},
        })
        assert client.get_task(name=task.name).name == task.name
        client.delete_task(name=task.name)
        try:
            client.get_task(name=task.name)
            raise AssertionError("a deleted task was still returned")
        except NotFound:
            pass
    finally:
        client.delete_queue(name=queue.name)
