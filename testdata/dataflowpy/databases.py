"""Google's Datastore, Bigtable and Spanner connectors for
TestPythonDataflowDatabases (#2215).

A batch job counts a few words and writes each count three ways, with the
Python SDK's own connectors against the instance's emulators (reached
through DATASTORE_EMULATOR_HOST, BIGTABLE_EMULATOR_HOST and
SPANNER_EMULATOR_HOST on the job's network): a Datastore entity Word/<word>
with property n, a Bigtable row <word> with cell cf:n, and a Spanner row of
table Words (Word, N). With --read_to it is a second job that reads the
Datastore kind and the Spanner table back with ReadFromDatastore and
ReadFromSpanner and writes what it read under that Cloud Storage prefix.
"""
import argparse
import logging

import apache_beam as beam
from apache_beam.io.gcp.bigtableio import WriteToBigTable
from apache_beam.io.gcp.datastore.v1new.datastoreio import ReadFromDatastore, WriteToDatastore
from apache_beam.io.gcp.datastore.v1new.types import Entity, Key, Query
from apache_beam.io.gcp.experimental.spannerio import ReadFromSpanner, WriteMutation, WriteToSpanner
from apache_beam.options.pipeline_options import PipelineOptions


class ToEntity(beam.DoFn):
    def __init__(self, project):
        self.project = project

    def process(self, kv):
        word, n = kv
        e = Entity(Key(["Word", word], project=self.project))
        e.set_properties({"n": n})
        yield e


def to_row(kv):
    from google.cloud.bigtable import row
    word, n = kv
    r = row.DirectRow(row_key=word.encode())
    r.set_cell("cf", b"n", str(n).encode())
    return r


def run(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--db_project", required=True)
    ap.add_argument("--bigtable_instance", required=True)
    ap.add_argument("--bigtable_table", required=True)
    ap.add_argument("--spanner_instance", required=True)
    ap.add_argument("--spanner_database", required=True)
    ap.add_argument("--read_to")
    known, rest = ap.parse_known_args(argv)
    p = beam.Pipeline(options=PipelineOptions(rest))
    if known.read_to:
        (p
         | "ReadDatastore" >> ReadFromDatastore(Query(kind="Word", project=known.db_project))
         | "FormatEntity" >> beam.Map(lambda e: "datastore %s=%d" % (e.key.path_elements[-1], e.properties["n"]))
         | "WriteDatastore" >> beam.io.WriteToText(known.read_to + "datastore"))
        (p
         | "ReadSpanner" >> ReadFromSpanner(known.db_project, known.spanner_instance, known.spanner_database,
                                            sql="SELECT Word, N FROM Words")
         | "FormatRow" >> beam.Map(lambda r: "spanner %s=%d" % (r[0], r[1]))
         | "WriteSpanner" >> beam.io.WriteToText(known.read_to + "spanner"))
        result = p.run()
        print("JOB_ID", result.job_id(), flush=True)
        return
    counts = (p
              | "Words" >> beam.Create(["kiwi", "fig", "kiwi"])
              | "Count" >> beam.combiners.Count.PerElement())
    (counts
     | "Entity" >> beam.ParDo(ToEntity(known.db_project))
     | "Datastore" >> WriteToDatastore(known.db_project))
    (counts
     | "Row" >> beam.Map(to_row)
     | "Bigtable" >> WriteToBigTable(known.db_project, known.bigtable_instance, known.bigtable_table))
    (counts
     | "Mutation" >> beam.MapTuple(lambda w, n: WriteMutation.insert_or_update("Words", ("Word", "N"), [(w, n)]))
     | "Spanner" >> WriteToSpanner(known.db_project, known.spanner_instance, known.spanner_database))
    result = p.run()
    print("JOB_ID", result.job_id(), flush=True)


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    run()
