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


def _plan_well_formed(plan):
    """Every node is at its own index, every child link names a node, and the
    root is the relational Distributed Union Google's plans start with."""
    nodes = list(plan.plan_nodes)
    assert nodes, "the query plan has no nodes"
    for i, n in enumerate(nodes):
        assert n.index == i
        for link in n.child_links:
            assert 0 <= link.child_index < len(nodes)
    PlanNode = type(nodes[0])
    assert nodes[0].display_name == "Distributed Union"
    assert nodes[0].kind == PlanNode.Kind.RELATIONAL
    assert any(n.kind == PlanNode.Kind.SCALAR and n.short_representation.description for n in nodes)
    return nodes, PlanNode


def test_query_plan_and_profile(project, suffix):
    """#2029: PLAN and PROFILE through google-cloud-spanner's execute_sql
    (ExecuteStreamingSql). PLAN returns the query plan and no rows; PROFILE
    the rows, the same plan with each operator's execution statistics, and
    the query statistics. The plan is Google's for a seek on the key and for
    a full scan with a residual condition: a Filter Scan over a Table Scan,
    the scan's rows those it read."""
    require("SPANNER_EMULATOR_HOST")
    from google.cloud.spanner_v1 import ExecuteSqlRequest

    spanner_helpers._cloud_region = "global"
    client = spanner.Client(project=project, disable_builtin_metrics=True)
    instance = client.instance(f"pyplan-{suffix}", configuration_name=f"projects/{project}/instanceConfigs/emulator-config",
                               display_name="CloudBurrow plans", node_count=1)
    instance.create().result(timeout=60)
    try:
        database = instance.database("plandb", ddl_statements=[
            "CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)",
        ])
        database.create().result(timeout=60)
        with database.batch() as batch:
            batch.insert(table="Widgets", columns=("Id", "Name"), values=[(1, "first"), (2, "second"), (3, "third")])

        for sql, seek in (("SELECT Name FROM Widgets WHERE Id = 2", True),
                          ("SELECT Id FROM Widgets WHERE Name = 'third'", False)):
            with database.snapshot() as snap:
                res = snap.execute_sql(sql, query_mode=ExecuteSqlRequest.QueryMode.PLAN)
                assert list(res) == []
                nodes, PlanNode = _plan_well_formed(res.stats.query_plan)
            assert not any(n.execution_stats for n in nodes), "a PLAN node has execution statistics"
            scans = [n for n in nodes if n.display_name == "Scan"]
            assert len(scans) == 1
            meta = dict(scans[0].metadata)
            assert meta["scan_type"] == "TableScan" and meta["scan_target"] == "Widgets"
            assert ("Full scan" in meta) is (not seek), meta
            filt = [n for n in nodes if n.display_name == "Filter Scan"]
            assert len(filt) == 1
            types = {link.type for link in filt[0].child_links}
            assert ("Seek Condition" if seek else "Residual Condition") in types, types
            assert sum(1 for n in nodes if n.display_name == "Serialize Result") == 1

            with database.snapshot() as snap:
                res = snap.execute_sql(sql, query_mode=ExecuteSqlRequest.QueryMode.PROFILE)
                rows = list(res)
                stats = res.stats
            assert rows == ([["second"]] if seek else [[3]])
            pnodes, _ = _plan_well_formed(stats.query_plan)
            assert [(n.display_name, n.kind) for n in pnodes] == [(n.display_name, n.kind) for n in nodes]
            qs = dict(stats.query_stats)
            assert qs["rows_returned"] == "1", qs
            assert qs["elapsed_time"] and qs["cpu_time"], qs
            pscan = [n for n in pnodes if n.display_name == "Scan"][0]
            es = dict(pscan.execution_stats)
            # a seek reads only the matching row; a full scan reads every row,
            # and the Filter Scan's Residual Condition keeps one
            want = "1" if seek else "3"
            assert es["rows"]["total"] == want and es["scanned_rows"]["total"] == want, {k: dict(v) for k, v in es.items()}
            pfilt = [n for n in pnodes if n.display_name == "Filter Scan"][0]
            assert dict(pfilt.execution_stats)["rows"]["total"] == "1"
            assert qs["rows_scanned"] == want, qs
            for n in pnodes:
                if n.kind == PlanNode.Kind.RELATIONAL:
                    assert "latency" in dict(n.execution_stats), n.display_name
    finally:
        instance.delete()
