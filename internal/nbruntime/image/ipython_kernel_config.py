# A Vertex AI Workbench instance's Python 3 kernels (#2198): %%bigquery
# (bigquery-magics) is loaded in every kernel, and inline plots are on.
c = get_config()  # noqa: F821

c.InteractiveShellApp.extensions = ["bigquery_magics"]
c.InteractiveShellApp.matplotlib = "inline"
