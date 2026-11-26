#!/bin/sh
# Starts one Python SDK worker of a job's worker pool (internal/beamrun):
# Beam's boot, with a semi-persistent directory of the worker's own.
#
# A job's Python environments share one pool, and so one container, where
# Dataflow gives each environment's workers a container of their own;
# boot downloads an environment's staged files to
# SEMI_PERSIST_DIR/staged and installs its requirements from there, so two
# environments' workers starting together (a pipeline with a Beam YAML
# Python provider's packages, or a cross-language Python transform) would
# write over each other's requirements.txt in the default /tmp.
id=""
for a in "$@"; do
	case "$a" in
	--id=*) id="${a#--id=}" ;;
	esac
done
dir="/tmp/cb-worker-$id"
mkdir -p "$dir"
exec /opt/apache/beam/boot --semi_persist_dir="$dir" "$@"
