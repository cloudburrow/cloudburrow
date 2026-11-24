"""A Python Flex Template's program for TestPythonDataflowFlexTemplate (#2215).

Google's launcher runs the file FLEX_TEMPLATE_PYTHON_PY_FILE names with the
launch's parameters and the DataflowRunner's options as its arguments. With
--input and --output it counts the words of a Cloud Storage file into Cloud
Storage, a batch job; with --subscription and --topic it is a streaming job
that upper-cases each Pub/Sub message into another topic.
"""
import argparse
import logging
import re

import apache_beam as beam
from apache_beam.options.pipeline_options import PipelineOptions, StandardOptions


def run(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--input")
    ap.add_argument("--output")
    ap.add_argument("--subscription")
    ap.add_argument("--topic")
    known, rest = ap.parse_known_args(argv)
    opts = PipelineOptions(rest)
    if known.subscription:
        opts.view_as(StandardOptions).streaming = True
        p = beam.Pipeline(options=opts)
        (p
         | "Read" >> beam.io.ReadFromPubSub(subscription=known.subscription)
         | "Upper" >> beam.Map(lambda b: b.upper())
         | "Write" >> beam.io.WriteToPubSub(known.topic))
        p.run()
        return
    p = beam.Pipeline(options=opts)
    (p
     | "Read" >> beam.io.ReadFromText(known.input)
     | "Split" >> beam.FlatMap(lambda line: re.findall(r"[a-z]+", line.lower()))
     | "Count" >> beam.combiners.Count.PerElement()
     | "Format" >> beam.MapTuple(lambda w, n: "%s: %d" % (w, n))
     | "Write" >> beam.io.WriteToText(known.output))
    p.run()


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    run()
