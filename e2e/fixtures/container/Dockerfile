# e2e tier-2 "remote libvirt host" container (see TESTING.md).
#
# Debian bookworm (libvirt 9.x): the trixie 11.x security (DAC) driver is
# fine too, but bookworm's libvirt predates a couple of container-hostile
# behaviors and keeps the image small. The guest image is trixie (see
# images.json) — the guest OS is independent of the host emulation base.
#
# Built by `env:up --backend libvirt` (podman build), run --privileged
# (needed for the /proc/sys tmpfs overlay the NAT bridge requires; see
# entrypoint.sh). All rootless-host restrictions are documented there and
# in src/env/libvirt.js.
FROM docker.io/library/debian:bookworm

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        openssh-server \
        dbus \
        polkitd \
        libvirt-daemon-system \
        libvirt-clients \
        qemu-system-x86 \
        dnsmasq \
        bridge-utils \
        iptables \
        kmod \
        cloud-image-utils \
        curl \
        ca-certificates \
        iputils-ping \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /vw/entrypoint.sh
COPY vm.xml        /vw/vm.xml
COPY images.json   /vw/images.json
RUN chmod +x /vw/entrypoint.sh

# sshd needs /run/sshd at startup (entrypoint creates it, but keep the
# package postinst from failing on the build step that creates it).
CMD ["/vw/entrypoint.sh"]
