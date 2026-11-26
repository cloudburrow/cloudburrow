# The stand-in remote compute host for `up --compute ssh://` (#2209): a
# Linux host with Docker Engine and sshd, as phase 2's provisioners make
# one on a VM. Docker in Docker, so it must run --privileged. Test-only:
# never shipped, and never run by cloudburrow itself.
#
# Base: docker:27.5.1-dind, pinned by its multi-platform index digest
# (dependencies.json components.realTools.dockerDind; Apache-2.0, with
# Alpine and its packages). openssh-server is Alpine's OpenSSH (BSD).
FROM docker:27.5.1-dind@sha256:aa3df78ecf320f5fafdce71c659f1629e96e9de0968305fe1de670e0ca9176ce

RUN apk upgrade --no-cache openssh-client-common openssh-keygen && apk add --no-cache openssh-server \
 && ssh-keygen -A \
 && (getent group docker >/dev/null || addgroup -S docker) \
 && adduser -D -s /bin/sh cb \
 && addgroup cb docker \
 && sed -i 's/^cb:!/cb:*/' /etc/shadow \
 && mkdir -p /home/cb/.ssh && chmod 700 /home/cb/.ssh && chown cb:cb /home/cb/.ssh

# What a provisioned host's sshd allows (docs/remote-compute.md): keys
# only, TCP and stream-local forwarding, and reverse forwards bound on an
# address the client names (the Docker networks' gateways), which only the
# host and its containers can reach.
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/remotehost-entrypoint.sh

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/remotehost-entrypoint.sh"]
