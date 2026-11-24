# Google's clients in a Vertex AI Workbench instance (#2198) reach the
# instance's own APIs. On Google they reach *.googleapis.com, with the
# credentials of the instance's service account from the metadata server.
# Here the credentials are the same (google.auth.default finds the
# metadata server at GCE_METADATA_HOST, which mints the instance's service
# account's tokens), and each client's default endpoint is the relay's
# port for that API: CB_BIGQUERY_ENDPOINT and CB_STORAGE_ENDPOINT. A
# client given an endpoint of its own keeps it; the container has no
# route but the relay's (an --internal Docker network).
#
# A .pth file imports this in every Python process of the instance (the
# kernels, the Jupyter Server, a terminal's python). It does nothing unless
# CB_WORKBENCH is set, and patches each library only when it is imported.
import os
import sys

_ENDPOINTS = {
    "google.cloud.bigquery.client": os.environ.get("CB_BIGQUERY_ENDPOINT", ""),
    "google.cloud.storage.client": os.environ.get("CB_STORAGE_ENDPOINT", ""),
}


def _default_endpoint(cls, endpoint):
    init = cls.__init__

    def local_init(self, *args, **kwargs):
        opts = kwargs.get("client_options")
        if opts is None:
            from google.api_core.client_options import ClientOptions

            kwargs["client_options"] = ClientOptions(api_endpoint=endpoint)
        elif isinstance(opts, dict):
            if not opts.get("api_endpoint"):
                kwargs["client_options"] = dict(opts, api_endpoint=endpoint)
        elif not getattr(opts, "api_endpoint", None):
            opts.api_endpoint = endpoint
        init(self, *args, **kwargs)

    local_init.__wrapped__ = init
    cls.__init__ = local_init


_RESOURCE_MANAGER = os.environ.get("CB_RESOURCEMANAGER_ENDPOINT", "")
_SERVICE_USAGE = os.environ.get("CB_SERVICEUSAGE_ENDPOINT", "")
_DATAPROC = os.environ.get("CB_DATAPROC_ENDPOINT", "")


def _gateway(module):
    """Google's Dataproc JupyterLab plugin's remote kernels are its kernel
    gateway's, https://PROJECT_NUMBER-dot-REGION.kernels.googleusercontent.com
    (dataproc_jupyter_plugin/handlers.py's configure_gateway_client_url);
    here the gateway is the instance's Dataproc API's (/kernels/ on its
    port, internal/service/dataproc's gateway.go). With no Dataproc API
    served, remote kernels are off, as the plugin turns them off when it
    cannot configure the gateway."""
    configure = module.configure_gateway_client_url

    def local_configure(c, log, kernel_gateway_project_number):
        if not _DATAPROC:
            log.warning("This instance serves no Dataproc API: remote kernels are off.")
            return False
        if not configure(c, log, kernel_gateway_project_number):
            return False
        url = str(c.GatewayClient.url)
        host = url.split("://", 1)[-1].split("/", 1)[0]
        label = host.split(".", 1)[0]
        c.GatewayClient.url = "http://" + _DATAPROC + "/kernels/" + label
        log.info("Remote kernel gateway is the instance's: %s", c.GatewayClient.url)
        return True

    local_configure.__wrapped__ = configure
    module.configure_gateway_client_url = local_configure


def _set(attr, value):
    def patch(module):
        if value and hasattr(module, attr):
            setattr(module, attr, value)

    return patch


# Google's JupyterLab plugins (#2198) name Google hosts in module constants;
# each is the relay's port for that API instead.
_MODULES = {
    "bigquery_jupyter_plugin.services.explorer": _set("_RESOURCE_MANAGER_URL", _RESOURCE_MANAGER and _RESOURCE_MANAGER + "/v1/projects"),
    "bigquery_jupyter_plugin.services.serviceusage": _set("_SERVICE_USAGE_URL", _SERVICE_USAGE and _SERVICE_USAGE + "/v1"),
    "dataproc_jupyter_plugin.handlers": _gateway,
}


def _patch(name, module):
    endpoint = _ENDPOINTS.get(name)
    if endpoint and hasattr(module, "Client"):
        _default_endpoint(module.Client, endpoint)
    if name in _MODULES:
        _MODULES[name](module)


class _Finder:
    """Patches a client module right after it is first executed."""

    def find_spec(self, name, path=None, target=None):
        if not _ENDPOINTS.get(name) and name not in _MODULES:
            return None
        for finder in sys.meta_path:
            if finder is self or not hasattr(finder, "find_spec"):
                continue
            spec = finder.find_spec(name, path, target)
            if spec is None or spec.loader is None:
                continue
            loader = spec.loader
            exec_module = loader.exec_module

            def patched(module, _exec=exec_module, _name=name):
                _exec(module)
                _patch(_name, module)

            try:
                loader.exec_module = patched
            except Exception:
                return None
            return spec
        return None


if os.environ.get("CB_WORKBENCH") and not any(isinstance(f, _Finder) for f in sys.meta_path):
    sys.meta_path.insert(0, _Finder())
