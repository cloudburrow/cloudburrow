# A Vertex AI Workbench instance's Jupyter Server (#2198): JupyterLab on
# port 8080, Workbench's ("If the default Jupyter port 8080 is changed,
# idle shutdown can't detect kernel activity"), its files the instance's
# data disk, /home/jupyter. The console's proxy is its only client: it
# reaches the server through the relay and sends the server's token
# (JUPYTER_TOKEN) with every request, as Google's proxy fronts an
# instance's JupyterLab. The proxy keeps the browser's Host, so the
# server's own Origin checks compare the page's origin with it.
c = get_config()  # noqa: F821

c.ServerApp.ip = "0.0.0.0"
c.ServerApp.port = 8080
c.ServerApp.port_retries = 0
c.ServerApp.open_browser = False
c.ServerApp.root_dir = "/home/jupyter"
c.ServerApp.allow_remote_access = True
c.ServerApp.trust_xheaders = True
c.ServerApp.terminado_settings = {"shell_command": ["/bin/bash", "-l"]}
c.ServerApp.terminals_enabled = True
# Kernels and terminals report their activity (idle shutdown reads it).
c.ServerApp.shutdown_no_activity_timeout = 0
# Nothing in the instance reaches the internet: JupyterLab's news and
# update checks and the extension manager's PyPI listing are off.
c.LabApp.news_url = None
c.LabApp.check_for_updates_class = "jupyterlab.NeverCheckForUpdate"
c.LabApp.extension_manager = "readonly"
# A JupyterLab page's calls to the instance's APIs (Google's Dataproc
# plugin's browser side calls them itself) go through the server
# (cloudburrow_endpoints' /cloudburrow-api/ route): nothing but the
# instance reaches the relay.
c.ServerApp.jpserver_extensions.update({"cloudburrow_endpoints": True})
