# A batch pipeline whose one step counts each element (the user counter
# dev.cloudburrow/seen) and then fails on the element "boom" (#2215,
# TestPythonDataflowCommittedAndTentative): the bundle holding it fails,
# so what it counted is attempted (tentative) but never committed.
import logging
import sys

import apache_beam as beam
from apache_beam.metrics import Metrics
from apache_beam.options.pipeline_options import PipelineOptions


class CountThenFail(beam.DoFn):
    def __init__(self):
        self.seen = Metrics.counter("dev.cloudburrow", "seen")

    def process(self, x):
        self.seen.inc()
        if x == "boom":
            raise ValueError("boom: the element this pipeline fails on")
        yield x


def main(argv):
    p = beam.Pipeline(options=PipelineOptions(argv))
    _ = p | "Create" >> beam.Create(["apple", "banana", "cherry", "boom"]) | "Count" >> beam.ParDo(CountThenFail())
    result = p.run()
    try:
        result.wait_until_finish()
    except Exception as e:  # the job FAILED, as it should
        print("FINISHED %s" % type(e).__name__)
    print("STATE %s" % result.state)


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    main(sys.argv[1:])
