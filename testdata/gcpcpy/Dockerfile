# Google Cloud Pipeline Components (google-cloud-pipeline-components 2.23.0,
# Apache-2.0) with the Kubeflow Pipelines SDK (kfp) and the Vertex AI SDK
# (google-cloud-aiplatform 1.x), for TestPythonGCPC (#2210): flow.py
# compiles pipelines of GCPC's CustomJob, BigQuery and batch prediction
# components, runs them with aiplatform.PipelineJob (from pipeline_spec and
# from an Artifact Registry template), and runs a CustomContainerTrainingJob.
# CloudBurrow runs GCPC's launchers itself, so no GCPC image is pulled; the
# custom job's container is this image. The test runs the SDK on an internal
# Docker network with no route out, and CloudBurrow runs the containers with
# no network; the install is the only step with a network. The base image is
# pinned by digest (a multi-arch manifest list) and every package by version
# and hash.
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
COPY requirements.txt /gcpcpy/requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps -r /gcpcpy/requirements.txt
ENTRYPOINT ["python", "-I"]
