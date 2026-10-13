FROM alpine:3.22
# Probes run non-interactively: accept an unknown host's key on first connect
# (instead of failing every probe with "Host key verification failed") and
# never prompt. System-wide, so it still applies when GIT_SSH_COMMAND is set.
# Accepted keys go to /tmp, which is writable whatever UID the pod runs as
# (e.g. runAsUser: 65534, whose home "/" is not).
# tini is PID 1 so it reaps the ssh processes git orphans when ls-remote fails:
# the Go binary never wait()s on children it did not start, so without an init
# every failed probe leaves a zombie behind.
RUN apk add --no-cache git openssh-client tini \
 && printf 'Host *\n    StrictHostKeyChecking accept-new\n    BatchMode yes\n    UserKnownHostsFile /tmp/known_hosts\n' > /etc/ssh/ssh_config.d/go-import-redirector.conf \
 && adduser -S -D -h /home/redirector redirector
COPY go-import-redirector /
USER redirector
ENTRYPOINT ["/sbin/tini", "--", "/go-import-redirector"]
