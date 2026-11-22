# The Jupyter optional component of a Dataproc cluster (#2162), built by
# the CLI on the cluster's own Spark image (the official apache/spark image
# of its image version, pinned by digest), so a notebook's Python is the
# executors'. JupyterLab and ipykernel (BSD-3-Clause), pinned, installed
# here and never part of the CLI. Two kernels, as the Jupyter component
# page lists them: "Python 3" and "PySpark", whose `spark` and `sc` are a
# session on the cluster's master.
ARG BASE
FROM ${BASE}
USER root
RUN pip3 install --no-cache-dir jupyterlab==3.6.8 ipykernel==6.29.5
COPY pyspark-startup.py jupyter.sh /opt/cloudburrow/
RUN set -e; py4j="$(ls /opt/spark/python/lib/py4j-*-src.zip | head -1)"; \
    mkdir -p /usr/local/share/jupyter/kernels/pyspark; \
    printf '{"argv": ["python3", "-m", "ipykernel_launcher", "-f", "{connection_file}", "--IPKernelApp.exec_files=[\\"/opt/cloudburrow/pyspark-startup.py\\"]"], "display_name": "PySpark", "language": "python", "env": {"SPARK_HOME": "/opt/spark", "PYTHONPATH": "/opt/spark/python:%s", "PYSPARK_PYTHON": "python3"}}\n' "$py4j" \
      > /usr/local/share/jupyter/kernels/pyspark/kernel.json
USER 185
ENV HOME=/tmp
ENTRYPOINT ["/bin/bash", "/opt/cloudburrow/jupyter.sh"]
