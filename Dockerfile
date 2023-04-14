FROM gcr.io/distroless/static-debian11:nonroot
COPY go-import-redirector /
USER nonroot
ENTRYPOINT ["/go-import-redirector"]
