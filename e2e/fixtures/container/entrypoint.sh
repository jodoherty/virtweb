#!/bin/sh
# entrypoint.sh — bring up the "remote libvirt host" services inside the
# Debian-trixie container. There is no systemd here, so we start the
# daemons directly. This runs as container-root (which maps to the host
# user running rootless podman), so libvirt runs in the normal "system"
# mode (qemu:///system) and qemu guests run as the same mapped user.
set -e

# ---- sshd (this is how virtweb / env:up reach the "remote host") ----
mkdir -p /run/sshd
# host keys are regenerated on every container create (like a fresh host);
# virtweb's ssh uses accept-new, so a changing host key is fine.
ssh-keygen -A >/dev/null
# Only key auth, no passwords. Root login allowed so env:up can use the
# e2e key directly (mirrors the real host's james@... key auth).
cat >> /etc/ssh/sshd_config.d/99-vw.conf <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
PubkeyAuthentication yes
EOF
/usr/sbin/sshd
echo "[entrypoint] sshd started"

# ---- libvirtd (system mode) ----
mkdir -p /var/run/libvirt /var/log/libvirt
# libvirtd daemonizes and manages /var/run/libvirt/libvirt-sock.
if ! virsh system-check >/dev/null 2>&1 && [ ! -S /var/run/libvirt/libvirt-sock ]; then
  /usr/sbin/libvirtd --daemon
  # wait for the socket
  i=0
  while [ ! -S /var/run/libvirt/libvirt-sock ] && [ $i -lt 50 ]; do
    sleep 0.2; i=$((i+1))
  done
fi
echo "[entrypoint] libvirtd: $(virsh version --daemon 2>&1 | tail -1)"

# ---- default NAT network (for domifaddr) ----
# A fresh container has no defined network, so define the standard default
# NAT net if it is missing, then autostart + start it.
# NOTE: `virsh net-start` creates the virbr0 bridge and writes
# /proc/sys/net/ipv6/conf/virbr0/disable_ipv6, which requires a WRITABLE
# /proc/sys. Under default rootless podman that path is read-only, so the
# container must be created with a writable /proc/sys (e.g. a tmpfs over
# /proc/sys) for NAT to come up. net-start is kept non-fatal so the host
# services still come up even if the bridge fails.
if [ "$(virsh net-info default 2>/dev/null | grep -c State)" = "0" ]; then
  cat > /tmp/vw-default-net.xml <<'EOF'
<network>
  <name>default</name>
  <forward mode="nat"><nat/></forward>
  <bridge name="virbr0" stp="on" delay="0"/>
  <ip address="192.168.122.1" netmask="255.255.255.0">
    <dhcp><range start="192.168.122.2" end="192.168.122.254"/></dhcp>
  </ip>
</network>
EOF
  virsh net-define /tmp/vw-default-net.xml
fi
virsh net-autostart default >/dev/null 2>&1 || true
virsh net-start default >/dev/null 2>&1 || true
echo "[entrypoint] default net: $(virsh net-info default 2>/dev/null | grep State | awk '{print $2}')"

# ---- keep the container alive (daemons above keep running) ----
# exec sleep so PID1 stays up; daemons are children of this.
echo "[entrypoint] ready"
exec /usr/bin/sleep infinity
