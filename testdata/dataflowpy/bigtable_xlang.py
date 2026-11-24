"""Python's cross-language Bigtable read for TestPythonDataflowBigtableRead
(#2215).

A batch job reads a Bigtable table with the Python SDK's ReadFromBigtable,
a Java transform (BigtableIO's read schema transform) expanded by Beam's
Java expansion service for Google Cloud I/O, and writes each row as
"KEY FAMILY:QUALIFIER=VALUE ..." under --output. The job runs the read in
the Java SDK's harness and the rest in Python's.
"""
import argparse
import logging

import apache_beam as beam
from apache_beam.io.gcp.bigtableio import ReadFromBigtable
from apache_beam.options.pipeline_options import PipelineOptions


def fmt(row):
    """row is the read's google.cloud.bigtable.row.PartialRowData: its
    row_key and its cells, by family and qualifier (bytes)."""
    parts = [row.row_key.decode()]
    for family in sorted(row.cells):
        for qualifier in sorted(row.cells[family]):
            q = qualifier.decode() if isinstance(qualifier, bytes) else qualifier
            for cell in row.cells[family][qualifier]:
                parts.append("%s:%s=%s" % (family, q, cell.value.decode()))
    return " ".join(parts)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--bigtable_project", required=True)
    ap.add_argument("--bigtable_instance", required=True)
    ap.add_argument("--bigtable_table", required=True)
    ap.add_argument("--expansion_service", required=True)
    ap.add_argument("--output", required=True)
    args, rest = ap.parse_known_args()
    with beam.Pipeline(options=PipelineOptions(rest)) as p:
        _ = (p
             | "ReadBigtable" >> ReadFromBigtable(table_id=args.bigtable_table, instance_id=args.bigtable_instance,
                                                  project_id=args.bigtable_project,
                                                  expansion_service=args.expansion_service)
             | "Format" >> beam.Map(fmt)
             | "Write" >> beam.io.WriteToText(args.output, shard_name_template=""))


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    main()
