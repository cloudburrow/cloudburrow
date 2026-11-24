"""Google Cloud Pipeline Components on Agent Platform Pipelines, against a
CloudBurrow instance (#2210): TestPythonGCPC runs this in a container on an
internal Docker network whose only routes are relays to the instance's
Vertex AI port (host.docker.internal) and its Cloud Run port (cb-run.localhost, a .localhost name the host guard accepts),
which serves Artifact Registry's KFP repositories.

With the pinned SDKs (google-cloud-pipeline-components, kfp,
google-cloud-aiplatform 1.x), as Google's guides use them:
  - template: kfp.registry.RegistryClient uploads a compiled pipeline to the
    KFP repository CB_KFP_REPO ("Create, upload, and use a pipeline
    template"), and aiplatform.PipelineJob runs it from its Artifact
    Registry URL (template_path=https://LOCATION-kfp.pkg.dev/.../TAG);
  - components: a pipeline of CustomTrainingJobOp (a custom job whose
    container writes a file to AIP_MODEL_DIR), BigqueryQueryJobOp (a query
    into a destination table), BigqueryCreateModelJobOp (a linear
    regression) and, on its model, BigqueryEvaluateModelJobOp,
    BigqueryPredictModelJobOp and BigqueryExportModelJobOp (EXPORT MODEL
    to CB_EXPORT_DIR);
  - training: aiplatform.CustomContainerTrainingJob(...).run(), a
    TrainingPipeline whose custom job uploads a Model.

The SDK reaches https://LOCATION-kfp.pkg.dev through the cb-run.localhost relay: the
URL is rewritten to the instance's path form
(http://cb-run.localhost:PORT/LOCATION-kfp.pkg.dev/...), as the Go tests reach it,
and nothing else is rewritten. The credential is the instance's ADC
fixture in the universe cloudburrow.localhost (docs/security.md).

Prints one line, RESULT followed by JSON.
"""

import json
import os
import sys
import tempfile
import urllib.request

import requests
from google.cloud import aiplatform
from google.oauth2 import service_account
from google.protobuf import json_format
from google_cloud_pipeline_components.v1.bigquery import (
    BigqueryCreateModelJobOp,
    BigqueryEvaluateModelJobOp,
    BigqueryExportModelJobOp,
    BigqueryPredictModelJobOp,
    BigqueryQueryJobOp,
)
from google_cloud_pipeline_components.v1.custom_job import CustomTrainingJobOp
from kfp import compiler, dsl
from kfp.registry import RegistryClient

IMAGE = os.environ.get("CB_TASK_IMAGE", "")
LOCATION = os.environ.get("CB_LOCATION", "us-central1")
KFP_HOST = "https://" + LOCATION + "-kfp.pkg.dev/"
RUN = os.environ.get("CB_RUN_ENDPOINT", "")


def local_url(url):
    """The instance's path form of a KFP repository URL; any other URL as is."""
    if url.startswith(KFP_HOST):
        return RUN + "/" + url[len("https://"):]
    return url


_send = requests.sessions.Session.request


def _request(self, method, url, *args, **kwargs):
    return _send(self, method, local_url(url), *args, **kwargs)


requests.sessions.Session.request = _request
_urlopen = urllib.request.urlopen


def _open(req, *args, **kwargs):
    if isinstance(req, urllib.request.Request):
        req.full_url = local_url(req.full_url)
    else:
        req = local_url(req)
    return _urlopen(req, *args, **kwargs)


urllib.request.urlopen = _open

# The custom job's container: it writes the model it "trains" to
# AIP_MODEL_DIR, through Cloud Storage FUSE's /gcs path.
TRAIN = (
    "import os; d = os.environ['AIP_MODEL_DIR']; p = '/gcs/' + d[len('gs://'):]; "
    "os.makedirs(p, exist_ok=True); open(os.path.join(p, 'model.txt'), 'w').write('trained ' + os.environ.get('CLOUD_ML_JOB_ID', ''))"
)


@dsl.component(base_image=IMAGE, install_kfp_package=False)
def hello(name: str) -> str:
    return "hello " + name


@dsl.pipeline(name="cb-gcpc-template")
def template(name: str = "world"):
    hello(name=name)


