#!/bin/sh
# The second ready hook creates a bucket through the address the instance
# exported to it, as a user's seeding script would (#285).
set -eu
curl -fsS -X POST -H 'Content-Type: application/json' \
  "$STORAGE_EMULATOR_HOST/storage/v1/b?project=$GOOGLE_CLOUD_PROJECT" \
  -d '{"name":"cloudburrow-hook-bucket"}'
echo
echo "created cloudburrow-hook-bucket"
