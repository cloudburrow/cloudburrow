# The TensorFlow SavedModel converter of BigQuery ML's imported TENSORFLOW
# models (#1775). The BigQuery front cannot run TensorFlow (it is a
# CGO-less static binary in the cluster), so `up` serves a converter on the
# host (internal/tfconvert) that runs this image, `docker run --network none`,
# once per CREATE MODEL: Google's tf.lite.TFLiteConverter.from_saved_model
# turns the SavedModel into a TensorFlow Lite flatbuffer of builtin
# operators, which internal/tflitemodel (pure Go) then runs for ML.PREDICT.
#
# tensorflow is Google's PyPI package (Apache-2.0), installed with every
# dependency from the hashed lock requirements.txt (uv pip compile
# --generate-hashes of requirements.in): pip checks each wheel's SHA-256
# and builds nothing from source (#2162). The base is the
# Docker official python image, pinned by its multi-arch index digest
# (dependencies.json's bigquery.tfconvertBase records the same digest).
# Re-resolve with `docker buildx imagetools inspect python:3.12-slim`.
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

COPY requirements.txt /opt/cloudburrow/requirements.txt
RUN pip install --no-cache-dir --require-hashes --no-deps --only-binary :all: -r /opt/cloudburrow/requirements.txt

COPY convert.py /opt/cloudburrow/convert.py

# No network when it runs: the converter reads /model and writes /out.
USER 65534:65534
ENTRYPOINT ["python3", "-I", "/opt/cloudburrow/convert.py"]
