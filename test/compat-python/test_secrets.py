"""Secret Manager through the official google-cloud-secret-manager client,
over an explicit plaintext channel to CLOUDBURROW_SECRETMANAGER_ENDPOINT."""

import os

import grpc
from google.cloud import secretmanager
from google.cloud.secretmanager_v1.services.secret_manager_service.transports import SecretManagerServiceGrpcTransport

from conftest import channel_target


def secrets_client():
    channel = grpc.insecure_channel(channel_target(os.environ["CLOUDBURROW_SECRETMANAGER_ENDPOINT"]))
    return secretmanager.SecretManagerServiceClient(transport=SecretManagerServiceGrpcTransport(channel=channel))


def test_secret_version_add_and_access(project, suffix):
    client = secrets_client()
    secret = client.create_secret(parent=f"projects/{project}", secret_id=f"py-{suffix}",
                                  secret={"replication": {"automatic": {}}})
    try:
        client.add_secret_version(parent=secret.name, payload={"data": b"first"})
        client.add_secret_version(parent=secret.name, payload={"data": b"second"})
        latest = client.access_secret_version(name=f"{secret.name}/versions/latest")
        assert latest.payload.data == b"second"
        first = client.access_secret_version(name=f"{secret.name}/versions/1")
        assert first.payload.data == b"first"
    finally:
        client.delete_secret(name=secret.name)
