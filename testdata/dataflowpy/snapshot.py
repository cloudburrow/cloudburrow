"""A streaming Pub/Sub word count for TestDataflowSnapshots (#2215).

It reads a subscription, counts each word in one-hour fixed windows, and
publishes "word: n" to a topic. An hour's window only closes when the job
is drained, so until then the counts are the job's state: what a snapshot
keeps and a job created from it starts with.
"""
import argparse
import logging

import apache_beam as beam
from apache_beam.options.pipeline_options import PipelineOptions, StandardOptions
from apache_beam.transforms import window


def run(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--subscription", required=True)
    ap.add_argument("--output_topic", required=True)
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
     | "Format" >> beam.Map(lambda kv: ("%s: %d" % kv).encode("utf-8"))
     | "Publish" >> beam.io.WriteToPubSub(known.output_topic))
    result = p.run()
    print("JOB_ID", result.job_id(), flush=True)


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    run()
