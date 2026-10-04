# virtweb container image — multi-stage: the binary is built inside
# the image (golang base, no host toolchain needed), then shipped on a
# minimal debian:trixie runtime with openssh-client + sshpass.
#
# Per-server SSH credentials live in the data dir, never in the image:
# mount a volume at /home/virtweb/.virtweb.
#
# Build:
#   docker build -t virtweb .
#   docker build --build-arg VERSION=<git-tag-or-hash> -t virtweb .

FROM docker.io/library/golang:1.27 AS build
ARG VERSION=dev
WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/virtweb .

FROM docker.io/library/debian:trixie
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates openssh-client sshpass && \
    apt-get clean && rm -rf /var/lib/apt/lists/* && \
    useradd -m -d /home/virtweb -s /usr/sbin/nologin virtweb
USER virtweb
WORKDIR /home/virtweb
COPY --from=build --chown=virtweb /out/virtweb /usr/local/bin/virtweb
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/virtweb"]
