#!/usr/bin/env bash
# endpoint-inventory.sh: regenerate internal/unserved's inventories of the
# endpoint settings the pinned Google tools have (#1975), after a pin bump
# in dependencies.json's realTools.
#
#   scripts/endpoint-inventory.sh GCLOUD_SDK_ROOT
#
# GCLOUD_SDK_ROOT is an unpacked google-cloud-cli archive of the pinned
# version (the one ci.yml downloads and checks). Terraform needs no
# credentials: `terraform providers schema -json` only reads the providers
# init installs. Nothing here calls a Google API.
set -euo pipefail

sdk=${1:?usage: scripts/endpoint-inventory.sh GCLOUD_SDK_ROOT}
repo=$(cd "$(dirname "$0")/.." && pwd)
out=$repo/internal/unserved
provider=$(jq -r '.components.realTools.hashicorpGoogleProvider.version' "$repo/dependencies.json")
gcloud=$(jq -r '.components.realTools.googleCloudCli.version' "$repo/dependencies.json")

work=$(mktemp -d)
trap 'rm -r "$work"' EXIT
cat >"$work/main.tf" <<EOF
terraform {
  required_providers {
    google      = { source = "hashicorp/google", version = "$provider" }
    google-beta = { source = "hashicorp/google-beta", version = "$provider" }
  }
}
EOF
(cd "$work" && CHECKPOINT_DISABLE=1 terraform init -input=false -no-color >/dev/null && terraform providers schema -json >schema.json)
for p in google google-beta; do
  {
    echo "# Every *_custom_endpoint argument of hashicorp/$p $provider, from \`terraform providers schema -json\` (scripts/endpoint-inventory.sh, #1975)."
    jq -r --arg p "registry.terraform.io/hashicorp/$p" \
      '.provider_schemas[$p].provider.block.attributes | keys[] | select(endswith("_custom_endpoint"))' "$work/schema.json" | LC_ALL=C sort
  } >"$out/terraform-$p-$provider.txt"
done

{
  echo "# Every api_endpoint_overrides property of gcloud $gcloud, from its lib/googlecloudsdk/core/properties.py (scripts/endpoint-inventory.sh, #1975)."
  awk '/^class _SectionApiEndpointOverrides/{f=1;next} f&&/^class /{exit} f' "$sdk/lib/googlecloudsdk/core/properties.py" |
    tr -d '\n' | grep -oE "_Add\(\s*'[a-z0-9_]+'" | sed -E "s/.*'(.*)'/\1/" | LC_ALL=C sort
} >"$out/gcloud-$gcloud.txt"

echo "wrote $out/terraform-google-$provider.txt, terraform-google-beta-$provider.txt and gcloud-$gcloud.txt;"
echo "update the versions and embeds in internal/unserved/unserved.go, and TerraformNoEnv and GcloudUnoverridable by hand"
