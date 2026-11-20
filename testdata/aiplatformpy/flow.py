"""The Vertex AI SDK's Model.upload(...).deploy(...) flow, against a
CloudBurrow instance (#1940): TestPythonAIPlatformUploadAndDeploy runs it
in a container on an internal Docker network, whose only route is a relay
to the instance's Vertex AI port.

The credential is the instance's ADC fixture in the universe
cloudburrow.localhost (docs/security.md): google-auth makes no Regional
Access Boundary lookup for a credential outside googleapis.com (#1981),
and signs its own JWTs, so it needs no token endpoint.

Prints one JSON line, the results, for the test to check.
"""

import json
import os
import sys

from google.cloud import aiplatform
from google.oauth2 import service_account


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://"):
        sys.exit("CB_ENDPOINT must be the instance's http:// Vertex AI address")
    project, location = os.environ["CB_PROJECT"], os.environ["CB_LOCATION"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    aiplatform.init(project=project, location=location, credentials=creds, api_endpoint=endpoint, api_transport="rest")

    out = {}
    model = aiplatform.Model.upload(
        display_name=os.environ["CB_MODEL_NAME"],
        artifact_uri=os.environ["CB_ARTIFACT_URI"],
        serving_container_image_uri=os.environ["CB_IMAGE"],
        serving_container_predict_route="/v1/models/linear:predict",
        serving_container_health_route="/v1/models/linear",
        serving_container_ports=[7080],
        serving_container_environment_variables={"MODEL_FLAVOUR": "python-sdk"},
        labels={"sdk": "python"},
        sync=True,
    )
    out["model"] = model.resource_name
    out["version_id"] = model.version_id
    out["version_aliases"] = list(model.version_aliases)
    out["export_formats"] = {k: sorted(c.name for c in v) for k, v in model.supported_export_formats.items()}

    second = aiplatform.Model.upload(
        parent_model=model.resource_name,
        display_name=os.environ["CB_MODEL_NAME"],
        artifact_uri=os.environ["CB_ARTIFACT_URI"],
        serving_container_image_uri=os.environ["CB_IMAGE"],
        serving_container_predict_route="/v1/models/linear:predict",
        serving_container_health_route="/v1/models/linear",
        serving_container_ports=[7080],
        is_default_version=False,
        version_aliases=["candidate"],
        sync=True,
    )
    out["second_version"] = second.version_id
    out["versions"] = sorted(v.version_id for v in model.versioning_registry.list_versions())

    endpoint_res = model.deploy(
        deployed_model_display_name="python-sdk",
        machine_type="n1-standard-2",
        min_replica_count=1,
        max_replica_count=1,
        traffic_percentage=100,
        sync=True,
    )
    out["endpoint"] = endpoint_res.resource_name
    deployed = endpoint_res.list_models()
    out["deployed"] = [d.id for d in deployed]
    out["traffic_split"] = dict(endpoint_res.traffic_split)

    pred = endpoint_res.predict(instances=[2, 5])
    out["predictions"] = json.loads(json.dumps(list(pred.predictions), default=str))
    out["deployed_model_id"] = pred.deployed_model_id
    out["model_version_id"] = pred.model_version_id

    listed = [e.resource_name for e in aiplatform.Endpoint.list()]
    out["endpoint_listed"] = endpoint_res.resource_name in listed
    out["model_listed"] = model.resource_name in [m.resource_name for m in aiplatform.Model.list()]

    endpoint_res.undeploy_all(sync=True)
    endpoint_res.delete(sync=True)
    second.versioning_registry.delete_version(second.version_id)
    model.delete(sync=True)
    out["cleaned"] = True
    print("RESULT " + json.dumps(out), flush=True)


if __name__ == "__main__":
    main()
