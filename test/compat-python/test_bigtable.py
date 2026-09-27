"""Bigtable through the official google-cloud-bigtable client, configured by
BIGTABLE_EMULATOR_HOST alone. Skipped where the instance does not run
Bigtable."""

from google.cloud import bigtable
from google.cloud.bigtable import column_family, row_filters
from google.cloud.bigtable.row_set import RowSet

from conftest import require


def test_table_and_rows_with_a_filtered_read(project, suffix):
    require("BIGTABLE_EMULATOR_HOST")
    client = bigtable.Client(project=project, admin=True)
    # The emulator has no instance resource: any instance ID is served.
    table = client.instance("cb-instance").table(f"py-widgets-{suffix}")
    table.create(column_families={"info": column_family.MaxVersionsGCRule(1)})
    try:
        assert table.exists()
        assert list(table.list_column_families()) == ["info"]

        row = table.direct_row(b"row-1")
        row.set_cell("info", b"name", b"first")
        row.commit()
        other = table.direct_row(b"other-1")
        other.set_cell("info", b"name", b"elsewhere")
        other.commit()

        got = table.read_row(b"row-1")
        assert got.cells["info"][b"name"][0].value == b"first"

        # A prefix scan with a family filter, the read Bigtable users rely on.
        rows = RowSet()
        rows.add_row_range_with_prefix("row-")
        seen = [r.row_key for r in table.read_rows(row_set=rows, filter_=row_filters.FamilyNameRegexFilter("info"))]
        assert seen == [b"row-1"]
    finally:
        table.delete()
