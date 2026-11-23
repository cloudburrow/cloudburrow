# A Jupyter session's kernels (#2162), built by the CLI on the session's
# runtime image (the official apache/spark image of its Spark, pinned by
# digest), so a kernel's Spark Connect client is the server's own Spark.
# Its Jupyter Server answers the kernel gateway's REST and websocket
# calls; the session's container network is its only one, so it reaches
# the session's Spark Connect server on localhost and nothing else.
#
# Two kernels, as a Jupyter session's kernel is PYTHON or SCALA:
#   - python3 ("PySpark"): ipykernel, its `spark` a Spark Connect session
#     of the session's server (the image's pyspark, with the Connect
#     client's pandas, pyarrow, grpcio and Google's googleapis-common-protos);
#   - scala ("Scala"): Almond (BSD-3-Clause), the Scala kernel for
#     Jupyter, its `spark` a Spark Connect session of the Scala client
#     (spark-connect-client-jvm, with Spark 4's spark-connect-shims;
#     Apache-2.0).
# Everything is pinned by hash, nothing resolved at build time: the PyPI
# packages from a full lock (requirements-PY.txt, pip --require-hashes),
# and every jar, Almond's whole class path (almond-SCALA.lock, as coursier
# resolved it from Maven Central and JitPack) and the Connect client
# (connect-SPARK.lock), by SHA-256 (fetch.sh). None of it is in the CLI.
ARG BASE
FROM ${BASE}
ARG SCALA=2.12.18
ARG SPARK=3.5.9
ARG PY=3.5
USER root
COPY requirements-${PY}.txt almond-${SCALA}.lock connect-${SPARK}.lock fetch.sh predef.sc pyspark-startup.py kernel.sh /opt/cloudburrow/
# Spark 4's Ubuntu 24.04 images' pip refuses a system install without
# PIP_BREAK_SYSTEM_PACKAGES; Ubuntu 22.04's older pip ignores it.
ENV PIP_BREAK_SYSTEM_PACKAGES=1
RUN pip3 install --no-cache-dir --require-hashes --no-deps --only-binary :all: -r /opt/cloudburrow/requirements-${PY}.txt
RUN set -e; \
    bash /opt/cloudburrow/fetch.sh /opt/cloudburrow/almond-${SCALA}.lock /opt/almond/lib; \
    bash /opt/cloudburrow/fetch.sh /opt/cloudburrow/connect-${SPARK}.lock /opt/cloudburrow/connect; \
    cp="$(ls /opt/cloudburrow/connect/*.jar | tr '\n' ':')"; \
    for j in /opt/spark/jars/*.jar; do case "$(basename "$j")" in \
      spark-common-utils*|spark-unsafe_*|spark-variant_*|spark-sketch_*|spark-tags_*) cp="$cp$j:" ;; \
      spark-*|scala-*|hive-*|hadoop-*|parquet-*|orc-*|arrow-*|netty-*|guava-*|protobuf-*|derby-*|datanucleus-*|kubernetes-*|zookeeper-*|avro-*) ;; \
      *) cp="$cp$j:" ;; esac; done; \
    mkdir -p /usr/local/share/jupyter/kernels/scala /usr/local/share/jupyter/kernels/python3; \
    CP="${cp%:}" python3 -c 'import json, os; json.dump({"argv": ["java", "-Duser.home=/tmp", "--add-opens=java.base/java.nio=ALL-UNNAMED", "--add-opens=java.base/sun.nio.ch=ALL-UNNAMED", "-cp", "/opt/almond/lib/*", "almond.ScalaKernel", "--extra-class-path", os.environ["CP"], "--predef", "/opt/cloudburrow/predef.sc", "--connection-file", "{connection_file}"], "display_name": "Scala", "language": "scala", "env": {"COURSIER_CACHE": "/tmp/coursier"}}, open("/usr/local/share/jupyter/kernels/scala/kernel.json", "w"), indent=1)'; \
    py4j="$(ls /opt/spark/python/lib/py4j-*-src.zip | head -1)"; \
    printf '{"argv": ["python3", "-m", "ipykernel_launcher", "-f", "{connection_file}", "--IPKernelApp.exec_files=[\\"/opt/cloudburrow/pyspark-startup.py\\"]"], "display_name": "PySpark", "language": "python", "env": {"SPARK_HOME": "/opt/spark", "PYTHONPATH": "/opt/spark/python:%s"}}\n' "$py4j" \
      > /usr/local/share/jupyter/kernels/python3/kernel.json; \
    chmod -R a+rX /opt/almond /opt/cloudburrow /usr/local/share/jupyter
USER 185
# The image user (185) has no home: Java's user.home is /nonexistent.
ENV HOME=/tmp
ENTRYPOINT ["/bin/bash", "/opt/cloudburrow/kernel.sh"]
