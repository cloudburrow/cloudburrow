# The tools container (test/compat/toolscontainer_test.go, #2174): the
# pinned Google Cloud CLI, Terraform and OpenTofu, with the hashicorp/google
# and google-beta providers pre-fetched into a filesystem mirror, so the
# compat tests run gcloud, gsutil, bq and Terraform on an internal Docker
# network with no route out. Everything is fetched here, at build time, and
# checked against a pinned SHA-256; nothing is fetched when a test runs.
#
# Google's own image, the 593.0.0 release CI pinned as a tarball before
# (#694), by digest. It is published for linux/amd64 only, so the image is
# built and run as linux/amd64 everywhere (emulated on an arm64 host).
FROM google/cloud-sdk:593.0.0-slim@sha256:e8394c66f4f66336af311343ff9a84c84521f8c1113ebc8abfe98bab9a155099
RUN apt-get update && apt-get install -y --no-install-recommends iptables iproute2 unzip ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN set -eu; cd /tmp; \
  get() { curl --retry 5 --retry-all-errors -fsSLo "$1" "$2"; echo "$3  $1" | sha256sum -c -; }; \
  get terraform.zip https://releases.hashicorp.com/terraform/1.16.6/terraform_1.16.6_linux_amd64.zip \
    38034b38d03096399a7a1264a14561a8029b2b496fd1f169452656e53968f1b7; \
  unzip -q terraform.zip terraform -d /usr/local/bin; rm terraform.zip; \
  get tofu.tar.gz https://github.com/opentofu/opentofu/releases/download/v1.12.6/tofu_1.12.6_linux_amd64.tar.gz \
    50a6106fa4de523d09c87af85f3db1dd47535fc005727fdca6852146476b88ec; \
  tar -xzf tofu.tar.gz -C /usr/local/bin tofu; rm tofu.tar.gz; \
  m=/opt/terraform-mirror; \
  mkdir -p $m/registry.terraform.io/hashicorp/google $m/registry.terraform.io/hashicorp/google-beta $m/registry.opentofu.org/hashicorp/google; \
  get $m/registry.terraform.io/hashicorp/google/terraform-provider-google_8.12.0_linux_amd64.zip \
    https://releases.hashicorp.com/terraform-provider-google/8.12.0/terraform-provider-google_8.12.0_linux_amd64.zip \
    eddf30f53a99381388ff25702ed0c727586deb8c502cd5af395dc98afb217a49; \
  get $m/registry.terraform.io/hashicorp/google-beta/terraform-provider-google-beta_8.12.0_linux_amd64.zip \
    https://releases.hashicorp.com/terraform-provider-google-beta/8.12.0/terraform-provider-google-beta_8.12.0_linux_amd64.zip \
    e40ea7bc09cee810da2fe997dbb126916f23c86413234624dd46da1aa9c2cbcf; \
  get $m/registry.opentofu.org/hashicorp/google/terraform-provider-google_8.12.0_linux_amd64.zip \
    https://github.com/opentofu/terraform-provider-google/releases/download/v8.12.0/terraform-provider-google_8.12.0_linux_amd64.zip \
    3d9eee56fd44148a429baee37fb6f631f5cba86b1c2f3072da9bcc2d75f7eb5c; \
  printf 'provider_installation {\n  filesystem_mirror {\n    path = "%s"\n  }\n}\n' $m > /etc/terraformrc; \
  printf '[component_manager]\ndisable_update_check = true\n' >> /usr/lib/google-cloud-sdk/properties; \
  mkdir -m 1777 /home/tools
# Only the mirror: no `direct` installation method, so init never reaches a
# registry. No update checks.
ENV TF_CLI_CONFIG_FILE=/etc/terraformrc CHECKPOINT_DISABLE=1 HOME=/home/tools \
    CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true CLOUDSDK_CORE_DISABLE_USAGE_REPORTING=true
