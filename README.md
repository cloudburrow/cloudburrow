# CloudBurrow

A local Google Cloud emulator for development and testing.

CloudBurrow aims to let you build and test applications on your own machine using familiar Google Cloud SDKs, without deploying to GCP for every change.

## Status

Early planning. No emulator implementation or installable release is available yet.

## Planned direction

- A standalone emulator written in Go, with compatible gRPC and REST endpoints.
- Initial focus on Cloud Storage, Pub/Sub, Cloud Run, and Cloud Tasks.
- Docker-backed execution for Cloud Run application containers.
- Local resource setup, event inspection, and repeatable resets for tests.
- Compatibility tests using official Google Cloud client libraries.

The first target workflow is uploading a file, publishing an event, running a worker, and saving the result locally. These are planned capabilities, not currently supported features.

## Domains

- cloudburrow.com
- cloudburrow.dev

Independent community project. Not affiliated with or endorsed by Google.
