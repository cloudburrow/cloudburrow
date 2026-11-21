# The TensorFlow SavedModel converter of BigQuery ML's imported TENSORFLOW
# models (#1775). The BigQuery front cannot run TensorFlow (it is a
# CGO-less static binary in the cluster), so `up` serves a converter on the
# host (internal/tfconvert) that runs this image, once per CREATE MODEL, on
# an --internal Docker network of its own (no route out): Google's
# tf.lite.TFLiteConverter.from_saved_model turns the SavedModel into a
# TensorFlow Lite flatbuffer of builtin operators, which
# internal/tflitemodel (pure Go) then runs for ML.PREDICT. A model
# TensorFlow Lite cannot take runs on TensorFlow in this image's serve mode
# (a long-lived predictor on the same network), as BigQuery runs it.
#
# The base is Google's official TensorFlow image, tensorflow/tensorflow
# 2.20.0 (Apache-2.0), pinned by digest (dependencies.json's
# bigqueryml.tfconvertBase records the same digest). Google publishes it
# for linux/amd64 only, so an arm64 host runs it under its Docker's amd64
# emulation; nothing here RUNs at build time, so the build is a COPY.
# Re-resolve with `docker buildx imagetools inspect tensorflow/tensorflow:2.20.0`.
FROM --platform=linux/amd64 tensorflow/tensorflow:2.20.0@sha256:6b0a7db409e62b7cf188ce72074c520af3e8c21f6d4d5b206f700520594dfb5a

COPY convert.py /opt/cloudburrow/convert.py

# No route out when it runs: the converter reads /model and writes /out.
USER 65534:65534
ENTRYPOINT ["python3", "-I", "/opt/cloudburrow/convert.py"]
