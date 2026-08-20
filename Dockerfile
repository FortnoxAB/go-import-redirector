FROM alpine:3.22
RUN apk add --no-cache git openssh-client \
 && adduser -S -D -h /home/redirector redirector
COPY go-import-redirector /
USER redirector
ENTRYPOINT ["/go-import-redirector"]
