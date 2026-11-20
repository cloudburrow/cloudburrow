# The Python Vertex AI SDK (google-cloud-aiplatform), for
# TestPythonAIPlatformUploadAndDeploy (#1940). The test runs it on an
# internal Docker network with no route out, so nothing it does can reach
# Google; the install is the only step with a network. The base image is
# pinned by digest (a multi-arch manifest list).
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
RUN pip install --no-cache-dir google-cloud-aiplatform==2.9.0
ENTRYPOINT ["python", "-I"]
