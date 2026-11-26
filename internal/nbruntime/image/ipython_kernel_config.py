# The runtime's Python 3 kernels: a Vertex AI Workbench instance's (#2198)
# load %%bigquery (bigquery-magics); a Colab Enterprise runtime's and a
# BigQuery Studio notebook's (CB_COLAB) load cloudburrow_colab, which
# points BigQuery and Dataproc at the instance and loads %%bigquery. Inline
# plots are on in both.
import os

c = get_config()  # noqa: F821

c.InteractiveShellApp.extensions = ["cloudburrow_colab"] if os.environ.get("CB_COLAB") else ["bigquery_magics"]
c.InteractiveShellApp.matplotlib = "inline"
