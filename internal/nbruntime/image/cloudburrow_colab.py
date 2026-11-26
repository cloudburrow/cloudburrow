# A Colab Enterprise runtime's (and a BigQuery Studio notebook's) kernel
# setup (internal/nbruntime): an IPython extension every ipykernel kernel
# of a runtime's Jupyter Server loads (ipython_kernel_config.py, when
# CB_COLAB is set), as a Colab runtime's kernel is a Jupyter kernel.
#
# BigQuery is the instance's: the client's endpoint is CB_BIGQUERY_ENDPOINT,
# its credentials anonymous (the instance takes every caller to be its one
# identity) and its project GOOGLE_CLOUD_PROJECT, set for
# google.cloud.bigquery.Client() and %%bigquery alike. A run that executes
# as a service account or a user (#2173) is given that identity's bearer
# token in CB_ACCESS_TOKEN, which the clients send instead. An Apache Spark
# notebook's DataprocSparkSession (#2156) reaches the instance's Dataproc
# API. The container has no route but the relay's (an --internal Docker
# network). Setup failures are kept in notes, which the runtime reports.
import os

PROJECT = os.environ.get("GOOGLE_CLOUD_PROJECT", "")
ENDPOINT = os.environ.get("CB_BIGQUERY_ENDPOINT", "")
TOKEN = os.environ.pop("CB_ACCESS_TOKEN", "")
DATAPROC = os.environ.get("CB_DATAPROC_ENDPOINT", "")

notes = []
shell = None


def run_credentials():
    """The run's credentials: its identity's token, else anonymous."""
    from google.auth.credentials import AnonymousCredentials

    if not TOKEN:
        return AnonymousCredentials()
    from google.oauth2.credentials import Credentials

    return Credentials(token=TOKEN)


def setup_bigquery():
    from google.api_core.client_options import ClientOptions
    from google.cloud import bigquery

    init = bigquery.Client.__init__

    def local_init(self, *args, **kwargs):
        if not args and kwargs.get("project") is None:
            kwargs["project"] = PROJECT
        if len(args) < 2 and kwargs.get("credentials") is None:
            kwargs["credentials"] = run_credentials()
        opts = kwargs.get("client_options")
        if opts is None:
            kwargs["client_options"] = ClientOptions(api_endpoint=ENDPOINT)
        elif isinstance(opts, dict):
            if not opts.get("api_endpoint"):
                kwargs["client_options"] = dict(opts, api_endpoint=ENDPOINT)
        elif not getattr(opts, "api_endpoint", None):
            opts.api_endpoint = ENDPOINT
        init(self, *args, **kwargs)

    bigquery.Client.__init__ = local_init
    shell.run_line_magic("load_ext", "bigquery_magics")
    import bigquery_magics

    ctx = bigquery_magics.context
    ctx.project = PROJECT
    ctx.credentials = run_credentials()
    ctx.bigquery_client_options = ClientOptions(api_endpoint=ENDPOINT)
    try:
        ctx.progress_bar_type = None
    except Exception:
        pass


def setup_dataproc():
    """An Apache Spark notebook's session (#2156) is the instance's.

    Google's DataprocSparkSession (google-cloud-dataproc-spark-connect)
    creates a Spark Connect session with google-cloud-dataproc's
    SessionControllerClient and reaches it through its TCP-over-WebSocket
    bridge (wss://HOST/tcp-over-websocket-bridge/...) with a bearer token
    of Application Default Credentials. Here the Dataproc clients use the
    relay's plaintext gRPC port, the bridge the same port over ws://, and
    the credentials are local ones the instance takes, as BigQuery's are
    anonymous.
    """
    import grpc
    import google.auth
    from google.auth import credentials as gcreds
    from google.cloud import dataproc_v1

    class LocalCredentials(gcreds.Credentials):
        def __init__(self):
            super().__init__()
            self.token = TOKEN or "cloudburrow-local"

        def refresh(self, request):
            self.token = TOKEN or "cloudburrow-local"

    creds = LocalCredentials()

    def local_default(scopes=None, request=None, quota_project_id=None, default_scopes=None):
        return creds, PROJECT

    google.auth.default = local_default

    def local(cls):
        init = cls.__init__

        def local_init(self, *args, **kwargs):
            if kwargs.get("transport") is None:
                kwargs.pop("credentials", None)
                kwargs.pop("client_options", None)
                kwargs["transport"] = cls.get_transport_class("grpc")(channel=grpc.insecure_channel(DATAPROC))
            init(self, *args, **kwargs)

        cls.__init__ = local_init

    for name in ("SessionControllerClient", "SessionTemplateControllerClient", "BatchControllerClient"):
        local(getattr(dataproc_v1, name))

    from google.cloud.dataproc_spark_connect.client import proxy

    connect = proxy.websocketclient.connect

    def local_connect(uri, *args, **kwargs):
        if uri.startswith("wss://"):
            uri = "ws://" + DATAPROC + "/" + uri.split("/", 3)[3]
        return connect(uri, *args, **kwargs)

    proxy.websocketclient.connect = local_connect


def load_ipython_extension(ip):
    global shell
    shell = ip
    notes.clear()
    if DATAPROC:
        try:
            setup_dataproc()
        except Exception as err:  # the kernel still runs Python
            notes.append("Dataproc client setup failed: %s" % err)
    try:
        setup_bigquery()
    except Exception as err:  # the kernel still runs Python
        notes.append("BigQuery client setup failed: %s" % err)
