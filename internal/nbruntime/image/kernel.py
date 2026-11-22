# The notebook runtime's kernel (internal/nbruntime): an IPython shell that
# runs one cell per request line on stdin and answers each with the cell's
# outputs, as nbformat 4 output objects, on a line of its own. The shell is
# IPython's, so %%bigquery (bigquery-magics), other magics and the last
# expression's value behave as they do in a Colab Enterprise notebook.
#
# BigQuery is the instance's: the client's endpoint is CB_BIGQUERY_ENDPOINT,
# its credentials anonymous (the instance takes every caller to be its one
# identity) and its project GOOGLE_CLOUD_PROJECT, set for
# google.cloud.bigquery.Client() and %%bigquery alike. The container has no
# route but that one (an --internal Docker network).
import io
import json
import os
import re
import sys

PROJECT = os.environ.get("GOOGLE_CLOUD_PROJECT", "")
ENDPOINT = os.environ.get("CB_BIGQUERY_ENDPOINT", "")
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


def setup_bigquery():
    from google.api_core.client_options import ClientOptions
    from google.auth.credentials import AnonymousCredentials
    from google.cloud import bigquery

    init = bigquery.Client.__init__

    def local_init(self, *args, **kwargs):
        if not args and kwargs.get("project") is None:
            kwargs["project"] = PROJECT
        if len(args) < 2 and kwargs.get("credentials") is None:
            kwargs["credentials"] = AnonymousCredentials()
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
    ctx.credentials = AnonymousCredentials()
    ctx.bigquery_client_options = ClientOptions(api_endpoint=ENDPOINT)
    try:
        ctx.progress_bar_type = None
    except Exception:
        pass


notes = []
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
