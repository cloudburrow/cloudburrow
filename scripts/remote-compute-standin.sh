#!/usr/bin/env bash
# remote-compute-standin.sh: start the stand-in remote compute host for
# `up --compute ssh://` (#2209) and its tests (docs/remote-compute.md).
#
# Builds testdata/remotehost (docker:27.5.1-dind by digest, plus Alpine's
# OpenSSH), runs it --privileged with its sshd on 127.0.0.1:<port> only,
# makes a throwaway bootstrap key in <dir> standing in for the user's own
# agent and config, and authorizes it in the stand-in. With an instance
# name it also makes that instance's own CloudBurrow key, where `up` looks
# for it, already authorized, so `up` signs in with it alone.
#
# Usage: scripts/remote-compute-standin.sh <dir> <port> [<state-dir> <instance>]
# Prints the CLOUDBURROW_TEST_REMOTE_* variables and the container's and
# image's IDs, for eval. Remove both by ID when done (the script prints the
# commands); the stand-in's Docker volume goes with `docker rm -v`.
set -euo pipefail

dir=${1:?usage: remote-compute-standin.sh <dir> <port> [<state-dir> <instance>]}
port=${2:?usage: remote-compute-standin.sh <dir> <port> [<state-dir> <instance>]}
state=${3:-}
instance=${4:-}
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$dir"
chmod 700 "$dir"
dir=$(cd "$dir" && pwd)
name="cb-remotehost-$port"

image=$(docker build -q "$root/testdata/remotehost")
container=$(docker run -d --privileged --name "$name" -p "127.0.0.1:$port:22" "$image")

if [ ! -f "$dir/bootstrap" ]; then
  ssh-keygen -q -t ed25519 -N '' -C cloudburrow-standin-bootstrap -f "$dir/bootstrap"
fi
keys=$(cat "$dir/bootstrap.pub")
if [ -n "$state" ] && [ -n "$instance" ]; then
  kdir="$state/$instance/remote/cb_at_127.0.0.1_$port"
  mkdir -p "$kdir"
  chmod 700 "$state" "$state/$instance" "$state/$instance/remote" "$kdir"
  if [ ! -f "$kdir/id_ed25519" ]; then
    ssh-keygen -q -t ed25519 -N '' -C "cloudburrow-$instance" -f "$kdir/id_ed25519"
  fi
  echo "ssh://cb@127.0.0.1:$port" > "$kdir/id_ed25519.authorized"
  chmod 600 "$kdir"/*
  keys="$keys
$(cat "$kdir/id_ed25519.pub")"
fi
for _ in $(seq 1 30); do
  docker exec "$container" docker version >/dev/null 2>&1 && break
  sleep 1
done
printf '%s\n' "$keys" | docker exec -i "$container" sh -c \
  'cat > /home/cb/.ssh/authorized_keys && chown cb:cb /home/cb/.ssh/authorized_keys && chmod 600 /home/cb/.ssh/authorized_keys'

echo "export CLOUDBURROW_TEST_REMOTE_HOST='ssh://cb@127.0.0.1:$port'"
echo "export CLOUDBURROW_TEST_REMOTE_BOOTSTRAP_KEY='$dir/bootstrap'"
echo "export CLOUDBURROW_TEST_REMOTE_STANDIN='$name'"
echo "# container $container, image $image"
echo "# when done: docker rm -f -v $container; docker rmi $image; and delete $dir"
