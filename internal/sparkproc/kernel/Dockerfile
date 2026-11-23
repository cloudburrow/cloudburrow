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
#     (spark-connect-client-jvm, Apache-2.0).
# Everything is pinned: the PyPI packages by version, the coursier
# launcher (Apache-2.0) and the Spark Connect client jar by SHA-256, and
# Almond by version (coursier resolves it from Maven Central and bundles
# it into one standalone launcher). None of it is part of the CLI.
ARG BASE
FROM ${BASE}
ARG SCALA=2.12.18
ARG CONNECT_URL=https://repo1.maven.org/maven2/org/apache/spark/spark-connect-client-jvm_2.12/3.5.9/spark-connect-client-jvm_2.12-3.5.9.jar
ARG CONNECT_SHA=ec52b1cdc2243187d6c363fdb37f1e39ddea46c0f98e9a01f01c5be05ce26590
USER root
RUN pip3 install --no-cache-dir jupyter_server==2.14.2 ipykernel==6.29.5 pandas==2.2.3 pyarrow==17.0.0 \
      grpcio==1.62.3 grpcio-status==1.62.3 protobuf==4.25.5 googleapis-common-protos==1.63.2
ADD --checksum=sha256:8d1e0d684a55c09828b7afda481139472d87a18e0544bb875e2e5e3e300f64b3 \
    https://github.com/coursier/coursier/releases/download/v2.1.27/coursier.jar /opt/cloudburrow/coursier.jar
ADD --checksum=sha256:${CONNECT_SHA} ${CONNECT_URL} /opt/cloudburrow/spark-connect-client-jvm.jar
COPY predef.sc pyspark-startup.py kernel.sh /opt/cloudburrow/
RUN set -e; mkdir -p /opt/almond; \
    java -jar /opt/cloudburrow/coursier.jar bootstrap --standalone -r jitpack \
      "sh.almond:scala-kernel_${SCALA}:0.14.5" -o /opt/almond/almond; \
    /opt/almond/almond --install --global --id scala --display-name "Scala" --predef /opt/cloudburrow/predef.sc; \
    cp=/opt/cloudburrow/spark-connect-client-jvm.jar; \
    for j in /opt/spark/jars/*.jar; do case "$(basename "$j")" in \
      spark-common-utils_*) cp="$cp:$j" ;; \
      spark-*|scala-*|hive-*|hadoop-*|parquet-*|orc-*|arrow-*|netty-*|guava-*|protobuf-*|derby-*|datanucleus-*|kubernetes-*|zookeeper-*|avro-*) ;; \
      *) cp="$cp:$j" ;; esac; done; \
    CP="$cp" python3 -c 'import json, os; p = "/usr/local/share/jupyter/kernels/scala/kernel.json"; k = json.load(open(p)); i = k["argv"].index("java") + 1; k["argv"][i:i] = ["-Duser.home=/tmp", "--add-opens=java.base/java.nio=ALL-UNNAMED", "--add-opens=java.base/sun.nio.ch=ALL-UNNAMED"]; i = k["argv"].index("--connection-file"); k["argv"][i:i] = ["--extra-class-path", os.environ["CP"]]; k["env"] = {"COURSIER_CACHE": "/tmp/coursier"}; json.dump(k, open(p, "w"), indent=1)'; \
    rm -rf /root/.cache/coursier /opt/cloudburrow/coursier.jar; \
    py4j="$(ls /opt/spark/python/lib/py4j-*-src.zip | head -1)"; \
    mkdir -p /usr/local/share/jupyter/kernels/python3; \
    printf '{"argv": ["python3", "-m", "ipykernel_launcher", "-f", "{connection_file}", "--IPKernelApp.exec_files=[\\"/opt/cloudburrow/pyspark-startup.py\\"]"], "display_name": "PySpark", "language": "python", "env": {"SPARK_HOME": "/opt/spark", "PYTHONPATH": "/opt/spark/python:%s"}}\n' "$py4j" \
      > /usr/local/share/jupyter/kernels/python3/kernel.json; \
    chmod -R a+rX /opt/almond /opt/cloudburrow /usr/local/share/jupyter
USER 185
# The image user (185) has no home: Java's user.home is /nonexistent.
ENV HOME=/tmp
ENTRYPOINT ["/bin/bash", "/opt/cloudburrow/kernel.sh"]
