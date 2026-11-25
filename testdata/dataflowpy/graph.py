# Regenerates graph.pb, the browser test's pipeline (#2215), in the pinned SDK:
#   docker run --rm --network none -v "$PWD/testdata/dataflowpy:/out" --entrypoint python \
#     apache/beam_python3.12_sdk@sha256:bf536d721f825259329a396a20947b81ce8d0bbe08472b26f3f149c9957c1e56 /out/graph.py
# Writes the pipeline proto of a three-step batch pipeline in the Python
# SDK's Docker environment, as the DataflowRunner stages one.
import apache_beam as beam
from apache_beam.options.pipeline_options import PipelineOptions, PortableOptions
from apache_beam.transforms import environments

opts = PipelineOptions(["--environment_type=DOCKER", "--environment_config=apache/beam_python3.12_sdk:2.78.0"])
p = beam.Pipeline(options=opts)
_ = p | "Create words" >> beam.Create(["apple", "banana", "cherry"]) | "Upper" >> beam.Map(str.upper) | "Length" >> beam.Map(len)
env = environments.DockerEnvironment.from_options(opts.view_as(PortableOptions))
proto = p.to_runner_api(default_environment=env)
with open("/out/graph.pb", "wb") as f:
    f.write(proto.SerializeToString())
print({k: (e.urn, e.payload[:60], len(e.dependencies)) for k, e in proto.components.environments.items()})
