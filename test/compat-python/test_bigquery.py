"""BigQuery through the official google-cloud-bigquery client.

No client library reads an emulator variable for BigQuery, so the endpoint
is passed as client_options api_endpoint, from CLOUDBURROW_BIGQUERY_ENDPOINT,
with the instance's own project: the emulator serves that one project only.
It is REST, so every connection is under the socket guard. Skipped where the
instance does not run BigQuery.
"""

import os
import time

import pytest
from google.api_core import exceptions
from google.auth.credentials import AnonymousCredentials
from google.cloud import bigquery

from conftest import require


def test_dataset_table_insert_and_query(suffix):
    endpoint = require("CLOUDBURROW_BIGQUERY_ENDPOINT")
    project = os.environ["GOOGLE_CLOUD_PROJECT"]
    client = bigquery.Client(project=project, credentials=AnonymousCredentials(),
                             client_options={"api_endpoint": endpoint})
    dataset = client.create_dataset(f"py_{suffix.replace('-', '_')}")
    try:
        assert client.get_dataset(dataset.dataset_id).dataset_id == dataset.dataset_id

        table = client.create_table(bigquery.Table(f"{project}.{dataset.dataset_id}.orders", schema=[
            bigquery.SchemaField("id", "INTEGER"),
            bigquery.SchemaField("region", "STRING"),
            bigquery.SchemaField("amount", "FLOAT"),
        ]))
        got = client.get_table(table)
        assert [(f.name, f.field_type) for f in got.schema] == [("id", "INTEGER"), ("region", "STRING"), ("amount", "FLOAT")]

        errors = client.insert_rows_json(table, [
            {"id": 1, "region": "eu", "amount": 10},
            {"id": 2, "region": "eu", "amount": 5.5},
            {"id": 3, "region": "us", "amount": 7},
            {"id": 4, "region": "us", "amount": 1},
        ])
        assert errors == []

        rows = client.query(
            f"SELECT region, SUM(amount) AS total, COUNT(*) AS n FROM `{project}.{dataset.dataset_id}.orders` "
            "WHERE amount > 2 GROUP BY region ORDER BY region").result()
        assert [(r["region"], r["total"], r["n"]) for r in rows] == [("eu", 15.5, 2), ("us", 7.0, 1)]

        client.delete_table(table)
        with pytest.raises(exceptions.NotFound):
            client.get_table(table)
    finally:
        client.delete_dataset(dataset, delete_contents=True, not_found_ok=True)
    with pytest.raises(exceptions.NotFound):
        client.get_dataset(dataset.dataset_id)


# A query that takes the engine some seconds: a COUNT of a cross join of
# three arrays, SLOW_N**3 rows.
SLOW_N = 380
SLOW_SQL = ("SELECT COUNT(*) AS n FROM UNNEST(GENERATE_ARRAY(1, {n})) a, UNNEST(GENERATE_ARRAY(1, {n})) b, "
            "UNNEST(GENERATE_ARRAY(1, {n})) c").format(n=SLOW_N)


def test_async_jobs_poll_and_cancel(suffix):
    """#1819: a job outlives the request that started it, as on BigQuery.

    jobs.query with a short timeoutMs answers jobComplete false with the job's
    jobReference, which "will be present even if the original request timed
    out" (https://cloud.google.com/bigquery/docs/reference/rest/v2/jobs/query);
    the client reads the job RUNNING, and QueryJob.result polls
    jobs.getQueryResults with timeoutMs until its rows. client.query
    (jobs.insert) answers the job RUNNING, and client.cancel_job ends it DONE
    with reason "stopped" (https://cloud.google.com/bigquery/docs/error-messages).
    """
    endpoint = require("CLOUDBURROW_BIGQUERY_ENDPOINT")
    project = os.environ["GOOGLE_CLOUD_PROJECT"]
    client = bigquery.Client(project=project, credentials=AnonymousCredentials(),
                             client_options={"api_endpoint": endpoint})
    want = SLOW_N ** 3

    # jobs.query, through the client's own connection, with a timeoutMs the
    # query outlasts.
    resp = client._connection.api_request(method="POST", path=f"/projects/{project}/queries",
                                          data={"query": SLOW_SQL, "useLegacySql": False, "timeoutMs": 300})
    assert resp["jobComplete"] is False
    assert "rows" not in resp and "totalRows" not in resp
    ref = resp["jobReference"]
    job = client.get_job(ref["jobId"], location=ref["location"])
    assert job.state == "RUNNING"
    assert job.done() is False
    # QueryJob.result polls jobs.getQueryResults (with timeoutMs) to the end.
    rows = list(job.result(timeout=120))
    assert [r["n"] for r in rows] == [want]
    assert client.get_job(ref["jobId"], location=ref["location"]).state == "DONE"

    # jobs.insert answers the job RUNNING.
    job = client.query(SLOW_SQL)
    assert job.state == "RUNNING"
    assert [r["n"] for r in job.result(timeout=120)] == [want]
    assert job.state == "DONE" and job.error_result is None

    # jobs.cancel ends a running job, "stopped".
    job = client.query(SLOW_SQL)
    assert job.state == "RUNNING"
    cancelled = client.cancel_job(job.job_id, location=job.location)
    assert cancelled.job_id == job.job_id
    deadline = time.time() + 60
    while not job.done():
        assert time.time() < deadline, "the cancelled job never ended"
        time.sleep(0.2)
    assert job.state == "DONE"
    assert job.error_result["reason"] == "stopped"
    with pytest.raises(exceptions.GoogleAPICallError):
        job.result(timeout=30)
