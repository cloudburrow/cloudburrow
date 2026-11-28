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


_FINGRAPH_DDL = [
    "CREATE TABLE Person (id INT64 NOT NULL, name STRING(MAX)) PRIMARY KEY (id)",
    "CREATE TABLE Account (id INT64 NOT NULL, is_blocked BOOL, nick_name STRING(MAX)) PRIMARY KEY (id)",
    "CREATE TABLE PersonOwnAccount (id INT64 NOT NULL, account_id INT64 NOT NULL, "
    "FOREIGN KEY (account_id) REFERENCES Account (id)) PRIMARY KEY (id, account_id), INTERLEAVE IN PARENT Person ON DELETE CASCADE",
    "CREATE TABLE AccountTransferAccount (id INT64 NOT NULL, to_id INT64 NOT NULL, amount FLOAT64, seq INT64 NOT NULL, "
    "FOREIGN KEY (to_id) REFERENCES Account (id)) PRIMARY KEY (id, to_id, seq), INTERLEAVE IN PARENT Account ON DELETE CASCADE",
    "CREATE PROPERTY GRAPH FinGraph NODE TABLES (Account, Person) EDGE TABLES ("
    "PersonOwnAccount SOURCE KEY (id) REFERENCES Person (id) DESTINATION KEY (account_id) REFERENCES Account (id) LABEL Owns, "
    "AccountTransferAccount SOURCE KEY (id) REFERENCES Account (id) DESTINATION KEY (to_id) REFERENCES Account (id) LABEL Transfers)",
    "CREATE PROPERTY GRAPH G2 NODE TABLES (Account LABEL Acct PROPERTIES (id, UPPER(nick_name) AS up, id * 2 AS dbl))",
]


def test_graph_query_plans_and_profiles(project, suffix):
    """#2029: PLAN and PROFILE of Spanner Graph queries through
    google-cloud-spanner's execute_sql, on Google's FinGraph rows: a
    quantified path pattern of two edges, a path mode on a part of a path,
    path functions grouped by their alias, a MATCH no element table matches
    (an Empty Relation), and properties a graph defines by expressions.
    PROFILE returns NORMAL's rows, PLAN's operators, each with execution
    statistics, and a root that counts the rows returned."""
    require("SPANNER_EMULATOR_HOST")
    from google.cloud.spanner_admin_instance_v1 import Instance
    from google.cloud.spanner_v1 import ExecuteSqlRequest

    spanner_helpers._cloud_region = "global"
    client = spanner.Client(project=project, disable_builtin_metrics=True)
    iid = f"pygraph-{suffix}"
    client.instance_admin_api.create_instance(
        parent=f"projects/{project}", instance_id=iid,
        instance=Instance(config=f"projects/{project}/instanceConfigs/emulator-config", display_name="CloudBurrow graph plans",
                          node_count=1, edition=Instance.Edition.ENTERPRISE)).result(timeout=60)
    instance = client.instance(iid)
    try:
        database = instance.database("fingraph", ddl_statements=_FINGRAPH_DDL)
        database.create().result(timeout=120)
        with database.batch() as batch:
            batch.insert(table="Account", columns=("id", "is_blocked", "nick_name"),
                         values=[(7, False, "Vacation Fund"), (16, True, "Vacation Fund"), (20, False, "Rainy Day Fund")])
            batch.insert(table="Person", columns=("id", "name"), values=[(1, "Alex"), (2, "Dana"), (3, "Lee")])
            batch.insert(table="AccountTransferAccount", columns=("id", "to_id", "amount", "seq"),
                         values=[(7, 16, 300.0, 1), (7, 16, 100.0, 2), (16, 20, 300.0, 3), (20, 7, 500.0, 4), (20, 16, 200.0, 5)])
            batch.insert(table="PersonOwnAccount", columns=("id", "account_id"), values=[(1, 7), (2, 20), (3, 16)])

        for sql, want, ops in (
            ("GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n",
             [[24]], {"Recursive Union", "Aggregate"}),
            ("GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) (ACYCLIC (b)-[u:Transfers]->(c:Account)-[w:Transfers]->(d:Account)) "
             "RETURN COUNT(*) AS n", [[7]], {"Cross Apply", "Aggregate"}),
            ("GRAPH FinGraph MATCH p=(a:Account)-[t:Transfers]->(b)-[u:Transfers]->(c) RETURN IS_ACYCLIC(p) AS ac, COUNT(*) AS n GROUP BY ac",
             [[False, 2], [True, 5]], {"Aggregate"}),
            ("GRAPH FinGraph MATCH (a:Account)-[o:Owns]->(b:Account) RETURN COUNT(*) AS n", [[0]], {"Empty Relation", "Aggregate"}),
            ("GRAPH G2 MATCH (a:Acct) WHERE a.dbl > 20 RETURN a.id AS i, a.up AS u",
             [[16, "VACATION FUND"], [20, "RAINY DAY FUND"]], {"Scan"}),
        ):
            with database.snapshot() as snap:
                normal = sorted(list(snap.execute_sql(sql)))
            assert normal == want, (sql, normal)
            with database.snapshot() as snap:
                res = snap.execute_sql(sql, query_mode=ExecuteSqlRequest.QueryMode.PLAN)
                assert list(res) == []
                plan = list(res.stats.query_plan.plan_nodes)
            assert plan and all(n.index == i for i, n in enumerate(plan)), sql
            assert not any(n.execution_stats for n in plan), sql
            assert ops <= {n.display_name for n in plan}, (sql, [n.display_name for n in plan])
            with database.snapshot() as snap:
                res = snap.execute_sql(sql, query_mode=ExecuteSqlRequest.QueryMode.PROFILE)
                rows = sorted(list(res))
                stats = res.stats
            assert rows == want, (sql, rows)
            pplan = list(stats.query_plan.plan_nodes)
            assert [(n.display_name, n.kind) for n in pplan] == [(n.display_name, n.kind) for n in plan], sql
            assert dict(pplan[0].execution_stats)["rows"]["total"] == str(len(want)), sql
            assert dict(stats.query_stats)["rows_returned"] == str(len(want)), sql
            PlanNode = type(pplan[0])
            for n in pplan:
                if n.kind == PlanNode.Kind.RELATIONAL:
                    assert "latency" in dict(n.execution_stats), (sql, n.display_name)
    finally:
        instance.delete()
