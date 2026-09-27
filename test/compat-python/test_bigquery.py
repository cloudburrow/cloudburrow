"""BigQuery through the official google-cloud-bigquery client.

No client library reads an emulator variable for BigQuery, so the endpoint
is passed as client_options api_endpoint, from CLOUDBURROW_BIGQUERY_ENDPOINT,
with the instance's own project: the emulator serves that one project only.
It is REST, so every connection is under the socket guard. Skipped where the
instance does not run BigQuery.
"""

import os

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
