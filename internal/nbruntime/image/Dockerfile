# The notebook runtime (internal/nbruntime): BigQuery Studio's notebooks'
# (#2094) and Colab Enterprise runtimes' kernel (kernel.py), and Vertex AI
# Workbench instances' JupyterLab (#2198). The console builds it on the
# host the first time a runtime or instance starts, and runs it on a
# `docker network create --internal` network whose one peer (relay.py, in
# the same image) relays the instance's served APIs, so the code in it
# reaches those and nothing else.
#
# Every PyPI package is pinned and hash-locked (pip --require-hashes):
# requirements.txt and spark.txt are `uv pip compile --universal
# --generate-hashes` of requirements.in and spark.in. Google's own:
# google-cloud-bigquery and bigquery-magics (%%bigquery),
# google-cloud-storage, google-cloud-dataproc and dataproc-spark-connect,
# google-auth and pydata-google-auth. Third-party, BSD, MIT, Apache or
# MPL licensed, installed here and never part of the CLI: JupyterLab and
# Jupyter Server (BSD-3-Clause) with ipykernel, terminado and IPython
# (BSD-3-Clause), pandas, pyarrow, db-dtypes, matplotlib, websockets,
# tqdm, packaging and their dependencies. The base is the Docker official
# python image, pinned by its multi-arch index digest (dependencies.json's
# bigquery.notebookRuntimeBase).
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

COPY requirements.txt spark.txt /opt/cloudburrow/
RUN pip install --no-cache-dir --require-hashes -r /opt/cloudburrow/requirements.txt
# An Apache Spark notebook's DataprocSparkSession (#2156): Google's
# dataproc-spark-connect, without its pyspark[connect] requirement, whose
# full pyspark (Spark's jars) a Spark Connect client does not use:
# pyspark-client 4.0.1 is the same client alone, the version of the
# runtime 3.0 sessions' Spark.
RUN pip install --no-cache-dir --no-deps --require-hashes -r /opt/cloudburrow/spark.txt

COPY kernel.py relay.py /opt/cloudburrow/
# A Workbench instance's Python points Google's clients at the instance's
# APIs (cloudburrow_endpoints, loaded by a .pth file in every Python of the
# instance), and its Jupyter Server and kernels are configured as
# Workbench's: JupyterLab on port 8080, files in /home/jupyter.
COPY cloudburrow_endpoints.py /usr/local/lib/python3.12/site-packages/
COPY jupyter_server_config.py /etc/jupyter/
COPY ipython_kernel_config.py /etc/ipython/
# Nothing in the instance reaches the internet, so JupyterLab neither
# fetches Jupyter's news nor asks whether to.
COPY overrides.json /usr/local/share/jupyter/lab/settings/
RUN echo "import cloudburrow_endpoints" > /usr/local/lib/python3.12/site-packages/cloudburrow_endpoints.pth

# Nothing in a notebook runs as root. A Colab runtime's HOME is colab's; a
# Workbench instance's is /home/jupyter, its data disk.
RUN useradd --create-home --uid 1000 colab && useradd --create-home --uid 1001 --shell /bin/bash jupyter
USER 1000:1000
WORKDIR /home/colab
ENV MPLBACKEND=module://matplotlib_inline.backend_inline PYTHONUNBUFFERED=1
ENTRYPOINT ["python3", "-I", "/opt/cloudburrow/kernel.py"]
