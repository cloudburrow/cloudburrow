#!/bin/bash
# The Jupyter component's server: trust the cluster's CA (the Storage
# API's TLS, for the BigQuery connector), then JupyterLab on port 8123 at
# the cluster's Component Gateway path, its notebooks the cluster's.
set -e
cp "$JAVA_HOME/lib/security/cacerts" /tmp/cb-trust.jks
chmod 600 /tmp/cb-trust.jks
"$JAVA_HOME/bin/keytool" -importcert -noprompt -alias cloudburrow-spark -file /cb/tls/ca.pem \
  -keystore /tmp/cb-trust.jks -storepass changeit >/dev/null 2>&1
exec jupyter lab --ip=0.0.0.0 --port=8123 --no-browser --ServerApp.base_url="${CB_BASE_URL:-/}" \
  --ServerApp.token= --ServerApp.password= --ServerApp.allow_origin='*' --ServerApp.allow_remote_access=True \
  --ServerApp.disable_check_xsrf=True --ServerApp.root_dir=/cb-notebooks
