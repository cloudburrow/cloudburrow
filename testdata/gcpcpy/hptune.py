"""Hyperparameter tuning against a CloudBurrow instance (#2210):
TestPythonHyperparameterTuning runs this in the gcpcpy fixture on an
internal Docker network whose only route is a relay to the instance's
Vertex AI port (host.docker.internal).

As Google's guides use the pinned SDKs:
  - aiplatform.HyperparameterTuningJob over an aiplatform.CustomJob
    ("Create a hyperparameter tuning job", Python tab): metric_spec
    {"loss": "minimize"}, a log-scaled DoubleParameterSpec lr and a
    CategoricalParameterSpec opt, max_trial_count 4, parallel_trial_count
    2; each trial's container is this image running TRAIN, which parses
    --lr and --opt with argparse and reports loss with cloudml-hypertune's
    report_hyperparameter_tuning_metric;
  - GCPC's HyperparameterTuningJobRunOp in a pipeline, its study_spec
    serialized with serialize_metrics and serialize_parameters, with
    GRID_SEARCH over an IntegerParameterSpec.

Prints one line, RESULT followed by JSON.
"""

import json
import os
import sys
import tempfile

from google.cloud import aiplatform
from google.cloud.aiplatform import hyperparameter_tuning as hpt
from google.oauth2 import service_account
from google.protobuf import json_format
from google_cloud_pipeline_components.v1.hyperparameter_tuning_job import (
    HyperparameterTuningJobRunOp,
    serialize_metrics,
    serialize_parameters,
)
from kfp import compiler, dsl

IMAGE = os.environ.get("CB_TASK_IMAGE", "")
LOCATION = os.environ.get("CB_LOCATION", "us-central1")

# The trial's training code: its loss is least at lr=0.3 with adam.
TRAIN = (
    "import argparse, hypertune\n"
    "p = argparse.ArgumentParser()\n"
    "p.add_argument('--lr', type=float, default=0.5)\n"
    "p.add_argument('--opt', default='adam')\n"
    "p.add_argument('--layers', type=int, default=1)\n"
    "a, _ = p.parse_known_args()\n"
    "loss = (a.lr - 0.3) ** 2 + (1.0 if a.opt == 'sgd' else 0.0) + 0.01 * a.layers\n"
    "t = hypertune.HyperTune()\n"
    "t.report_hyperparameter_tuning_metric(hyperparameter_metric_tag='loss', metric_value=loss + 0.5, global_step=1)\n"
    "t.report_hyperparameter_tuning_metric(hyperparameter_metric_tag='loss', metric_value=loss, global_step=2)\n"
)


def worker_pool():
    return [{
        "machine_spec": {"machine_type": "n1-standard-4"},
        "replica_count": 1,
        "container_spec": {"image_uri": IMAGE, "command": ["python", "-c", TRAIN]},
    }]


@dsl.pipeline(name="cb-gcpc-hptune")
def tuning(project: str, base: str):
    HyperparameterTuningJobRunOp(
        display_name="cb-gcpc-hptune",
        project=project,
        location=LOCATION,
        worker_pool_specs=worker_pool(),
        study_spec_metrics=serialize_metrics({"loss": "minimize"}),
        study_spec_parameters=serialize_parameters({"layers": hpt.IntegerParameterSpec(min=1, max=3, scale="linear")}),
        study_spec_algorithm="GRID_SEARCH",
        max_trial_count=3,
        parallel_trial_count=3,
        base_output_directory=base,
    ).set_display_name("tune")


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://") or not IMAGE:
        sys.exit("CB_ENDPOINT must be the instance's http:// address, and CB_TASK_IMAGE set")
    project = os.environ["CB_PROJECT"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    aiplatform.init(project=project, location=LOCATION, credentials=creds, api_endpoint=endpoint,
                    api_transport="rest", staging_bucket=os.environ["CB_STAGING"])
    out = {}

    custom = aiplatform.CustomJob(display_name="cb-hptune-trial", worker_pool_specs=worker_pool(),
                                  base_output_dir=os.environ["CB_BASE"])
    job = aiplatform.HyperparameterTuningJob(
        display_name="cb-hptune",
        custom_job=custom,
        metric_spec={"loss": "minimize"},
        parameter_spec={
            "lr": hpt.DoubleParameterSpec(min=0.01, max=1, scale="log"),
            "opt": hpt.CategoricalParameterSpec(values=["adam", "sgd"]),
        },
        max_trial_count=4,
        parallel_trial_count=2,
    )
    job.run(sync=True)
    out["sdk"] = json_format.MessageToDict(job.gca_resource._pb)

    with tempfile.TemporaryDirectory() as d:
        path = os.path.join(d, "tuning.yaml")
        compiler.Compiler().compile(tuning, path)
        run = aiplatform.PipelineJob(display_name="cb-gcpc-hptune", template_path=path,
                                     pipeline_root=os.environ["CB_PIPELINE_ROOT"], enable_caching=False,
                                     parameter_values={"project": project, "base": os.environ["CB_BASE"] + "/gcpc"})
        run.submit()
        try:
            run.wait()
        except RuntimeError as e:
            print("the run failed:", e, file=sys.stderr)
        out["pipeline"] = json_format.MessageToDict(run.gca_resource._pb)
    print("RESULT " + json.dumps(out))


if __name__ == "__main__":
    main()
