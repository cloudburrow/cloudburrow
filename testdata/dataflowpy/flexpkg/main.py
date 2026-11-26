"""A Python Flex Template's program with packages, for
TestPythonDataflowFlexTemplatePackages (#2215).

Its image names a requirements file (FLEX_TEMPLATE_PYTHON_REQUIREMENTS_FILE:
cbgreet, from an Artifact Registry Python repository), a setup file
(FLEX_TEMPLATE_PYTHON_SETUP_FILE: the cbsetup package beside it) and an
extra package (FLEX_TEMPLATE_PYTHON_EXTRA_PACKAGES: the cbextra wheel). The
launcher installs the requirements before it runs this program, so cbgreet
is imported here, at the top; the job's workers install all three from
what the SDK staged, and use them in Tag.
"""
import argparse
import logging

import apache_beam as beam
import cbgreet  # installed in the launcher from the requirements file
from apache_beam.options.pipeline_options import PipelineOptions


def tag(word):
    import cbextra
    import cbsetup
    return "%s %s %s" % (cbgreet.greet(word), cbextra.shout(word), cbsetup.TAG)


def run(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--input")
    ap.add_argument("--output")
    known, rest = ap.parse_known_args(argv)
    p = beam.Pipeline(options=PipelineOptions(rest))
    (p
     | "Read" >> beam.io.ReadFromText(known.input)
     | "Tag" >> beam.Map(tag)
     | "Write" >> beam.io.WriteToText(known.output))
    p.run()


if __name__ == "__main__":
    logging.getLogger().setLevel(logging.INFO)
    run()
