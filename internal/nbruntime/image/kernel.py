# The notebook runtime's kernel (internal/nbruntime): an IPython shell that
# runs one cell per request line on stdin and answers each with the cell's
# outputs, as nbformat 4 output objects, on a line of its own. The shell is
# IPython's, so %%bigquery (bigquery-magics), other magics and the last
# expression's value behave as they do in a Colab Enterprise notebook.
#
# BigQuery is the instance's: the client's endpoint is CB_BIGQUERY_ENDPOINT,
# its credentials anonymous (the instance takes every caller to be its one
# identity) and its project GOOGLE_CLOUD_PROJECT, set for
# google.cloud.bigquery.Client() and %%bigquery alike. A run that executes
# as a service account or a user (#2173) is given that identity's bearer
# token in CB_ACCESS_TOKEN, which the clients send instead. The container
# has no route but that one (an --internal Docker network).
import io
import json
import os
import re
import sys

PROJECT = os.environ.get("GOOGLE_CLOUD_PROJECT", "")
ENDPOINT = os.environ.get("CB_BIGQUERY_ENDPOINT", "")
TOKEN = os.environ.pop("CB_ACCESS_TOKEN", "")
DATAPROC = os.environ.get("CB_DATAPROC_ENDPOINT", "")
MAX_TEXT = 1 << 20

# Answers go to the original stdout; the cells' own output is captured.
proto = os.fdopen(os.dup(1), "w", buffering=1)
requests = sys.stdin
sys.stdin = io.StringIO("")

ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
outputs = []


def clip(s):
    s = str(s)
    return s if len(s) <= MAX_TEXT else s[:MAX_TEXT] + "\n… (output truncated)"


def mime(data):
    out = {}
    for k, v in (data or {}).items():
        if k.startswith("text/") or k.startswith("image/") or k == "application/json":
            out[k] = v if isinstance(v, (dict, list)) else clip(v)
    return out


class Stream(io.TextIOBase):
    def __init__(self, name):
        self.name = name

    def writable(self):
        return True

    def write(self, s):
        if not s:
            return 0
        if outputs and outputs[-1].get("output_type") == "stream" and outputs[-1]["name"] == self.name:
            outputs[-1]["text"] = clip(outputs[-1]["text"] + s)
        else:
            outputs.append({"output_type": "stream", "name": self.name, "text": clip(s)})
        return len(s)


from traitlets import Type  # noqa: E402
from IPython.core.displayhook import DisplayHook  # noqa: E402
from IPython.core.displaypub import DisplayPublisher  # noqa: E402
from IPython.core.interactiveshell import InteractiveShell  # noqa: E402


class Hook(DisplayHook):
    def write_output_prompt(self):
        pass

    def write_format_data(self, format_dict, md_dict=None):
        outputs.append({"output_type": "execute_result", "execution_count": self.prompt_count,
                        "data": mime(format_dict), "metadata": {}})

    def finish_displayhook(self):
        pass


class Pub(DisplayPublisher):
    def publish(self, data, metadata=None, source=None, *, transient=None, update=False, **kwargs):
        outputs.append({"output_type": "display_data", "data": mime(data), "metadata": metadata or {}})

    def clear_output(self, wait=False):
        outputs.clear()


class Shell(InteractiveShell):
    displayhook_class = Type(Hook)
    display_pub_class = Type(Pub)

    def _showtraceback(self, etype, evalue, stb):
        outputs.append({"output_type": "error", "ename": getattr(etype, "__name__", "Error"),
                        "evalue": clip(evalue), "traceback": [ANSI.sub("", clip(line)) for line in stb]})


shell = Shell.instance()


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


notes = []
if DATAPROC:
    try:
        setup_dataproc()
    except Exception as err:  # the shell still runs Python
        notes.append("Dataproc client setup failed: %s" % err)
try:
    setup_bigquery()
except Exception as err:  # the shell still runs Python
    notes.append("BigQuery client setup failed: %s" % err)
try:
    shell.run_line_magic("matplotlib", "inline")
except Exception:
    pass
outputs.clear()

proto.write(json.dumps({"ready": True, "python": sys.version.split()[0], "notes": notes}) + "\n")

while True:
    try:
        line = requests.readline()
    except KeyboardInterrupt:  # an interrupt between cells
        continue
    if not line:
        break
    try:
        req = json.loads(line)
    except ValueError:
        continue
    outputs.clear()
    real = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = Stream("stdout"), Stream("stderr")
    ok = False
    try:
        result = shell.run_cell(req.get("code", ""), store_history=True)
        ok = result.success
    except KeyboardInterrupt:
        outputs.append({"output_type": "error", "ename": "KeyboardInterrupt", "evalue": "", "traceback": ["KeyboardInterrupt"]})
    except BaseException as err:  # noqa: BLE001  a cell's SystemExit
        outputs.append({"output_type": "error", "ename": type(err).__name__, "evalue": clip(err), "traceback": [repr(err)]})
    finally:
        sys.stdout, sys.stderr = real
    proto.write(json.dumps({"status": "ok" if ok else "error", "execution_count": shell.execution_count - 1,
                            "outputs": list(outputs)}, default=str) + "\n")
