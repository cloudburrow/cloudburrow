# The notebook runtime (internal/nbruntime): BigQuery Studio's notebooks'
# (#2094) and Colab Enterprise runtimes' Jupyter Server and kernels, and
# Vertex AI Workbench instances' JupyterLab (#2198): the same JupyterLab. The console builds it on the
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
# google-auth and pydata-google-auth, and Workbench's JupyterLab plugins
# (dataproc-jupyter-plugin, bigquery-jupyter-plugin, gcs-jupyter-plugin,
# google-cloud-jupyter-config, kernels-mixer, scheduler-jupyter-plugin;
# Apache-2.0). Third-party, BSD, MIT, Apache or
# MPL licensed, installed here and never part of the CLI: JupyterLab and
# Jupyter Server (BSD-3-Clause) with ipykernel, terminado and IPython
# (BSD-3-Clause), pandas, pyarrow, db-dtypes, matplotlib, websockets,
# tqdm, packaging and their dependencies. The base is the Docker official
# python image, pinned by its multi-arch index digest (dependencies.json's
# bigquery.notebookRuntimeBase).
FROM python:3.12-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b

# pip checks each wheel's SHA-256 and installs nothing the lock does not
# list (--no-deps); it builds only pyspark-client, an sdist alone, with the
# lock's own setuptools (build-requirements.txt, installed first, and
# --no-build-isolation, so no build requirement is fetched beside the lock).
COPY requirements.txt spark.txt build-requirements.txt /opt/cloudburrow/
RUN pip install --no-cache-dir --require-hashes --no-deps --only-binary :all: -r /opt/cloudburrow/build-requirements.txt && \
    pip install --no-cache-dir --require-hashes --no-deps --no-build-isolation --only-binary :all: --no-binary pyspark-client \
      -r /opt/cloudburrow/requirements.txt
# An Apache Spark notebook's DataprocSparkSession (#2156): Google's
# dataproc-spark-connect, without its pyspark[connect] requirement, whose
# full pyspark (Spark's jars) a Spark Connect client does not use:
# pyspark-client 4.0.1 is the same client alone, the version of the
# runtime 3.0 sessions' Spark.
RUN pip install --no-cache-dir --no-deps --require-hashes --only-binary :all: -r /opt/cloudburrow/spark.txt

# A Workbench instance's Google Cloud CLI (fetch_gcloud.py: Google's
# versioned archive, SHA-256 pinned), which Google's JupyterLab plugins
# (dataproc-jupyter-plugin, bigquery-jupyter-plugin, gcs-jupyter-plugin,
# in the lock) read their configuration and credentials from.
COPY fetch_gcloud.py /opt/cloudburrow/
RUN python3 -I /opt/cloudburrow/fetch_gcloud.py && rm -f /tmp/google-cloud-cli.tar.gz && \
    ln -s /opt/google-cloud-sdk/bin/gcloud /usr/local/bin/gcloud
ENV CLOUDSDK_PYTHON=/usr/local/bin/python3
COPY relay.py /opt/cloudburrow/
# A Colab runtime's kernels load cloudburrow_colab (ipython_kernel_config.py).
COPY cloudburrow_colab.py /usr/local/lib/python3.12/site-packages/
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
# Workbench's JupyterLab plugins are a Workbench instance's: a Colab or
# BigQuery Studio runtime's Jupyter Server (JUPYTER_CONFIG_PATH, set for
# those alone) loads none of them.
RUN mkdir -p /etc/cloudburrow/colab/labconfig && \
    echo '{"ServerApp":{"jpserver_extensions":{"dataproc_jupyter_plugin":false,"bigquery_jupyter_plugin":false,"gcs_jupyter_plugin":false,"scheduler_jupyter_plugin":false,"google.cloud.jupyter_config":false}}}' \
      > /etc/cloudburrow/colab/jupyter_server_config.json && \
    echo '{"disabledExtensions":{"dataproc_jupyter_plugin":true,"sparkmonitor":true,"bigquery-jupyter-plugin":true,"gcs-jupyter-plugin":true,"scheduler-jupyter-plugin":true}}' \
      > /etc/cloudburrow/colab/labconfig/page_config.json

# Nothing in a notebook runs as root. A Colab runtime's HOME is colab's; a
# Workbench instance's is /home/jupyter, its data disk.
RUN useradd --create-home --uid 1000 colab && useradd --create-home --uid 1001 --shell /bin/bash jupyter
USER 1000:1000
WORKDIR /home/colab
ENV MPLBACKEND=module://matplotlib_inline.backend_inline PYTHONUNBUFFERED=1
ENTRYPOINT ["jupyter", "lab"]
