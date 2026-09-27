"""Firestore through the official google-cloud-firestore client, configured
by FIRESTORE_EMULATOR_HOST alone. Skipped where the instance does not run
Firestore."""

from google.cloud import firestore

from conftest import require


def test_document_crud_query_and_transaction(project, suffix):
    require("FIRESTORE_EMULATOR_HOST")
    client = firestore.Client(project=project)
    coll = client.collection(f"py-widgets-{suffix}")
    one = coll.document("one")
    try:
        one.set({"size": 3, "name": "first"})
        assert one.get().to_dict() == {"size": 3, "name": "first"}

        # A query, which is the part a document store must actually get right.
        coll.document("two").set({"size": 9, "name": "second"})
        got = [d.to_dict()["name"] for d in coll.where(filter=firestore.FieldFilter("size", ">", 5)).stream()]
        assert got == ["second"]

        @firestore.transactional
        def grow(tx, ref):
            snap = ref.get(transaction=tx)
            tx.update(ref, {"size": snap.get("size") + 39})

        grow(client.transaction(), one)
        assert one.get().get("size") == 42

        one.delete()
        assert not one.get().exists
    finally:
        for doc in coll.list_documents():
            doc.delete()
