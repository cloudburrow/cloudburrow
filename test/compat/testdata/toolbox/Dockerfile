# The compat harness's tool box (test/compat/toolbox_test.go, #2174): the
# container every gcloud, gsutil, bq and Terraform command of test/compat
# runs in, on an internal Docker network with no route out. gcloud is the
# harness's own (CLOUDBURROW_TEST_GCLOUD), mounted at its host path; the
# image carries Python for it, iptables for the box's rules, ssh for
# `gcloud compute ssh`, and Terraform and OpenTofu at the versions CI pins
# (.github/workflows/ci.yml), with the hashicorp/google and google-beta
# providers the tests require already in a filesystem mirror: the box has
# no network to fetch them, and its CLI configuration installs from the
# mirror alone. boxd and run.sh are mounted at /box when it runs.
#
# Everything is fetched here, at build time, from HashiCorp's and
# OpenTofu's release servers and registries; nothing from Google.
# dependencies.json's testOracles.compatToolbox* entries record the pins.
FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS fetch
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
ARG TERRAFORM_VERSION=1.16.6
ARG TOFU_VERSION=1.12.6
ARG GOOGLE_PROVIDER_VERSION=8.12.0
RUN set -eu; case "$(uname -m)" in \
      aarch64) a=arm64 tf=a750b5a11038f165846b2658c40c88cd7070aa67a61bab431a14ba27928ea9ea tofu=9bd0228a81bcd0c88f7045c74378f45a815779f19897191dff7d9efba9976b9e ;; \
      x86_64) a=amd64 tf=38034b38d03096399a7a1264a14561a8029b2b496fd1f169452656e53968f1b7 tofu=50a6106fa4de523d09c87af85f3db1dd47535fc005727fdca6852146476b88ec ;; \
      *) echo "no pinned Terraform for $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl --retry 5 --retry-all-errors -fsSLo /tmp/tf.zip "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_$a.zip"; \
    echo "$tf  /tmp/tf.zip" | sha256sum -c -; \
    unzip -q /tmp/tf.zip terraform -d /usr/local/bin; \
    curl --retry 5 --retry-all-errors -fsSLo /tmp/tofu.tar.gz "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_$a.tar.gz"; \
    echo "$tofu  /tmp/tofu.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/tofu.tar.gz -C /usr/local/bin tofu; \
    rm /tmp/tf.zip /tmp/tofu.tar.gz; \
    mkdir /m && cd /m; \
    printf 'terraform {\n  required_providers {\n    google = { source = "hashicorp/google", version = "%s" }\n    google-beta = { source = "hashicorp/google-beta", version = "%s" }\n  }\n}\n' \
      "$GOOGLE_PROVIDER_VERSION" "$GOOGLE_PROVIDER_VERSION" > main.tf; \
    CHECKPOINT_DISABLE=1 terraform providers mirror -platform=linux_$a /mirror; \
    tofu providers mirror -platform=linux_$a /mirror; \
    rm -rf /m

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && apt-get install -y --no-install-recommends python3 iptables iproute2 ca-certificates openssh-client \
 && rm -rf /var/lib/apt/lists/*
COPY --from=fetch /usr/local/bin/terraform /usr/local/bin/tofu /usr/local/bin/
COPY --from=fetch /mirror /opt/box/mirror
COPY terraformrc /opt/box/terraformrc
RUN useradd --system --uid 64999 --no-create-home --shell /usr/sbin/nologin boxd \
 && mkdir -m 1777 /home/box /run/box /run/box/plugins
ENV TF_CLI_CONFIG_FILE=/opt/box/terraformrc CHECKPOINT_DISABLE=1 HOME=/home/box
