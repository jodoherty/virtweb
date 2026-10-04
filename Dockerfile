# virtweb container image. Build the binary first (make dist, or a
# plain `go build`); the image is alpine + openssh-client + sshpass +
# the binary. Per-server SSH credentials are stored in the data dir
# (mount a volume at /home/virtweb/.virtweb), never on the host.
FROM alpine:latest
RUN apk add --no-cache ca-certificates openssh-client sshpass && \
    adduser -D -H -h /home/virtweb -s /sbin/nologin virtweb
USER virtweb
WORKDIR /home/virtweb
COPY --chown=virtweb dist/virtweb-linux-amd64 /usr/local/bin/virtweb
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/virtweb"]
