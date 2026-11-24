"""KFP control flow on Agent Platform Pipelines, against a CloudBurrow
instance (#2205): TestPythonKFPControlFlow runs this in a container on an
internal Docker network whose only route is a relay to the instance's
Vertex AI port.

It compiles pipelines with the Kubeflow Pipelines SDK (kfp) and runs them
with the Vertex AI SDK's aiplatform.PipelineJob, as Google's "Build a
pipeline" and "Run a pipeline" guides do:
  - control_flow: an importer, a nested pipeline, dsl.If/Elif/Else on a
    task's output, dsl.ParallelFor with parallelism and dsl.Collected, a
    component that writes a Model and logs Metrics;
  - exit_handler: dsl.ExitHandler around a task that always fails, retried
    twice (set_retry), and the exit task that takes its final status.
control_flow runs with enable_caching=True twice (the second run is all
cache hits) and once with enable_caching=False.

The credential is the instance's ADC fixture in the universe
cloudburrow.localhost (docs/security.md), as testdata/aiplatformpy's.

Prints one line, RESULT followed by JSON: each run's PipelineJob (as the
REST API returned it) and what the SDK read back from ML Metadata.
"""

import json
import os
import sys
import tempfile
from typing import List

from google.cloud import aiplatform
from google.oauth2 import service_account
from google.protobuf import json_format
from kfp import compiler, dsl
from kfp.dsl import Dataset, Input, Metrics, Model, Output

IMAGE = os.environ.get("CB_TASK_IMAGE", "")


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def flip(seed: int) -> str:
    return ["heads", "tails", "edge"][seed % 3]


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def say(msg: str) -> str:
    return msg


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def square(x: int) -> int:
    return x * x


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def total(xs: List[int]) -> int:
    return sum(xs)


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def train(data: Input[Dataset], model: Output[Model], metrics: Output[Metrics]):
    with open(data.path) as f:
        rows = f.read()
    with open(model.path, "w") as f:
        f.write(rows.upper())
    metrics.log_metric("accuracy", 0.9)
    metrics.log_metric("rows", len(rows.splitlines()))


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def boom():
    raise RuntimeError("boom")


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def notify(status: dsl.PipelineTaskFinalStatus) -> str:
    return f"{status.state} {status.pipeline_task_name} {status.error_code}"


@dsl.pipeline(name="cb-inner")
def inner(n: int) -> int:
    return square(x=n).output


@dsl.pipeline(name="cb-control-flow")
def control_flow(data_uri: str, seed: int = 3, xs: List[int] = [1, 2, 3]):
    data = dsl.importer(artifact_uri=data_uri, artifact_class=Dataset, reimport=False)
    coin = flip(seed=seed)
    with dsl.If(coin.output == "heads"):
        say(msg="H").set_display_name("say-heads")
    with dsl.Elif(coin.output == "tails"):
        say(msg="T").set_display_name("say-tails")
    with dsl.Else():
        say(msg="E").set_display_name("say-edge")
    with dsl.ParallelFor(xs, parallelism=2) as x:
        sq = square(x=x)
    total(xs=dsl.Collected(sq.output))
    train(data=data.output)
    inner(n=seed)


@dsl.pipeline(name="cb-exit-handler")
def exit_handler():
    done = notify()
    with dsl.ExitHandler(done):
        boom().set_retry(num_retries=2, backoff_duration="1s", backoff_factor=1.0)


def run(path, root, params, caching):
    job = aiplatform.PipelineJob(display_name=os.path.basename(path), template_path=path, pipeline_root=root,
                                 parameter_values=params, enable_caching=caching)
    job.submit()
    try:
        job.wait()
    except RuntimeError as e:
        print("the run failed, as it may:", e, file=sys.stderr)
    return json_format.MessageToDict(job.gca_resource._pb)


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://") or not IMAGE:
        sys.exit("CB_ENDPOINT must be the instance's http:// Vertex AI address, and CB_TASK_IMAGE set")
    project, location = os.environ["CB_PROJECT"], os.environ["CB_LOCATION"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    aiplatform.init(project=project, location=location, credentials=creds, api_endpoint=endpoint, api_transport="rest")
    root, data_uri = os.environ["CB_PIPELINE_ROOT"], os.environ["CB_DATA_URI"]

    out = {}
    with tempfile.TemporaryDirectory() as d:
        cf, eh = os.path.join(d, "control_flow.json"), os.path.join(d, "exit_handler.json")
        compiler.Compiler().compile(control_flow, cf)
        compiler.Compiler().compile(exit_handler, eh)
        with open(cf) as f:
            out["spec"] = json.load(f)
        params = {"data_uri": data_uri, "seed": 1}
        out["first"] = run(cf, root, params, True)
        out["cached"] = run(cf, root, params, True)
        out["uncached"] = run(cf, root, dict(params, seed=2), False)
        out["exit"] = run(eh, root, None, None)

    metrics = aiplatform.Artifact.list(filter='schema_title="system.Metrics"')
    out["metrics"] = [{"name": a.resource_name, "metadata": dict(a.metadata)} for a in metrics]
    out["models"] = [a.uri for a in aiplatform.Artifact.list(filter='schema_title="system.Model"')]
    execs = aiplatform.Execution.list(filter='schema_title="system.ContainerExecution" AND state="CACHED"')
    out["cached_executions"] = len(execs)
    print("RESULT " + json.dumps(out))


if __name__ == "__main__":
    main()
