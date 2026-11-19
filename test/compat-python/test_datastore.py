"""Datastore through the official google-cloud-datastore client, configured
by DATASTORE_EMULATOR_HOST alone. Skipped where the instance does not run
Datastore."""

from google.cloud import datastore
from google.cloud.datastore.query import And, Or, PropertyFilter

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


def test_not_equal_in_not_in_and_or(project, suffix):
    """!=, IN, NOT_IN and OR (#1888), which the emulator refuses and the
    Datastore front serves: merged in key order, ordered, paged at the
    cursor, and counted."""
    require("DATASTORE_EMULATOR_HOST")
    client = datastore.Client(project=project)
    kind = f"PyOps-{suffix}"
    keys = [client.key(kind, i) for i in range(1, 6)]
    try:
        for i, key in enumerate(keys, start=1):
            entity = datastore.Entity(key=key)
            entity.update({"n": i, "m": 10 - i})
            client.put(entity)

        def ids(query, **kw):
            return [e.key.id for e in query.fetch(**kw)]

        q = lambda *filters: client.query(kind=kind, filters=list(filters))
        assert ids(q(PropertyFilter("n", "!=", 2))) == [1, 3, 4, 5]
        assert ids(q(PropertyFilter("n", "IN", [4, 1]))) == [1, 4]
        assert ids(q(PropertyFilter("n", "NOT_IN", [1, 5]))) == [2, 3, 4]
        assert ids(q(Or([PropertyFilter("n", "=", 1), PropertyFilter("m", "<", 7)]))) == [1, 4, 5]
        assert ids(q(And([PropertyFilter("m", ">", 6), Or([PropertyFilter("n", "=", 2), PropertyFilter("n", "=", 4)])]))) == [2]

        ordered = q(PropertyFilter("n", "!=", 3))
        ordered.order = ["-n"]
        it = ordered.fetch(limit=2)
        page = next(it.pages)
        assert [e.key.id for e in page] == [5, 4]
        assert ids(ordered, start_cursor=it.next_page_token) == [2, 1]

        count = client.aggregation_query(q(PropertyFilter("n", "IN", [1, 2, 3]))).count(alias="n")
        assert [r.value for r in next(iter(count.fetch()))] == [3]
    finally:
        client.delete_multi(keys)
