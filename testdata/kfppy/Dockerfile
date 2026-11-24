# The Kubeflow Pipelines SDK (kfp, Apache-2.0) and the Vertex AI SDK
# (google-cloud-aiplatform), for TestPythonKFPControlFlow (#2205): flow.py
# compiles pipelines with kfp and runs them with aiplatform.PipelineJob,
# and the pipelines' lightweight Python components run in this same image
# (base_image), so a task needs nothing it would have to download. The
# test runs the SDK on an internal Docker network with no route out, and
# CloudBurrow runs the tasks with --network none; the install is the only
# step with a network. The base image is pinned by digest (a multi-arch
# manifest list) and every package by version and hash.
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
COPY requirements.txt /kfppy/requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps -r /kfppy/requirements.txt
ENTRYPOINT ["python", "-I"]
