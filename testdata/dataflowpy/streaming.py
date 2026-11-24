"""A streaming Pub/Sub to BigQuery pipeline for TestPythonDataflow (#2215).

It reads a subscription, counts each word in one-hour fixed windows, and
streams the counts into a BigQuery table with Beam's own WriteToBigQuery
(streaming inserts). An hour's window only closes when the job is
drained: the counts reach BigQuery when it is.
"""
import argparse
import logging

import apache_beam as beam
from apache_beam.options.pipeline_options import PipelineOptions, StandardOptions
from apache_beam.transforms import window


def run(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--subscription", required=True)
    ap.add_argument("--table", required=True)
    known, rest = ap.parse_known_args(argv)
    opts = PipelineOptions(rest)
    opts.view_as(StandardOptions).streaming = True
    p = beam.Pipeline(options=opts)
    (p
     | "Read" >> beam.io.ReadFromPubSub(subscription=known.subscription)
     | "Decode" >> beam.Map(lambda b: b.decode("utf-8").strip())
     | "Window" >> beam.WindowInto(window.FixedWindows(3600))
     | "Pair" >> beam.Map(lambda w: (w, 1))
     | "Count" >> beam.CombinePerKey(sum)
     | "Row" >> beam.Map(lambda kv: {"word": kv[0], "n": kv[1]})
     | "Write" >> beam.io.WriteToBigQuery(
         known.table, schema="word:STRING,n:INTEGER",
         method=beam.io.WriteToBigQuery.Method.STREAMING_INSERTS,
         create_disposition=beam.io.BigQueryDisposition.CREATE_IF_NEEDED,
         write_disposition=beam.io.BigQueryDisposition.WRITE_APPEND))
    result = p.run()
    print("JOB_ID", result.job_id(), flush=True)


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    run()
