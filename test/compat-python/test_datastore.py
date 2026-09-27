"""Datastore through the official google-cloud-datastore client, configured
by DATASTORE_EMULATOR_HOST alone. Skipped where the instance does not run
Datastore."""

from google.cloud import datastore
from google.cloud.datastore.query import PropertyFilter

from conftest import require


def test_entity_crud_query_and_transaction(project, suffix):
    require("DATASTORE_EMULATOR_HOST")
    client = datastore.Client(project=project)
    kind = f"PyWidget-{suffix}"
    key = client.key(kind, "one")
    two = client.key(kind, "two")
    try:
        entity = datastore.Entity(key=key)
        entity.update({"name": "first", "size": 3})
        client.put(entity)
        assert client.get(key)["name"] == "first"

        other = datastore.Entity(key=two)
        other.update({"name": "second", "size": 9})
        client.put(other)
        # A non-ancestor query right after the write sees it (#371).
        got = [e["name"] for e in client.query(kind=kind, filters=[PropertyFilter("size", ">", 5)]).fetch()]
        assert got == ["second"]

        with client.transaction():
            current = client.get(key)
            current["size"] = 42
            client.put(current)
        assert client.get(key)["size"] == 42

        client.delete(key)
        assert client.get(key) is None
    finally:
        client.delete_multi([key, two])
