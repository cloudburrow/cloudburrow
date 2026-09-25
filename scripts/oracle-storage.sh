#!/bin/sh
# oracle-storage.sh — the Cloud Storage differential oracle (#497), CI only.
#
# Runs storage-testbench, pinned by the image digest in dependencies.json,
# with no network at all (--network none), and runs the oracle test binary
# in a second container that shares only the testbench's loopback. The
# builtin server runs inside the test binary. Nothing here reaches any
# network, and nothing touches live Google. It removes only the container it
# started.
#
# Usage: scripts/oracle-storage.sh    (needs docker, go, jq; linux/amd64)
set -eu

IMAGE=$(jq -r '.components.testOracles.storageTestbench | .image + "@" + .digest' dependencies.json)
NAME=cloudburrow-oracle-testbench
BIN=$(mktemp -d)
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$BIN"' EXIT

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags oracle -o "$BIN/oracle.test" ./test/oracle/storage
docker pull --quiet "$IMAGE"
docker run -d --name "$NAME" --network none "$IMAGE" >/dev/null
docker run --rm --network "container:$NAME" \
	-e CLOUDBURROW_ORACLE_TESTBENCH=http://127.0.0.1:9000 \
	-v "$BIN/oracle.test:/oracle.test:ro" --entrypoint /oracle.test \
	"$IMAGE" -test.v -test.count=1 -test.timeout=10m || {
	echo "--- testbench log"
	docker logs "$NAME" 2>&1 | tail -50
	exit 1
}
