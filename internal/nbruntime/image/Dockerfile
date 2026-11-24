# The notebook runtime of BigQuery Studio's notebooks (#2094), CloudBurrow's
# stand-in for a Colab Enterprise runtime. The console builds it on the host
# the first time a notebook is connected, then runs kernel.py in it on a
# `docker network create --internal` network whose one peer (relay.py, in
# the same image) relays the instance's BigQuery REST port, so a notebook's
# code reaches that and nothing else.
#
# Google's own packages, pinned: google-cloud-bigquery and bigquery-magics
# (%%bigquery), with pydata-google-auth (Apache-2.0). Third-party, pinned,
# BSD or Apache licensed, installed here and never part of the CLI: IPython
# and ipykernel (the cells' shell), pandas, pyarrow and db-dtypes (query
# results as DataFrames), matplotlib (inline plots), and for Spark
# notebooks websockets (BSD-3-Clause), tqdm (MPL-2.0 and MIT) and
# packaging (Apache-2.0 or BSD-2-Clause). The base is the Docker
# official python image, pinned by its multi-arch index digest
# (dependencies.json's bigquery.notebookRuntimeBase).
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

RUN pip install --no-cache-dir \
      google-cloud-bigquery==3.47.0 \
      bigquery-magics==0.15.2 \
      db-dtypes==1.9.0 \
      ipython==9.9.0 \
      pandas==2.3.3 \
      pyarrow==22.0.0 \
      matplotlib==3.10.7 \
      google-cloud-dataproc==5.32.0 \
      pyspark-client==4.0.1 \
      websockets==17.4 \
      tqdm==4.70.1 \
      packaging==26.3
# An Apache Spark notebook's DataprocSparkSession (#2156): Google's
# dataproc-spark-connect (Apache-2.0), without its pyspark[connect]
# requirement, whose full pyspark (Spark's jars) a Spark Connect client
# does not use: pyspark-client 4.0.1 (Apache-2.0) is the same client alone,
# the version of the runtime 3.0 sessions' Spark.
RUN pip install --no-cache-dir --no-deps dataproc-spark-connect==1.1.0

COPY kernel.py relay.py /opt/cloudburrow/

# Nothing in the notebook runs as root; HOME is the runtime's own.
RUN useradd --create-home --uid 1000 colab
USER 1000:1000
WORKDIR /home/colab
ENV MPLBACKEND=module://matplotlib_inline.backend_inline PYTHONUNBUFFERED=1
ENTRYPOINT ["python3", "-I", "/opt/cloudburrow/kernel.py"]