@dsl.pipeline(name="cb-gcpc-components")
def components(project: str, bq_project: str, dataset: str, model_dir: str):
    CustomTrainingJobOp(
        project=project,
        location=LOCATION,
        display_name="cb-gcpc-train",
        worker_pool_specs=[
            {
                "machine_spec": {"machine_type": "n1-standard-4"},
                "replica_count": 1,
                "container_spec": {"image_uri": IMAGE, "command": ["python", "-c", TRAIN]},
            }
        ],
        base_output_directory=model_dir,
    ).set_display_name("custom-job")
    BigqueryQueryJobOp(
        project=bq_project,
        location="US",
        query="SELECT x, y, x * 2 AS z FROM `" + os.environ["CB_BQ_PROJECT"] + "." + os.environ["CB_BQ_DATASET"] + ".train`",
        job_configuration_query={
            "destinationTable": {"projectId": os.environ["CB_BQ_PROJECT"], "datasetId": os.environ["CB_BQ_DATASET"], "tableId": "doubled"},
            "writeDisposition": "WRITE_TRUNCATE",
        },
    ).set_display_name("query")
    model = BigqueryCreateModelJobOp(
        project=bq_project,
        location="US",
        query="CREATE OR REPLACE MODEL `" + os.environ["CB_BQ_PROJECT"] + "." + os.environ["CB_BQ_DATASET"] + ".lin` "
        "OPTIONS(model_type='linear_reg', input_label_cols=['y']) AS SELECT x, y FROM `"
        + os.environ["CB_BQ_PROJECT"] + "." + os.environ["CB_BQ_DATASET"] + ".train`",
    ).set_display_name("create-model")
    BigqueryEvaluateModelJobOp(project=bq_project, location="US", model=model.outputs["model"]).set_display_name("evaluate")
    BigqueryPredictModelJobOp(
        project=bq_project,
        location="US",
        model=model.outputs["model"],
        table_name=os.environ["CB_BQ_PROJECT"] + "." + os.environ["CB_BQ_DATASET"] + ".train",
        job_configuration_query={
            "destinationTable": {"projectId": os.environ["CB_BQ_PROJECT"], "datasetId": os.environ["CB_BQ_DATASET"], "tableId": "predicted"},
        },
    ).set_display_name("predict")
    BigqueryExportModelJobOp(
        project=bq_project, location="US", model=model.outputs["model"], model_destination_path=os.environ["CB_EXPORT_DIR"],
    ).set_display_name("export-model")


def run(path, root, params, template_path=None):
    job = aiplatform.PipelineJob(display_name=os.path.basename(path), template_path=template_path or path,
                                 pipeline_root=root, parameter_values=params, enable_caching=False)
    job.submit()
    try:
        job.wait()
    except RuntimeError as e:
        print("the run failed:", e, file=sys.stderr)
    return json_format.MessageToDict(job.gca_resource._pb)


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://") or not RUN.startswith("http://") or not IMAGE:
        sys.exit("CB_ENDPOINT and CB_RUN_ENDPOINT must be the instance's http:// addresses, and CB_TASK_IMAGE set")
    project = os.environ["CB_PROJECT"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    root = os.environ["CB_PIPELINE_ROOT"]
    aiplatform.init(project=project, location=LOCATION, credentials=creds, api_endpoint=endpoint,
                    api_transport="rest", staging_bucket=os.environ["CB_STAGING"])

    out = {}
    with tempfile.TemporaryDirectory() as d:
        tp, cp = os.path.join(d, "template.yaml"), os.path.join(d, "components.yaml")
        compiler.Compiler().compile(template, tp)
        compiler.Compiler().compile(components, cp)

        host = KFP_HOST + project + "/" + os.environ["CB_KFP_REPO"]
        client = RegistryClient(host=host, auth=creds)
        pkg, version = client.upload_pipeline(file_name=tp, tags=["v1", "latest"], extra_headers={"description": "cb-gcpc"})
        out["uploaded"] = {"package": pkg, "version": version}
        out["template"] = run(tp, root, {"name": "registry"}, template_path=host + "/" + pkg + "/v1")

        out["components"] = run(cp, root, {
            "project": project, "bq_project": os.environ["CB_BQ_PROJECT"], "dataset": os.environ["CB_BQ_DATASET"],
            "model_dir": os.environ["CB_MODEL_DIR"],
        })

    job = aiplatform.CustomContainerTrainingJob(
        display_name="cb-gcpc-training", container_uri=IMAGE, command=["python", "-c", TRAIN],
        model_serving_container_image_uri=IMAGE, model_serving_container_predict_route="/predict",
        model_serving_container_health_route="/health",
    )
    model = job.run(replica_count=1, machine_type="n1-standard-4", model_display_name="cb-gcpc-model", sync=True)
    out["training"] = {
        "pipeline": json_format.MessageToDict(job.gca_resource._pb),
        "model": model.resource_name if model else "",
        "model_uri": model.uri if model else "",
    }
    print("RESULT " + json.dumps(out))


if __name__ == "__main__":
    main()
