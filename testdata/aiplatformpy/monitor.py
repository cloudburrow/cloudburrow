"""The Vertex AI SDK's Model Monitoring v2 flow (vertexai.resources.preview
.ml_monitoring), against a CloudBurrow instance (#2214):
TestPythonAIPlatformModelMonitoring runs it as flow.py runs, in a container
on an internal Docker network whose only route is a relay to the
instance's Vertex AI port, with the instance's ADC fixture in the universe
cloudburrow.localhost.

A model is uploaded; ModelMonitor.create monitors it with a training
dataset in Cloud Storage and a drift objective; monitor.run runs a job on a
drifted target dataset; the job's state, monitor.search_metrics and
monitor.search_alerts are read back, and monitor.delete(force=True)
deletes the monitor with its job. Prints one JSON line, the results.
"""

import json
import os
import sys
import time

from google.cloud import aiplatform
from google.oauth2 import service_account
from vertexai.resources.preview import ml_monitoring


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://"):
        sys.exit("CB_ENDPOINT must be the instance's http:// Vertex AI address")
    project, location, bucket = os.environ["CB_PROJECT"], os.environ["CB_LOCATION"], os.environ["CB_BUCKET"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    aiplatform.init(project=project, location=location, credentials=creds, api_endpoint=endpoint, api_transport="rest")

    out = {}
    model = aiplatform.Model.upload(
        display_name=os.environ["CB_MODEL_NAME"],
        serving_container_image_uri="us-docker.pkg.dev/p/r/serve:1",
        sync=True,
    )
    out["model"] = model.resource_name
    schema = ml_monitoring.spec.ModelMonitoringSchema(
        feature_fields=[
            ml_monitoring.spec.FieldSchema(name="age", data_type="integer"),
            ml_monitoring.spec.FieldSchema(name="country", data_type="categorical"),
        ]
    )
    drift = ml_monitoring.spec.DataDriftSpec(
        categorical_metric_type="l_infinity",
        numeric_metric_type="jensen_shannon_divergence",
        default_categorical_alert_threshold=0.2,
        default_numeric_alert_threshold=0.2,
    )
    monitor = ml_monitoring.ModelMonitor.create(
        display_name=os.environ["CB_MODEL_NAME"] + " monitor",
        model_name=model.resource_name,
        model_version_id="1",
        model_monitoring_schema=schema,
        training_dataset=ml_monitoring.spec.MonitoringInput(gcs_uri="gs://%s/train.csv" % bucket, data_format="csv"),
        tabular_objective_spec=ml_monitoring.spec.TabularObjective(feature_drift_spec=drift),
        output_spec=ml_monitoring.spec.OutputSpec(gcs_base_dir="gs://%s/results" % bucket),
    )
    out["monitor"] = monitor.resource_name
    job = monitor.run(
        display_name="drifted",
        target_dataset=ml_monitoring.spec.MonitoringInput(gcs_uri="gs://%s/serve.jsonl" % bucket, data_format="jsonl"),
        sync=True,
    )
    out["job"] = job.resource_name
    state = ""
    for _ in range(300):
        state = monitor.get_model_monitoring_job(job.resource_name).state.name
        if state in ("JOB_STATE_SUCCEEDED", "JOB_STATE_FAILED", "JOB_STATE_CANCELLED"):
            break
        time.sleep(1)
    out["state"] = state
    out["jobs_listed"] = any(j.name == job.resource_name for j in monitor.list_jobs())
    stats = monitor.search_metrics(model_monitoring_job_name=job.resource_name)
    out["stats"] = sorted({m.tabular_stats.stats_name for m in stats})
    alerts = monitor.search_alerts(model_monitoring_job_name=job.resource_name)
    out["alert_keys"] = sorted(alerts)
    out["alerts"] = sorted(a.stats_name for a in alerts.get("model_monitoring_alerts", []))
    out["total_alerts"] = alerts.get("total_number_alerts", 0)
    monitor.delete(force=True)
    try:
        ml_monitoring.ModelMonitor(monitor.resource_name)
        out["deleted"] = False
    except Exception as e:  # NotFound
        out["deleted"] = "404" in str(e) or "NotFound" in type(e).__name__ or "not found" in str(e).lower()
    model.delete()
    print("RESULT " + json.dumps(out, sort_keys=True))


if __name__ == "__main__":
    main()
