#!/bin/bash
# A Jupyter session's kernel server (internal/sparkproc, #2162): Jupyter
# Server on port 8888 of the session's container network, its REST and
# websocket API the kernel gateway's backend.
set -e
mkdir -p /tmp/cb-notebooks
exec jupyter server --ip=0.0.0.0 --port=8888 --no-browser --ServerApp.token= --ServerApp.password= \
  --ServerApp.allow_origin='*' --ServerApp.allow_remote_access=True --ServerApp.disable_check_xsrf=True \
  --ServerApp.root_dir=/tmp/cb-notebooks
