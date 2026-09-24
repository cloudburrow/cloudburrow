#!/bin/sh
# Runs on `stop`, before the cluster stops: storage still answers, and the
# bucket list it returns is kept for CI to check.
set -eu
curl -fsS "$STORAGE_EMULATOR_HOST/storage/v1/b?project=$GOOGLE_CLOUD_PROJECT" > "${RUNNER_TEMP:-/tmp}/cb-shutdown-hook.json"
echo "storage answered at shutdown"
