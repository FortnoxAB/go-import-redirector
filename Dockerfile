FROM alpine:3.22
# Probes run non-interactively: accept an unknown host's key on first connect
# (instead of failing every probe with "Host key verification failed") and
# never prompt. System-wide, so it still applies when GIT_SSH_COMMAND is set.
RUN apk add --no-cache git openssh-client \
 && printf 'Host *\n    StrictHostKeyChecking accept-new\n    BatchMode yes\n' > /etc/ssh/ssh_config.d/go-import-redirector.conf \
 && adduser -S -D -h /home/redirector redirector
COPY go-import-redirector /
USER redirector
ENTRYPOINT ["/go-import-redirector"]
