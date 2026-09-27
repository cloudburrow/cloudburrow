"""Spanner through the official google-cloud-spanner client, configured by
SPANNER_EMULATOR_HOST alone. Skipped where the instance does not run
Spanner."""

from google.cloud import spanner
from google.cloud.spanner_v1 import _helpers as spanner_helpers

from conftest import require


def test_instance_database_ddl_write_and_query(project, suffix):
    require("SPANNER_EMULATOR_HOST")
    # Built-in metrics are on by default, even against the emulator, and their
    # setup asks GCP's resource detector for the region, which looks up
    # metadata.google.internal directly: GCE_METADATA_HOST does not move it
    # (measured with google-cloud-spanner 3.71, caught by the socket guard).
    # disable_builtin_metrics, or SPANNER_DISABLE_BUILTIN_METRICS=true, is
    # what keeps the metrics setup local.
    # Tracing asks the same detector, whatever the metrics setting: the first
    # traced call runs _get_cloud_region, which has no switch, falls back to
    # "global" when the lookup fails, and caches that. Seeding the cache with
    # its own fallback keeps the lookup from happening (3.71; the socket guard
    # caught it in CI while every test passed).
    spanner_helpers._cloud_region = "global"
    client = spanner.Client(project=project, disable_builtin_metrics=True)
    instance = client.instance(f"py-{suffix}", configuration_name=f"projects/{project}/instanceConfigs/emulator-config",
                               display_name="CloudBurrow", node_count=1)
    instance.create().result(timeout=60)
    try:
        database = instance.database("pydb", ddl_statements=[
            "CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)",
        ])
        database.create().result(timeout=60)

        with database.batch() as batch:
            batch.insert(table="Widgets", columns=("Id", "Name"), values=[(1, "first"), (2, "second")])

        with database.snapshot() as snap:
            got = list(snap.read(table="Widgets", columns=("Name",), keyset=spanner.KeySet(keys=[[1]])))
        assert got == [["first"]]

        # DML in a read-write transaction, then a parameterised query.
        def rename(tx):
            return tx.execute_update("UPDATE Widgets SET Name = 'renamed' WHERE Id = 2")

        assert database.run_in_transaction(rename) == 1
        with database.snapshot() as snap:
            rows = list(snap.execute_sql("SELECT Id, Name FROM Widgets WHERE Id >= @min ORDER BY Id",
                                         params={"min": 1}, param_types={"min": spanner.param_types.INT64}))
        assert rows == [[1, "first"], [2, "renamed"]]
    finally:
        instance.delete()
