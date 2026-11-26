# The Google Cloud CLI (gcloud) of a Vertex AI Workbench instance (#2198),
# which Google's JupyterLab plugins read their project, region, credentials
# and endpoints from, as on a Workbench VM. Google's versioned archive for
# the build's architecture, from Google's download host, checked against
# its pinned SHA-256 before it is unpacked into /opt/google-cloud-sdk.
import hashlib
import platform
import sys
import tarfile
import urllib.request

VERSION = "594.0.1"
ARCHIVES = {
    "x86_64": ("x86_64", "8fc798fd0638428551f80277a568112d08afcf9cafd967d1cf39ae0918df2a1d"),
    "aarch64": ("arm", "ab9d36381e5e1de6efb4969ffa71d5c9b4765b7d4a534542dcb0360c062fbbe6"),
}

machine = platform.machine()
if machine not in ARCHIVES:
    sys.exit("no pinned Google Cloud CLI for " + machine)
arch, want = ARCHIVES[machine]
url = "https://dl.google.com/dl/cloudsdk/channels/rapid/downloads/google-cloud-cli-%s-linux-%s.tar.gz" % (VERSION, arch)
path = "/tmp/google-cloud-cli.tar.gz"
urllib.request.urlretrieve(url, path)
h = hashlib.sha256()
with open(path, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        h.update(chunk)
if h.hexdigest() != want:
    sys.exit("google-cloud-cli %s: sha256 %s, want %s" % (arch, h.hexdigest(), want))
with tarfile.open(path) as t:
    t.extractall("/opt", filter="tar")
