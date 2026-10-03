#!/bin/sh
# entrypoint.sh — bring up the "remote libvirt host" inside the e2e
# container (Debian bookworm, rootless podman). No systemd, so daemons
# are started directly. Runs as container-root (maps to the host user
# running rootless podman); libvirt runs in normal "system" mode
# (qemu:///system) and QEMU guests run as the libvirt-qemu user on TCG
# (see the /dev/kvm note below).
set -e

# ---- /dev/kvm / TCG ---------------------------------------------
# Under rootless podman the host /dev/kvm cannot be opened by ANY
# container process (host-side permission check uses the real host
# groups, which the userns cannot influence; trusted.* xattrs and
# device chown are likewise impossible — see vw-xattr-shim.c). Guests
# therefore run on TCG (explicit in /vw/vm.xml). TCG boots are slow
# (~3-5 min for the cloud image); the @real specs account for it.

# ---- /proc/sys (writable, for the NAT bridge) --------------------
# Rootless podman mounts /proc/sys read-only; `virsh net-start` writes
# net/ipv6/conf/virbr0/disable_ipv6 and fails otherwise. --privileged
# lets us overlay a tmpfs. The overlay must be populated with the
# original tree: `podman exec` reads kernel/cap_last_cap, and libvirtd
# reads sysctls at runtime (an empty tmpfs makes it hang). A few
# privileged special sysctls can't be read (copy kept non-fatal).
if ! touch /proc/sys/net/ipv4/conf/all/test-vw-write 2>/dev/null; then
	cp -a /proc/sys /tmp/vw-procsys 2>/dev/null || true
	mount -t tmpfs tmpfs /proc/sys
	cp -a /tmp/vw-procsys/. /proc/sys/ 2>/dev/null || true
	rm -rf /tmp/vw-procsys
fi

# ---- sshd (how virtweb / env:up reach the "remote host") ---------
mkdir -p /run/sshd
# host keys are regenerated on every container create (like a fresh
# host); virtweb's ssh uses accept-new, so a changing key is fine.
ssh-keygen -A >/dev/null
# Only key auth, no passwords. Root login so env:up can use the e2e
# key directly (mirrors the real host's key auth).
cat >> /etc/ssh/sshd_config.d/99-vw.conf <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
PubkeyAuthentication yes
EOF
/usr/sbin/sshd
echo "[entrypoint] sshd started"

# ---- guest image (pinned, checksummed) ---------------------------
# Cached in the named podman volume backing /var/lib/libvirt/images,
# so it survives container recreation; `env:down --wipe` removes it.
mkdir -p /var/lib/libvirt/images
image="$(python3 - <<'EOF' 2>/dev/null || awk -F'"' '/"name"/{n=$4} /"url"/{u=$4} /"sha512"/{s=$4} END{print n" "u" "s}' /vw/images.json
import json
d = json.load(open("/vw/images.json"))
print(d["name"], d["url"], d["sha512"])
EOF
)"
image_name="${image%% *}"
rest="${image#* }"
image_url="${rest%% *}"
image_sha="${rest#* }"
if [ ! -f "/var/lib/libvirt/images/$image_name" ]; then
	echo "[entrypoint] downloading $image_name ..."
	curl -fsSL -o "/var/lib/libvirt/images/$image_name" "$image_url"
	echo "$image_sha  /var/lib/libvirt/images/$image_name" | sha512sum -c -
fi
echo "[entrypoint] guest image ready"

# ---- config drive (NoCloud): root password + hostname ------------
# The password must be set by runcmd chpasswd: cloud-init's
# `password:` directive applies to the default user, which is not
# guaranteed to be root on Debian images. The disk is fresh on every
# env, so cloud-init runs its first boot fully (on a reused disk with
# the same instance id it would skip user-data).
mkdir -p /tmp/vw-nocloud
cat > /tmp/vw-nocloud/user-data <<'EOF'
#cloud-config
runcmd:
  - [sh, -c, "echo 'root:vw-e2e-pass' | chpasswd"]
  - [sh, -c, "hostname vw-guest"]
EOF
echo "instance-id: vw-e2e-$(date +%s)" > /tmp/vw-nocloud/meta-data
cloud-localds /var/lib/libvirt/images/config-drive.iso \
	/tmp/vw-nocloud/user-data /tmp/vw-nocloud/meta-data
echo "[entrypoint] config drive ready"

# ---- libvirtd + virtlogd (with the xattr/chown/chmod shim) -------
# libvirt's security driver sets trusted.* xattrs, POSIX ACLs, chowns
# and chmods on domain files and devices — impossible under rootless
# podman (init-userns CAP_SYS_ADMIN / host device nodes). The shim
# (mounted by env:up, built from vw-xattr-shim.c) no-ops exactly
# those calls; everything else passes through.
# ---- dbus + polkit ------------------------------------------------
# libvirt's interactive console (virsh console, what virtweb's serial
# console is) requires polkit authorization; without a polkitd answering
# the check it fails with "Authorization not available". Root is
# auto-approved by the default policy.
dbus-daemon --system --fork
/usr/lib/polkit-1/polkitd --no-debug &
echo "[entrypoint] dbus + polkit started"

mkdir -p /var/run/libvirt /var/log/libvirt
export LD_PRELOAD=/vw/vw-xattr-shim.so
/usr/sbin/libvirtd --daemon
/usr/sbin/virtlogd --daemon 2>/dev/null || /usr/sbin/virtlogd
i=0
while [ ! -S /var/run/libvirt/libvirt-sock ] && [ $i -lt 50 ]; do
	sleep 0.2; i=$((i+1))
done
echo "[entrypoint] libvirtd: $(virsh version --daemon 2>&1 | tail -1)"

# ---- default NAT network (for domifaddr) --------------------------
if ! virsh net-info default >/dev/null 2>&1; then
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

# ---- define the guest (not started: the specs exercise Start) -----
virsh define /vw/vm.xml >/dev/null 2>&1 || true
echo "[entrypoint] guest defined: $(virsh list --all | tail -1)"

# ---- keep the container alive (daemons above keep running) --------
echo "[entrypoint] ready"
exec /usr/bin/sleep infinity
