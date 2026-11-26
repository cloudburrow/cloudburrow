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


_RELAY = "http://host.docker.internal:"
_API_PORTS = [p for p in os.environ.get("CB_API_PORTS", "").split(",") if p]
_API_ROUTE = "cloudburrow-api"


def _for_browser(handler, url):
    """The browser's URL for a relay URL: the instance's JupyterLab's own
    /cloudburrow-api/PORT/ route (_ApiHandler), on the page's origin."""
    if not isinstance(url, str) or not url.startswith(_RELAY):
        return url
    port, _, rest = url[len(_RELAY):].partition("/")
    if port not in _API_PORTS:
        return url
    base = handler.settings.get("base_url", "/")
    return "%s://%s%s%s/%s/%s" % (handler.request.protocol, handler.request.host, base, _API_ROUTE, port, rest)


def _browser_urls(module):
    """The Dataproc plugin's browser side calls Google's APIs itself, at the
    URLs its getGcpServiceUrls answers (the gcloud api_endpoint_overrides,
    read server-side). Those are the relay's, which only the instance
    reaches, so the browser is given the instance's own route to each."""
    handler = module.UrlHandler
    get = handler.get

    async def local_get(self):
        url_map = await module.urls.map()
        self.finish({k: _for_browser(self, v) for k, v in url_map.items()})

    import tornado.web

    local_get.__wrapped__ = get
    handler.get = tornado.web.authenticated(local_get)


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
    "bigquery_jupyter_plugin.services.credentials": _set("_USERINFO_URL", os.environ.get("GCE_METADATA_HOST") and "http://" + os.environ["GCE_METADATA_HOST"] + "/oauth2/v3/userinfo"),
    "dataproc_jupyter_plugin.handlers": lambda m: (_gateway(m), _browser_urls(m)),
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


# The /cloudburrow-api/PORT/ route (#2198): a Jupyter Server extension of a
# Workbench instance (jupyter_server_config.py), which relays a JupyterLab
# page's calls to an API of the instance, as the page's calls to
# *.googleapis.com reach Google. The page is the instance's JupyterLab's,
# signed in to it; each call is sent with the instance's service account's
# token, which is the one the page itself holds (the plugins' credentials
# handler reads gcloud's, from the metadata server).
def _jupyter_server_extension_points():
    return [{"module": "cloudburrow_endpoints"}]


def _load_jupyter_server_extension(server_app):
    if not _API_PORTS:
        return
    import google.auth
    import google.auth.transport.requests
    import tornado.httpclient
    import tornado.web
    from jupyter_server.base.handlers import APIHandler
    from jupyter_server.utils import url_path_join

    creds, _ = google.auth.default(scopes=["https://www.googleapis.com/auth/cloud-platform"])
    client = tornado.httpclient.AsyncHTTPClient()

    class _ApiHandler(APIHandler):
        def check_xsrf_cookie(self):
            # The page sends its own Authorization, so Jupyter's token
            # check, not the XSRF cookie, authenticates it (the console's
            # proxy adds the server's token).
            return None

        @tornado.web.authenticated
        async def _relay(self, port, path):
            if port not in _API_PORTS:
                raise tornado.web.HTTPError(404)
            if not creds.valid:
                await tornado.ioloop.IOLoop.current().run_in_executor(None, creds.refresh, google.auth.transport.requests.Request())
            url = _RELAY + port + "/" + path
            if self.request.query:
                url += "?" + self.request.query
            headers = {"Authorization": "Bearer " + creds.token}
            for h in ("Content-Type", "Accept", "X-Goog-Upload-Protocol", "X-Goog-Upload-Command", "X-Goog-Upload-Offset", "X-Goog-Upload-Header-Content-Type"):
                if h in self.request.headers:
                    headers[h] = self.request.headers[h]
            method = self.request.method
            body = self.request.body if method in ("POST", "PUT", "PATCH") else None
            if method in ("POST", "PUT", "PATCH") and body is None:
                body = b""
            resp = await client.fetch(url, method=method, headers=headers, body=body, raise_error=False,
                                      allow_nonstandard_methods=True, request_timeout=600, follow_redirects=False)
            self.set_status(resp.code if resp.code >= 100 else 502)
            for h in ("Content-Type", "Location", "X-Goog-Upload-URL", "X-Goog-Upload-Status"):
                v = resp.headers.get(h)
                if v:
                    self.set_header(h, _for_browser(self, v) if h in ("Location", "X-Goog-Upload-URL") else v)
            if resp.body:
                self.write(resp.body)
            await self.finish()

        get = post = put = patch = delete = _relay

    route = url_path_join(server_app.web_app.settings["base_url"], _API_ROUTE, r"(\d+)/(.*)")
    server_app.web_app.add_handlers(".*$", [(route, _ApiHandler)])
    server_app.log.info("Relaying JupyterLab's API calls at %s", route)
