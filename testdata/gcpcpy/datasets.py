"""Vertex AI managed datasets with the Vertex AI SDK, against a CloudBurrow
instance (#2210): TestPythonVertexAIDatasets runs this in the gcpcpy image
(google-cloud-aiplatform 1.x, pinned and hash-locked) on an internal Docker
network whose only route is a relay to the instance's Vertex AI port.

As Google's guides use the SDK:
  - aiplatform.ImageDataset.create with a Cloud Storage import file in the
    single-label classification schema;
  - aiplatform.TextDataset.create with a sentiment import file;
  - aiplatform.TabularDataset.create from a CSV file in Cloud Storage
    (no import: the dataset reads its source);
  - aiplatform.CustomContainerTrainingJob(...).run(dataset=...) on the image
    dataset with a fraction split, its container writing the AIP_*_DATA_URI
    variables it was given to AIP_MODEL_DIR, and on the tabular dataset.

The test uploads the fixture files (CB_BUCKET) before running this; the SDK
never reaches Cloud Storage itself (a CustomContainerTrainingJob stages
nothing, which is why it is used in place of CustomTrainingJob's script
package). The credential is the instance's ADC fixture in the universe
cloudburrow.localhost (docs/security.md).

Prints one line, RESULT followed by JSON.
"""

import json
import os
import sys

from google.cloud import aiplatform
from google.oauth2 import service_account

IMAGE = os.environ.get("CB_TASK_IMAGE", "")
LOCATION = os.environ.get("CB_LOCATION", "us-central1")

# The custom job's container: it records the data variables it was given.
TRAIN = (
    "import os; d = os.environ['AIP_MODEL_DIR']; p = '/gcs/' + d[len('gs://'):]; os.makedirs(p, exist_ok=True); "
    "open(os.path.join(p, 'env.json'), 'w').write(__import__('json').dumps({k: v for k, v in os.environ.items() if k.startswith('AIP_')}))"
)


def main():
    endpoint = os.environ["CB_ENDPOINT"]
    if not endpoint.startswith("http://") or not IMAGE:
        sys.exit("CB_ENDPOINT must be the instance's http:// address, and CB_TASK_IMAGE set")
    bucket = os.environ["CB_BUCKET"]
    creds = service_account.Credentials.from_service_account_file(
        os.environ["CB_CREDENTIALS"], scopes=["https://www.googleapis.com/auth/cloud-platform"]
    ).with_universe_domain("cloudburrow.localhost")
    aiplatform.init(project=os.environ["CB_PROJECT"], location=LOCATION, credentials=creds, api_endpoint=endpoint,
                    api_transport="rest", staging_bucket="gs://" + bucket + "/staging")
    out = {}

    image = aiplatform.ImageDataset.create(
        display_name="cb-flowers", gcs_source="gs://" + bucket + "/flowers.jsonl",
        import_schema_uri=aiplatform.schema.dataset.ioformat.image.single_label_classification, sync=True)
    # create() keeps the Dataset it read before the import; read it again
    # for the imported items' count.
    image = aiplatform.ImageDataset(image.resource_name)
    out["image"] = {"name": image.resource_name, "schema": image.metadata_schema_uri,
                    "items": int(image.gca_resource.data_item_count)}

    text = aiplatform.TextDataset.create(
        display_name="cb-reviews", gcs_source="gs://" + bucket + "/reviews.jsonl",
        import_schema_uri=aiplatform.schema.dataset.ioformat.text.sentiment, sync=True)
    text = aiplatform.TextDataset(text.resource_name)
    out["text"] = {"name": text.resource_name, "items": int(text.gca_resource.data_item_count)}

    tabular = aiplatform.TabularDataset.create(display_name="cb-tab", gcs_source=["gs://" + bucket + "/tab.csv"], sync=True)
    # Not column_names: the SDK reads the CSV header from Cloud Storage itself.
    out["tabular"] = {"name": tabular.resource_name, "schema": tabular.metadata_schema_uri}

    listed = aiplatform.ImageDataset.list(filter='display_name="cb-flowers"')
    out["listed"] = [d.resource_name for d in listed]

    exported = image.export_data(output_dir="gs://" + bucket + "/export")
    out["exported"] = list(exported)

    job = aiplatform.CustomContainerTrainingJob(display_name="cb-ds-training", container_uri=IMAGE, command=["python", "-c", TRAIN])
    job.run(
        dataset=image, annotation_schema_uri=aiplatform.schema.dataset.annotation.image.classification,
        base_output_dir="gs://" + bucket + "/tp", replica_count=1, machine_type="n1-standard-4",
        training_fraction_split=0.5, validation_fraction_split=0.25, test_fraction_split=0.25, sync=True,
    )
    out["training"] = {"state": job.state.name, "name": job.resource_name}

    tjob = aiplatform.CustomContainerTrainingJob(display_name="cb-tab-training", container_uri=IMAGE, command=["python", "-c", TRAIN])
    tjob.run(dataset=tabular, base_output_dir="gs://" + bucket + "/tab-tp", replica_count=1, machine_type="n1-standard-4", sync=True)
    out["tabular_training"] = {"state": tjob.state.name}

    text.delete(sync=True)
    print("RESULT " + json.dumps(out))


if __name__ == "__main__":
    main()
