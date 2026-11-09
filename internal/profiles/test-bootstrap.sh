#!/bin/sh
# Tests bootstrap.sh against a scratch ROOT and stub system commands — nothing real is touched.
#   sh internal/profiles/test-bootstrap.sh        (make test-bootstrap)
# It runs a scratch copy of the tree the agent embeds (internal/profiles/bootstrap) with the by-hand profiles (by-hand/profiles)
# added beside the agent's own, so the features only those use (docker, nginx, the Pi's) are tested too.
set -eu
# the test itself needs these (the features fall back when a machine lacks xxd; the test checks the fingerprints with it)
for c in xxd ssh-keygen; do command -v "$c" >/dev/null 2>&1 || { echo "test-bootstrap needs $c (Debian/Ubuntu: apt-get install xxd openssh-client)" >&2; exit 1; }; done
src="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
here="$tmp/tree"
mkdir -p "$here"
cp -R "$src/bootstrap/." "$here/"
cp "$src/../../by-hand/profiles/"*.sh "$here/profiles/"
export ROOT="$tmp/root" STATE="$tmp/state" CALLS="$tmp/calls"
mkdir -p "$ROOT/boot/firmware" "$STATE" "$tmp/bin"
printf 'console=serial0,115200 root=PARTUUID=abc rootwait\n' >"$ROOT/boot/firmware/cmdline.txt"
printf 'dtparam=audio=on\n' >"$ROOT/boot/firmware/config.txt"
: >"$CALLS"

stub() { printf '#!/bin/sh\n%s\n' "$2" >"$tmp/bin/$1"; chmod +x "$tmp/bin/$1"; }
stub uname 'if [ "${1:-}" = -m ]; then echo "${ARCH:-aarch64}"; else /usr/bin/uname "$@"; fi'
stub id 'case "$*" in "-nG nuitcloud") cat "$STATE/groups" 2>/dev/null || echo nuitcloud;; "nuitcloud") [ -e "$STATE/nouser" ] && exit 1; exit 0;; *) /usr/bin/id "$@";; esac'
stub usermod 'echo "usermod $*" >>"$CALLS"; echo "nuitcloud docker" >"$STATE/groups"'
stub apt-get 'echo "apt-get $*" >>"$CALLS"; for a in "$@"; do case "$a" in zram-tools|unattended-upgrades|ca-certificates|curl|iproute2|iputils-arping) touch "$STATE/pkg-$a";; esac; done'
stub dpkg '[ -e "$STATE/pkg-$2" ]'
stub systemctl 'case "$1" in is-enabled|is-active) ;; *) echo "systemctl $*" >>"$CALLS";; esac
case "$1" in
  enable) shift; [ "$1" = --now ] && shift; touch "$STATE/enabled-$1"; touch "$STATE/active-$1";;
  disable) shift; [ "$1" = --now ] && shift; rm -f "$STATE/enabled-$1";;
  is-enabled) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/enabled-$1" ];;
  is-active) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/active-$1" ];;
esac'
stub docker '[ "$1" = inspect ] || echo "docker $*" >>"$CALLS"; case "$1" in inspect) [ -e "$STATE/nginx" ]; exit $?;; run) touch "$STATE/nginx";; esac; exit 0'
stub curl 'printf "echo installing-docker >>\"\$CALLS\"; touch \"\$STATE/docker-installed\"; touch \"$tmp/bin/docker-present\"\n"'
stub timedatectl 'case "$1" in show) [ -e "$STATE/clock-unsynced" ] && echo no || echo yes;; esac; exit 0'
stub getent 'echo "getent $*" >>"$STATE/getent-calls"; [ ! -e "$STATE/dns-down" ] || exit 2; case "$1" in hosts) echo "192.0.2.1 $2";; esac; exit 0'
export PATH="$tmp/bin:$PATH"
run() { sh "$here/bootstrap.sh" "$@"; }
ok() { echo "ok - $1"; }
bad() { echo "FAIL - $1" >&2; exit 1; }

# 1. --check on a fresh box reports drift, exits 1 and changes nothing
if run raspberry-pi-3 --check >"$tmp/out" 2>&1; then bad "check passed on a fresh box"; fi
grep -q 'DRIFT   pi_cgroup' "$tmp/out" && ! grep -q cgroup "$ROOT/boot/firmware/cmdline.txt" && [ ! -s "$CALLS" ] && ok "--check reports drift and changes nothing" || bad "--check"

# 2. apply (docker present already via stub PATH): Pi features are made, reboot flagged
touch "$STATE/enabled-docker" "$STATE/active-systemd-timesyncd"
run raspberry-pi-3 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "apply failed"; }
grep -q '^console=serial0,115200 root=PARTUUID=abc rootwait cgroup_enable=memory cgroup_memory=1$' "$ROOT/boot/firmware/cmdline.txt" \
  && [ "$(wc -l <"$ROOT/boot/firmware/cmdline.txt")" -eq 1 ] && ok "cmdline.txt stays one line with the cgroup flags" || bad "cmdline.txt"
grep -q '^gpu_mem=16$' "$ROOT/boot/firmware/config.txt" && grep -q 'audio=on' "$ROOT/boot/firmware/config.txt" && ok "config.txt gets gpu_mem and keeps its lines" || bad "config.txt"
grep -q '"max-size": "5m"' "$ROOT/etc/docker/daemon.json" && grep -q 'max-file": "2"' "$ROOT/etc/docker/daemon.json" && ok "Pi log cap is 5m x 2" || bad "daemon.json"
[ -e "$ROOT/etc/systemd/system.conf.d/nuit-watchdog.conf" ] && [ -e "$ROOT/etc/systemd/journald.conf.d/nuit-cloud.conf" ] && [ -e "$ROOT/etc/default/zramswap" ] && ok "watchdog, journal cap, zram written" || bad "pi files"
grep -q 'reboot is needed' "$tmp/out" && ok "reboot is flagged" || bad "reboot flag"

# 3. a second run and --check are clean and touch nothing
: >"$CALLS"
run raspberry-pi-3 >"$tmp/out" 2>&1 && run raspberry-pi-3 --check >/dev/null 2>&1 && ! grep -q applying "$tmp/out" && [ ! -s "$CALLS" ] || { cat "$tmp/out"; bad "second run not idempotent"; }
ok "second run is a no-op and --check is clean"

# 4. a daemon.json someone else wrote is left alone and reported as manual
rm -rf "$ROOT/etc/docker"; mkdir -p "$ROOT/etc/docker"; echo '{"storage-driver":"overlay2"}' >"$ROOT/etc/docker/daemon.json"
if run raspberry-pi-3 >"$tmp/out" 2>&1; then bad "existing daemon.json should need a person"; fi
grep -q 'MANUAL  docker_logs' "$tmp/out" && grep -q overlay2 "$ROOT/etc/docker/daemon.json" && ! grep -q max-size "$ROOT/etc/docker/daemon.json" && ok "foreign daemon.json is left alone, reported MANUAL" || bad "daemon.json overwrite"

# 5. a 32-bit OS is reported, not silently accepted; a missing user is reported
ARCH=armv7l run raspberry-pi-3 --check 2>&1 | grep -q 'DRIFT   pi_arch' && ok "32-bit OS is flagged" || bad "armv7l"
touch "$STATE/nouser"; if run docker-host >"$tmp/out" 2>&1; then bad "missing user passed"; fi
grep -q 'MANUAL  user' "$tmp/out" && ok "missing nuitcloud user is reported MANUAL" || bad "user"
rm "$STATE/nouser"

# 6. nginx is opt-in, and the old script name still works
run docker-host --check >/dev/null 2>&1 || true
! run docker-host --check 2>&1 | grep -q nginx && run docker-host --with nginx --check 2>&1 | grep -q 'DRIFT   nginx' && ok "--with nginx adds the feature" || bad "--with"
run no-such-profile >/dev/null 2>&1 && bad "unknown profile accepted" || ok "unknown profile refused"

# 7. the Nuit Cloud Server profiles: nothing risky is changed, everything that needs a person is said, and a good machine is clean
export CALLS2="$tmp/calls2"
NCS_CLEAN() { rm -rf "$ROOT/sys" "$ROOT/dev"; mkdir -p "$ROOT/sys/class/net/enp3s0" "$ROOT/sys/fs/cgroup" "$ROOT/dev" "$ROOT/usr/local/bin"; echo up >"$ROOT/sys/class/net/enp3s0/operstate"; : >"$ROOT/sys/fs/cgroup/cgroup.controllers"; }
stub sshd 'printf "permitrootlogin %s\npasswordauthentication %s\n" "${SSHD_ROOT:-yes}" "${SSHD_PASS:-yes}"'
stub modprobe 'echo "modprobe $*" >>"$CALLS"; exit 1'
NCS_CLEAN
rm -f "$ROOT/etc/systemd/journald.conf.d/nuit-cloud.conf"
rm -f "$STATE/nouser" "$STATE/pkg-ca-certificates" "$STATE/pkg-curl" "$STATE/pkg-iproute2"
touch "$STATE/pkg-unattended-upgrades"; mkdir -p "$ROOT/etc/apt/apt.conf.d"; : >"$ROOT/etc/apt/apt.conf.d/20auto-upgrades"
touch "$STATE/active-systemd-timesyncd"
: >"$CALLS"
if run nuit-cloud-server --check >"$tmp/out" 2>&1; then bad "a machine with no journal cap, packages or SSH hardening passed"; fi
grep -q 'DRIFT   journal' "$tmp/out" && grep -q 'DRIFT   ncs_packages' "$tmp/out" && grep -q 'DRIFT   ncs_ssh' "$tmp/out" && grep -q 'ok      ncs_wired' "$tmp/out" && ok "nuit-cloud-server --check names what is missing and sees the cable" || { cat "$tmp/out"; bad "ncs check"; }

# applying makes the safe things and reports SSH as a person's job, without touching sshd
: >"$CALLS"
run nuit-cloud-server >"$tmp/out" 2>&1 && bad "SSH hardening should need a person"
grep -q 'MANUAL  ncs_ssh' "$tmp/out" && grep -q 'done    journal' "$tmp/out" && grep -q 'done    ncs_packages' "$tmp/out" && [ -e "$ROOT/etc/systemd/journald.conf.d/nuit-cloud.conf" ] && ! grep -q "sshd" "$CALLS" && ok "safe features applied; SSH left to a person" || { cat "$tmp/out"; bad "ncs apply"; }

# once SSH forbids passwords and root login, the base profile is clean and a second run changes nothing
SSHD_ROOT=prohibit-password SSHD_PASS=no run nuit-cloud-server >"$tmp/out" 2>&1 && SSHD_ROOT=no SSHD_PASS=no run nuit-cloud-server --check >/dev/null 2>&1 && ! grep -q applying "$tmp/out" && ok "a hardened machine is clean, and a second run is a no-op" || { cat "$tmp/out"; bad "ncs clean"; }

# no cable: wireless and virtual interfaces do not count as wired
rm -rf "$ROOT/sys/class/net/enp3s0"; mkdir -p "$ROOT/sys/class/net/wlan0/wireless"; echo up >"$ROOT/sys/class/net/wlan0/operstate"
SSHD_ROOT=no SSHD_PASS=no run nuit-cloud-server >"$tmp/out" 2>&1 && bad "wireless only should need a person"
grep -q 'MANUAL  ncs_wired' "$tmp/out" && ok "wireless-only is reported MANUAL: connect a cable" || { cat "$tmp/out"; bad "wireless"; }
NCS_CLEAN

# the container-service profile adds /dev/kvm, cgroup v2 and the runtime; modprobe is tried, and a missing /dev/kvm needs a person
SSHD_ROOT=no SSHD_PASS=no run nuit-container-service --check >"$tmp/out" 2>&1 && bad "no kvm and no runtime passed"
grep -q 'DRIFT   kvm' "$tmp/out" && grep -q 'DRIFT   oci_saas_runtime' "$tmp/out" && grep -q 'ok      cgroup2' "$tmp/out" && ok "nuit-container-service adds kvm, cgroup2 and the runtime" || { cat "$tmp/out"; bad "ncs profile"; }
: >"$CALLS"; SSHD_ROOT=no SSHD_PASS=no run nuit-container-service >"$tmp/out" 2>&1 && bad "kvm should need a person"
grep -q 'MANUAL  kvm' "$tmp/out" && grep -q 'modprobe kvm_intel' "$CALLS" && grep -q 'MANUAL  oci_saas_runtime' "$tmp/out" && ok "kvm: the module is tried, then a person is asked; the runtime installer is said not to exist yet" || { cat "$tmp/out"; bad "kvm"; }
: >"$ROOT/dev/kvm"; : >"$ROOT/usr/local/bin/crictl-ocisaas"; chmod +x "$ROOT/usr/local/bin/crictl-ocisaas"; touch "$STATE/active-containerd-ocisaas"
# the runtime is installed only from what the platform hands the agent, and only after the bundle's root key is the pinned one
OCI="$tmp/oci"; mkdir -p "$OCI/bundle/packaging/dedicated" "$OCI/bundle/bin" "$OCI/bundle/guest" "$OCI/bundle/policy" "$OCI/cd/bin"
printf 'root 00112233445566778899aabbccddeeff\n' >"$OCI/bundle/artifacts.pub"; cp "$OCI/bundle/artifacts.pub" "$OCI/bundle/policy.pub"
printf '#!/bin/sh\necho smoke\n' >"$OCI/bundle/packaging/dedicated/smoke.sh"; chmod +x "$OCI/bundle/packaging/dedicated/smoke.sh"
printf '#!/bin/sh\necho "install.sh $*" >>"$CALLS"\necho "containerd=$(command -v containerd)" >>"$CALLS"\ntouch "$ROOT/run/oci-saas/containerd.sock" "$STATE/active-containerd-ocisaas"; : >"$ROOT/usr/local/bin/crictl-ocisaas"; chmod +x "$ROOT/usr/local/bin/crictl-ocisaas"; mkdir -p "$ROOT/etc/oci-saas"; cp "%s/bundle/artifacts.pub" "$ROOT/etc/oci-saas/artifacts.pub"; cp "%s/bundle/policy.pub" "$ROOT/etc/oci-saas/policy.pub"\n' "$OCI" "$OCI" >"$OCI/bundle/packaging/dedicated/install.sh"
chmod +x "$OCI/bundle/packaging/dedicated/install.sh"
printf '#!/bin/sh\necho "containerd github.com/containerd/containerd/v2 v2.2.10"\n' >"$OCI/cd/bin/containerd"; chmod +x "$OCI/cd/bin/containerd"
tar -C "$OCI" -czf "$OCI/bundle.tar.gz" bundle; tar -C "$OCI/cd" -czf "$OCI/containerd.tar.gz" bin
printf '#!/bin/sh\necho crictl\n' >"$OCI/crictl"; chmod +x "$OCI/crictl"; tar -C "$OCI" -czf "$OCI/crictl.tar.gz" crictl; mkdir -p "$ROOT/usr/local/bin"
mkdir -p "$ROOT/run/oci-saas" "$ROOT/usr/bin" "$ROOT/usr/local/lib/nuit-cloud"; rm -f "$ROOT/usr/local/bin/crictl-ocisaas"; rm -f "$STATE/active-containerd-ocisaas"
GOODFP="$(printf '00112233445566778899aabbccddeeff' | xxd -r -p | sha256sum | awk '{print $1}')"
stub systemd-version 'true'; stub systemctl 'case "$1" in --version) echo "systemd 257 (257.9-1)";; is-enabled|is-active) ;; *) echo "systemctl $*" >>"$CALLS";; esac
case "$1" in
  enable) shift; [ "$1" = --now ] && shift; touch "$STATE/enabled-$1"; touch "$STATE/active-$1";;
  is-enabled) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/enabled-$1" ];;
  is-active) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/active-$1" ];;
esac'
RT="NCS_RUNTIME_BUNDLE=$OCI/bundle.tar.gz NCS_CONTAINERD_TARBALL=$OCI/containerd.tar.gz NCS_CRICTL_TARBALL=$OCI/crictl.tar.gz NCS_RUNTIME_BUNDLE_SHA256=abc123"
: >"$CALLS"; env SSHD_ROOT=no SSHD_PASS=no $RT NCS_RUNTIME_ROOT_FINGERPRINT=deadbeef sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 && bad "a wrong root fingerprint was accepted"
grep -q 'FAILED  oci_saas_runtime' "$tmp/out" && grep -q 'not the root the platform pinned' "$tmp/out" && ! grep -q 'install.sh' "$CALLS" && [ ! -e "$ROOT/usr/bin/containerd" ] && ok "runtime: a bundle whose keys are not the pinned root installs nothing" || { cat "$tmp/out"; bad "runtime fingerprint"; }
# names do not resolve: the runtime stops before touching apt, as a person's job
printf 'nameserver 10.10.10.151\nnameserver 10.10.10.153\n' >"$ROOT/etc/resolv.conf"; touch "$STATE/dns-down"; : >"$CALLS"
env SSHD_ROOT=no SSHD_PASS=no $RT NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 && bad "the runtime went ahead without name resolution"
grep -q 'MANUAL  oci_saas_runtime' "$tmp/out" && grep -q 'cannot resolve names (its resolvers: 10.10.10.151, 10.10.10.153): set them first' "$tmp/out" && ! grep -q 'apt-get' "$CALLS" \
  && ok "runtime: a machine that cannot resolve names stops with MANUAL and the resolvers, before apt" || { cat "$tmp/out" "$CALLS"; bad "runtime without dns"; }
rm -f "$STATE/dns-down"
printf '#!/bin/sh\necho "containerd github.com/containerd/containerd 1.7.24"\n' >"$ROOT/usr/bin/containerd"; chmod +x "$ROOT/usr/bin/containerd"
env SSHD_ROOT=no SSHD_PASS=no $RT NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "an older containerd stopped the install"; }
grep -q 'done    oci_saas_runtime' "$tmp/out" && grep -q '1.7.24' "$ROOT/usr/bin/containerd" && [ -x "$ROOT/usr/local/lib/oci-saas/bin/containerd" ] \
  && grep -q 'ExecStart=$' "$ROOT/etc/systemd/system/containerd-ocisaas.service.d/10-dedicated-containerd.conf" && grep -q 'ExecStart=/usr/local/lib/oci-saas/bin/containerd --config /etc/oci-saas/containerd.toml' "$ROOT/etc/systemd/system/containerd-ocisaas.service.d/10-dedicated-containerd.conf" \
  && grep -q "containerd=$ROOT/usr/local/lib/oci-saas/bin/containerd" "$CALLS" \
  && ok "runtime: another service's containerd 1.x is left alone and the runtime runs the pinned 2.x from its own directory, with nobody asked" || { cat "$tmp/out"; cat "$CALLS"; bad "runtime beside an older containerd"; }
# the same machine again: nothing is installed twice
rm -rf "$ROOT/usr/local/lib/oci-saas" "$ROOT/etc/systemd/system/containerd-ocisaas.service.d" "$ROOT/etc/oci-saas/installed-bundle" "$ROOT/usr/local/lib/nuit-cloud/oci-saas-smoke.sh" "$ROOT/usr/local/bin/crictl"
rm -f "$ROOT/usr/bin/containerd"; : >"$CALLS"
env SSHD_ROOT=no SSHD_PASS=no $RT NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 || { tail -40 "$tmp/out"; bad "the runtime did not install"; }
grep -q 'done    oci_saas_runtime' "$tmp/out" && [ -x "$ROOT/usr/bin/containerd" ] && grep -q -- '--policy-dir .*/policy' "$CALLS" && grep -q -- '--cni-bin /usr/lib/cni --add-user nuitcloud' "$CALLS" && [ "$(cat "$ROOT/etc/oci-saas/installed-bundle")" = abc123 ] \
  && [ -x "$ROOT/usr/local/bin/crictl" ] && [ -x "$ROOT/usr/local/lib/nuit-cloud/oci-saas-smoke.sh" ] && grep -q 'apt-get install -y -qq erofs-utils' "$CALLS" && ok "runtime: containerd 2.x and the bundle are installed with the bundle's initial policy, and the bundle is recorded" || { cat "$tmp/out"; cat "$CALLS"; bad "runtime install"; }
: >"$ROOT/usr/local/bin/crictl-ocisaas"; chmod +x "$ROOT/usr/local/bin/crictl-ocisaas"; touch "$STATE/active-containerd-ocisaas"
env SSHD_ROOT=no SSHD_PASS=no $RT sh "$here/bootstrap.sh" nuit-container-service --check >"$tmp/out" 2>&1 && ok "a machine with kvm, cgroup v2 and the runtime is clean" || { cat "$tmp/out"; bad "ncs full"; }
env SSHD_ROOT=no SSHD_PASS=no NCS_RUNTIME_BUNDLE_SHA256=newer sh "$here/bootstrap.sh" nuit-container-service --check >"$tmp/out" 2>&1 && bad "a different pinned bundle was not drift"
grep -q 'DRIFT   oci_saas_runtime' "$tmp/out" && ok "runtime: a newly pinned bundle shows as drift" || { cat "$tmp/out"; bad "runtime drift"; }
: >"$CALLS"; env SSHD_ROOT=no SSHD_PASS=no $RT NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 || bad "second run failed"
! grep -q 'install.sh' "$CALLS" && ok "runtime: a second apply with the same bundle changes nothing" || { cat "$CALLS"; bad "runtime idempotent"; }
# a newly pinned bundle whose install.sh has no --keep-policy (older bundles): the node's current policy is carried over and the service restarted
mkdir -p "$ROOT/var/lib/oci-saas/policy"; echo '{"version":1}' >"$ROOT/var/lib/oci-saas/policy/policy.json"; echo sig >"$ROOT/var/lib/oci-saas/policy/policy.json.sig"
: >"$CALLS"; env SSHD_ROOT=no SSHD_PASS=no NCS_RUNTIME_BUNDLE=$OCI/bundle.tar.gz NCS_CONTAINERD_TARBALL=$OCI/containerd.tar.gz NCS_CRICTL_TARBALL=$OCI/crictl.tar.gz NCS_RUNTIME_BUNDLE_SHA256=newer NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP sh "$here/bootstrap.sh" nuit-container-service >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "upgrade failed"; }
grep -q 'done    oci_saas_runtime' "$tmp/out" && grep -q -- '--policy-dir .*keep-policy' "$CALLS" && ! grep -q -- '--keep-policy' "$CALLS" && grep -q 'systemctl restart containerd-ocisaas' "$CALLS" && [ "$(cat "$ROOT/etc/oci-saas/installed-bundle")" = newer ] \
  && ok "runtime: an upgrade with an older install.sh carries the node's policy over and restarts the service" || { cat "$tmp/out"; cat "$CALLS"; bad "runtime upgrade"; }
rm -rf "$ROOT/var/lib/oci-saas/policy"

# A runtime installed by hand: running, with no record. The check says so, in words, and the record profile writes the record only after
# comparing what is installed with the bundle, restarting nothing.
mkdir -p "$OCI/bundle/bin" "$OCI/bundle/guest"; echo shim >"$OCI/bundle/bin/shim"; echo kernel >"$OCI/bundle/guest/kernel"; tar -C "$OCI" -czf "$OCI/bundle.tar.gz" bundle
mkdir -p "$ROOT/etc/oci-saas/artifacts"; cp "$OCI/bundle/bin/shim" "$ROOT/usr/local/bin/shim"; cp "$OCI/bundle/guest/kernel" "$ROOT/etc/oci-saas/artifacts/kernel"
rm -f "$ROOT/etc/oci-saas/installed-bundle" "$ROOT/usr/local/lib/nuit-cloud/oci-saas-smoke.sh"; : >"$CALLS"
env SSHD_ROOT=no SSHD_PASS=no NCS_RUNTIME_BUNDLE_SHA256=abc123 sh "$here/bootstrap.sh" nuit-container-service --check >"$tmp/out" 2>&1 && bad "an unrecorded runtime was clean"
grep -q 'DRIFT   oci_saas_runtime' "$tmp/out" && grep -q 'installed and running, but not recorded: missing the smoke-test script' "$tmp/out" && grep -q 'installed-bundle' "$tmp/out" && ok "runtime: one that runs but has no record says so, and what is missing" || { cat "$tmp/out"; bad "runtime unrecorded note"; }
env SSHD_ROOT=no SSHD_PASS=no sh "$here/bootstrap.sh" nuit-container-service-record --check >"$tmp/out" 2>&1 && bad "record check was clean without a record"
RC="NCS_RUNTIME_BUNDLE=$OCI/bundle.tar.gz NCS_RUNTIME_BUNDLE_SHA256=abc123 NCS_RUNTIME_ROOT_FINGERPRINT=$GOODFP"
echo changed >"$ROOT/etc/oci-saas/artifacts/kernel"
env $RC sh "$here/bootstrap.sh" nuit-container-service-record >"$tmp/out" 2>&1 && bad "a different install was recorded"
grep -q 'MANUAL  oci_saas_record' "$tmp/out" && grep -q 'artifacts/kernel differs' "$tmp/out" && [ ! -e "$ROOT/etc/oci-saas/installed-bundle" ] && ok "record: an install that is not the pinned bundle is not recorded, and the file that differs is named" || { cat "$tmp/out"; bad "record mismatch"; }
cp "$OCI/bundle/guest/kernel" "$ROOT/etc/oci-saas/artifacts/kernel"
env $RC NCS_RUNTIME_ROOT_FINGERPRINT=deadbeef sh "$here/bootstrap.sh" nuit-container-service-record >"$tmp/out" 2>&1 && bad "a wrong root was recorded"
grep -q 'not the root the platform pinned' "$tmp/out" && [ ! -e "$ROOT/etc/oci-saas/installed-bundle" ] && ok "record: a bundle whose keys are not the pinned root is not trusted" || { cat "$tmp/out"; bad "record root"; }
: >"$CALLS"; env $RC sh "$here/bootstrap.sh" nuit-container-service-record >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "record failed"; }
grep -q 'done    oci_saas_record' "$tmp/out" && [ "$(cat "$ROOT/etc/oci-saas/installed-bundle")" = abc123 ] && [ -x "$ROOT/usr/local/lib/nuit-cloud/oci-saas-smoke.sh" ] && ! grep -q 'install.sh\|restart' "$CALLS" \
  && ok "record: an install that is the pinned bundle is recorded, with nothing installed and nothing restarted" || { cat "$tmp/out"; cat "$CALLS"; bad "record"; }
env SSHD_ROOT=no SSHD_PASS=no NCS_RUNTIME_BUNDLE_SHA256=abc123 sh "$here/bootstrap.sh" nuit-container-service --check >"$tmp/out" 2>&1 && ok "runtime: once recorded, the runtime check is clean" || { cat "$tmp/out"; bad "runtime after record"; }
rm -f "$STATE/active-containerd-ocisaas"
env $RC sh "$here/bootstrap.sh" nuit-container-service-record >"$tmp/out" 2>&1 && bad "recorded a runtime that is not running"
grep -q 'nothing to record' "$tmp/out" && ok "record: a runtime that is not running is not recorded" || { cat "$tmp/out"; bad "record not running"; }
env SSHD_ROOT=no SSHD_PASS=no NCS_RUNTIME_BUNDLE_SHA256=abc123 sh "$here/bootstrap.sh" nuit-container-service --check >"$tmp/out" 2>&1 && bad "a stopped runtime was clean"
grep -q 'not installed, or not running here; missing: containerd-ocisaas(service not active)' "$tmp/out" && ok "runtime: a stopped runtime says which part is missing" || { cat "$tmp/out"; bad "runtime stopped"; }
touch "$STATE/active-containerd-ocisaas"

# the root fingerprint needs no xxd (a machine installed by hand may lack it): perl does it, and when nothing can, it says so instead of blaming the bundle
mkdir -p "$tmp/noxxd"; for t in sh cat awk sha256sum perl tar find head mkdir rm mktemp basename dirname cmp install printf; do ln -sf "$(command -v $t)" "$tmp/noxxd/$t" 2>/dev/null || true; done
env PATH="$tmp/noxxd:$tmp/bin" $RC sh "$here/bootstrap.sh" nuit-container-service-record --check >/dev/null 2>&1 || true
got="$(PATH="$tmp/noxxd" sh -c '. "$1/features.sh" 2>/dev/null; root_fingerprint_of "$2"' x "$here" "$OCI/bundle/artifacts.pub")"
[ "$got" = "$GOODFP" ] && ok "root fingerprint: worked out with perl when xxd is absent" || { echo "got [$got] want [$GOODFP]"; bad "fingerprint without xxd"; }
mkdir -p "$tmp/nothing"; for t in sh cat awk sha256sum; do ln -sf "$(command -v $t)" "$tmp/nothing/$t"; done
got="$(PATH="$tmp/nothing" sh -c '. "$1/features.sh" 2>/dev/null; root_fingerprint_of "$2"' x "$here" "$OCI/bundle/artifacts.pub")" || true
[ -z "$got" ] && ok "root fingerprint: with neither perl nor xxd it gives nothing, never the hash of nothing" || { echo "got [$got]"; bad "fingerprint with no tool"; }

# 8. ncs_volumes: put the volumes directory on a roomier filesystem with a bind mount, only when it is safe to
rm -rf "$ROOT/var/lib/oci-saas/logs"   # the log root (ncs_log_root) was made by the earlier full runs; this case starts from an empty runtime directory
GIB=$((1024 * 1024))
printf '/dev/sda1 /boot/efi vfat rw 0 0\n/dev/mapper/vg-root / ext4 rw 0 0\n/dev/mapper/vg-var /var ext4 rw 0 0\n/dev/mapper/vg-srv /srv ext4 rw 0 0\n' >"$ROOT/proc-mounts"
mkdir -p "$ROOT/proc" "$ROOT/srv" "$ROOT/var/lib" "$ROOT/etc"; cp "$ROOT/proc-mounts" "$ROOT/proc/mounts"
: >"$ROOT/etc/fstab"
# a fake df: free space by the path's top-level directory, in KiB
stub df 'for a in "$@"; do last="$a"; done
case "$last" in
  */srv|*/srv/*) free=$((197 * 1024 * 1024));; */var|*/var/*) free=$((6 * 1024 * 1024));; */boot/efi) free=1000000;; *) free=$((15 * 1024 * 1024));;
esac
printf "Filesystem 1024-blocks Used Available Capacity Mounted on\nx 1 1 %s 1%% /\n" "$free"'
stub mountpoint '[ -e "$STATE/mounted" ]'
stub mount 'echo "mount $*" >>"$CALLS"; touch "$STATE/mounted"'
rm -f "$STATE/mounted"; : >"$CALLS"
run nuit-container-service --check >"$tmp/out" 2>&1 || true
grep -q 'DRIFT   ncs_volumes' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_volumes: a small /var is reported as drift, and --check changes nothing" || { cat "$tmp/out"; bad "ncs_volumes check"; }
run nuit-container-service >"$tmp/out" 2>&1 || true
grep -q 'done    ncs_volumes' "$tmp/out" && grep -q '/srv/oci-saas /var/lib/oci-saas none bind,nofail,x-systemd.requires-mounts-for=/srv 0 0' "$ROOT/etc/fstab" && grep -q 'mount /var/lib/oci-saas' "$CALLS" && [ -d "$ROOT/srv/oci-saas" ] && ok "ncs_volumes: /srv is chosen, a nofail bind mount is written and mounted" || { cat "$tmp/out"; cat "$ROOT/etc/fstab"; bad "ncs_volumes apply"; }
lines_before="$(wc -l <"$ROOT/etc/fstab")"; run nuit-container-service >"$tmp/out" 2>&1 || true
[ "$(wc -l <"$ROOT/etc/fstab")" = "$lines_before" ] && grep -q 'ok      ncs_volumes' "$tmp/out" && ok "ncs_volumes: a second run changes nothing (no second fstab line)" || { cat "$tmp/out"; bad "ncs_volumes idempotent"; }
# a directory with content is never touched
rm -f "$STATE/mounted"; : >"$ROOT/etc/fstab"; mkdir -p "$ROOT/var/lib/oci-saas"; touch "$ROOT/var/lib/oci-saas/important"; : >"$CALLS"
run nuit-container-service >"$tmp/out" 2>&1 || true
grep -q 'MANUAL  ncs_volumes' "$tmp/out" && [ ! -s "$ROOT/etc/fstab" ] && ! grep -q mount "$CALLS" && [ -e "$ROOT/var/lib/oci-saas/important" ] && ok "ncs_volumes: a directory that already has content is left alone and reported MANUAL" || { cat "$tmp/out"; bad "ncs_volumes content"; }
rm -f "$ROOT/var/lib/oci-saas/important"
# nothing roomy enough: reported, not guessed
printf '/dev/mapper/vg-root / ext4 rw 0 0\n/dev/mapper/vg-var /var ext4 rw 0 0\n' >"$ROOT/proc/mounts"; : >"$CALLS"
run nuit-container-service >"$tmp/out" 2>&1 || true
grep -q 'MANUAL  ncs_volumes' "$tmp/out" && [ ! -s "$ROOT/etc/fstab" ] && ok "ncs_volumes: with no roomy filesystem it says so instead of guessing" || { cat "$tmp/out"; bad "ncs_volumes none"; }

# 9. ncs_address: the platform's allocated management address becomes the machine's static one, and is undone if it cannot be proved
OLDROOT="$ROOT"; export ROOT="$tmp/root-addr"; rm -rf "$ROOT"
mk_net() { # mk_net <current address> : a Debian with ifupdown whose eth0 got <current> from DHCP
  rm -rf "$ROOT"; mkdir -p "$ROOT/etc/network/interfaces.d" "$ROOT/var/lib"
  printf 'source /etc/network/interfaces.d/*\n' >"$ROOT/etc/network/interfaces"
  printf 'auto lo\niface lo inet loopback\nauto eth0\niface eth0 inet dhcp\n    hwaddress ether 02:00:00:00:00:01\n\n# a second interface that must stay\niface eth1 inet static\n    address 192.168.9.2/24\n' >"$ROOT/etc/network/interfaces.d/eth0"
  printf 'eth0 %s/24\neth0.5 10.20.0.2/24\neth0.5 10.20.0.3/24\n' "$1" >"$STATE/ip-addrs"; printf 'default via 10.10.10.1 dev eth0\n' >"$STATE/ip-route"
  printf '10.10.10.0/24 dev eth0 proto kernel scope link src %s\n10.20.0.0/24 dev eth0.5 proto kernel scope link src 10.20.0.2\n' "$1" >"$STATE/ip-routes"; printf '0:\tfrom all lookup local\n100:\tfrom all fwmark 0x5 lookup 105\n' >"$STATE/ip-rules"
  rm -f "$STATE/active-NetworkManager" "$STATE/arping-dupe" "$STATE/curl-fail" "$STATE/lose-vlan" "$STATE/stubborn"; : >"$CALLS"
}
stub ip 'case "$*" in
  "-4 route show default") cat "$STATE/ip-route";;
  "-4 -o addr show") awk '"'"'{printf "2: %s    inet %s brd 10.10.10.255 scope global %s\n", $1, $2, $1}'"'"' "$STATE/ip-addrs";;
  "-4 route show") cat "$STATE/ip-routes";;
  "-4 rule show") cat "$STATE/ip-rules";;
  "-4 -o addr show dev "*) awk -v d="$6" '"'"'$1 == d {printf "2: %s    inet %s brd 10.10.10.255 scope global %s\n", d, $2, d}'"'"' "$STATE/ip-addrs";;
  "addr add "*) echo "ip $*" >>"$CALLS"; if [ -e "$STATE/stubborn" ] && [ "$5" = eth0.5 ]; then :; else grep -qx "$5 $3" "$STATE/ip-addrs" || echo "$5 $3" >>"$STATE/ip-addrs"; fi;;
  "addr del "*) echo "ip $*" >>"$CALLS"; grep -vx "$5 $3" "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"
    if [ -e "$STATE/lose-vlan" ]; then grep -v "^eth0.5 " "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"; fi;;
  "route replace "*) echo "ip $*" >>"$CALLS";;
  *) echo "ip $*" >>"$CALLS";;
esac'
stub ifup 'echo "ifup $*" >>"$CALLS"'
stub systemd-run 'echo "systemd-run $*" >>"$CALLS"'
stub arping 'echo "arping $*" >>"$CALLS"; [ ! -e "$STATE/arping-dupe" ]'
stub sysctl 'echo "sysctl $*" >>"$CALLS"'
stub sleep 'true'
stub curl '[ ! -e "$STATE/curl-fail" ] && echo "curl $*" >>"$CALLS"'
addr_run() { env NCS_MANAGEMENT_ADDRESS="$1" NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-address ${2:+"$2"}; }

# already on that address through DHCP: it only becomes static, nothing is added or removed, and the safety timer is cancelled
mk_net 10.10.10.162
addr_run 10.10.10.162 --check >"$tmp/out" 2>&1 && bad "a DHCP address passed the check"
grep -q 'DRIFT   ncs_address' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address: a DHCP address that matches is still drift, and --check changes nothing" || { cat "$tmp/out"; bad "address check"; }
addr_run 10.10.10.162 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "address apply"; }
grep -q 'done    ncs_address' "$tmp/out" && grep -q '^# nuit-cloud: iface eth0 inet dhcp$' "$ROOT/etc/network/interfaces.d/eth0" && grep -q '^# nuit-cloud:     hwaddress ether' "$ROOT/etc/network/interfaces.d/eth0" \
  && grep -q '^auto eth0$' "$ROOT/etc/network/interfaces.d/eth0" && grep -q '^iface eth1 inet static$' "$ROOT/etc/network/interfaces.d/eth0" && grep -q '^    address 192.168.9.2/24$' "$ROOT/etc/network/interfaces.d/eth0" \
  && grep -q 'address 10.10.10.162/24' "$ROOT/etc/network/interfaces.d/nuit-cloud-address" && grep -q 'gateway 10.10.10.1' "$ROOT/etc/network/interfaces.d/nuit-cloud-address" \
  && grep -q 'systemd-run.*nuit-cloud-address-rollback' "$CALLS" && grep -q 'systemctl stop nuit-cloud-address-rollback.timer' "$CALLS" && ! grep -q 'ip addr del' "$CALLS" && ! grep -q 'ip addr add' "$CALLS" \
  && grep -q 'curl .*--interface 10.10.10.162 .*agent.example/api/agent/download/VERSION' "$CALLS" && [ -f "$ROOT/var/lib/nuit-cloud-agent/address/backup/files" ] && [ ! -e "$ROOT/var/lib/nuit-cloud-agent/address/armed" ] \
  && ok "ncs_address: only the machine's own stanza is commented out, the static one is written, the proof ran from the new address and the timer was cancelled" || { cat "$tmp/out"; cat "$ROOT/etc/network/interfaces.d/eth0"; cat "$CALLS"; bad "address apply result"; }
: >"$CALLS"; addr_run 10.10.10.162 >"$tmp/out" 2>&1 && addr_run 10.10.10.162 --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_address: a second run is a no-op and --check is clean" || { cat "$tmp/out"; cat "$CALLS"; bad "address idempotent"; }

# a different address in the same subnet: added beside the old one, the old removed only after it works
mk_net 10.10.10.170
addr_run 10.10.10.171 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "address change"; }
grep -q 'done    ncs_address' "$tmp/out" && grep -q 'arping .*10.10.10.171' "$CALLS" && grep -q 'ip addr add 10.10.10.171/24 dev eth0' "$CALLS" && grep -q 'promote_secondaries' "$CALLS" && grep -q 'ip addr del 10.10.10.170/24 dev eth0' "$CALLS" \
  && grep -qx 'eth0 10.10.10.171/24' "$STATE/ip-addrs" && ! grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && ok "ncs_address: a new address is checked for a duplicate, added beside the old, and the old removed after the proof" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs"; bad "address switch"; }

# the platform cannot be reached from the new address: everything is put back at once
mk_net 10.10.10.170; cp "$ROOT/etc/network/interfaces.d/eth0" "$tmp/eth0.before"; touch "$STATE/curl-fail"
addr_run 10.10.10.171 >"$tmp/out" 2>&1 && bad "an unproved change was kept"
cmp -s "$tmp/eth0.before" "$ROOT/etc/network/interfaces.d/eth0" && [ ! -e "$ROOT/etc/network/interfaces.d/nuit-cloud-address" ] && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && ! grep -qx 'eth0 10.10.10.171/24' "$STATE/ip-addrs" \
  && grep -q 'ifup --force eth0' "$CALLS" && grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'old configuration was put back' "$tmp/out" \
  && ok "ncs_address: when the platform cannot be reached from the new address the files and addresses are restored and DHCP restarted" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs"; bad "address rollback"; }

# refusals change nothing and arm nothing
mk_net 10.10.10.170; touch "$STATE/arping-dupe"
addr_run 10.10.10.171 >"$tmp/out" 2>&1 && bad "a duplicate address was taken"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'already answers' "$tmp/out" && ! grep -q 'systemd-run' "$CALLS" && ! grep -q 'ip addr add' "$CALLS" && ok "ncs_address: an address another device answers for is refused before anything is changed" || { cat "$tmp/out"; cat "$CALLS"; bad "address duplicate"; }
mk_net 10.10.10.170; touch "$STATE/active-NetworkManager"
addr_run 10.10.10.171 >"$tmp/out" 2>&1 && bad "NetworkManager ignored"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q 'NetworkManager manages' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address: a network managed by NetworkManager is left to a person" || { cat "$tmp/out"; bad "address NM"; }
mk_net 10.10.10.170
addr_run 10.10.20.5 >"$tmp/out" 2>&1 && bad "another subnet accepted"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q 'not in the machine.s current subnet' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address: an address in another subnet is left to a person" || { cat "$tmp/out"; bad "address subnet"; }
for bad_addr in "" "10.10.10.999" "not-an-ip" "10.10.10.171; reboot"; do
  mk_net 10.10.10.170; addr_run "$bad_addr" >"$tmp/out" 2>&1 && bad "bad address '$bad_addr' accepted"
  [ ! -s "$CALLS" ] || { cat "$CALLS"; bad "bad address '$bad_addr' changed something"; }
done; ok "ncs_address: no address, or one that is not an IPv4 address, changes nothing"

# the VLAN interface and the tenant addresses on it must survive the change; if the change loses them they are put back
mk_net 10.10.10.170; touch "$STATE/lose-vlan"
addr_run 10.10.10.171 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "healing failed"; }
grep -q 'done    ncs_address' "$tmp/out" && grep -qx 'eth0.5 10.20.0.2/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.3/24' "$STATE/ip-addrs" && grep -qx 'eth0 10.10.10.171/24' "$STATE/ip-addrs" && grep -q 'ip addr add 10.20.0.3/24 dev eth0.5' "$CALLS" \
  && ok "ncs_address: VLAN and tenant addresses lost by the change are put back, and the change stands" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs"; bad "address heal"; }
mk_net 10.10.10.170; touch "$STATE/lose-vlan" "$STATE/stubborn"; cp "$ROOT/etc/network/interfaces.d/eth0" "$tmp/eth0.before"
addr_run 10.10.10.171 >"$tmp/out" 2>&1 && bad "a change that lost the VLAN was kept"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'lost other network configuration' "$tmp/out" && grep -q 'addr eth0.5 10.20.0.3/24' "$tmp/out" && cmp -s "$tmp/eth0.before" "$ROOT/etc/network/interfaces.d/eth0" && [ ! -e "$ROOT/etc/network/interfaces.d/nuit-cloud-address" ] \
  && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && ! grep -qx 'eth0 10.10.10.171/24' "$STATE/ip-addrs" && ok "ncs_address: when other network configuration cannot be kept, everything is undone and the loss is named" || { cat "$tmp/out"; cat "$STATE/ip-addrs"; bad "address vlan rollback"; }

# 10. ncs_vlan: the tenant VLAN slice goes on the machine as a sub-interface, additive and undone if the platform is lost
stub ifup 'echo "ifup $*" >>"$CALLS"; [ -e "$STATE/ifup-fail" ] && { echo "ifup: boom" >&2; exit 1; }
case "$1" in eth0.5) for a in 10.20.0.18 10.20.0.19 10.20.0.20 10.20.0.21; do echo "eth0.5 $a/24" >>"$STATE/ip-addrs"; done;; esac'
stub ifdown 'echo "ifdown $*" >>"$CALLS"; grep -v "^eth0.5 " "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"'
stub ping 'echo "ping $*" >>"$CALLS"; [ ! -e "$STATE/gw-silent" ]'
stub iptables 'true'
stub ifquery '[ ! -e "$STATE/no-ifstate" ]'
vlan_run() { env NCS_VLAN_ID=5 NCS_VLAN_PREFIX=24 NCS_VLAN_ADDRESS=10.20.0.18 NCS_VLAN_GATEWAY=10.20.0.1 NCS_VLAN_TENANT_ADDRESSES="${TENANTS:-10.20.0.19 10.20.0.20 10.20.0.21}" NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-vlan ${1:+"$1"}; }
vlan_net() { mk_net 10.10.10.170; sed -i.bak '/^eth0.5 /d' "$STATE/ip-addrs"; rm -f "$STATE/ifup-fail" "$STATE/gw-silent" "$STATE/no-ifstate"; : >"$CALLS"; rm -f "$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5" "$ROOT/etc/modules-load.d/8021q.conf"; }

vlan_net
vlan_run --check >"$tmp/out" 2>&1 && bad "a machine without the VLAN passed the check"
grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: --check reports a missing VLAN and changes nothing" || { cat "$tmp/out"; bad "vlan check"; }
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "vlan apply"; }
f="$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5"
grep -q 'done    ncs_vlan' "$tmp/out" && grep -q '^iface eth0.5 inet static$' "$f" && grep -q '^    address 10.20.0.18/24$' "$f" && grep -q 'post-up ip addr add 10.20.0.20/24 dev eth0.5' "$f" && grep -q 'ip link add link eth0 name eth0.5 type vlan id 5' "$f" \
  && grep -q 'ip rule add fwmark 0x5 lookup 105' "$f" && grep -q 'ip route replace default via 10.20.0.1 dev eth0.5 table 105' "$f" && grep -qx '8021q' "$ROOT/etc/modules-load.d/8021q.conf" \
  && grep -q 'systemd-run.*nuit-cloud-vlan-rollback' "$CALLS" && grep -q 'ifup eth0.5' "$CALLS" && grep -q 'systemctl stop nuit-cloud-vlan-rollback.timer' "$CALLS" && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.19/24' "$STATE/ip-addrs" \
  && grep -q 'VLAN gateway 10.20.0.1 answers' "$tmp/out" && ok "ncs_vlan: the sub-interface is written like server-vlan5.sh, brought up, the machine's own address untouched, the timer cancelled" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "vlan apply result"; }
: >"$CALLS"; vlan_run >"$tmp/out" 2>&1 && vlan_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_vlan: a second run is a no-op and --check is clean" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan idempotent"; }
(TENANTS="10.20.0.19 10.20.0.20"; vlan_run --check >/dev/null 2>&1) && bad "a changed tenant block passed the check" || ok "ncs_vlan: a different tenant block is drift"
# a VLAN set up by hand with nuit-cloud/nuit-cloud-servers scripts/server-vlan5.sh is adopted as it is
cat >"$f" <<HAND
# Managed by deploy/server-vlan5.sh (nuit-cloud). Remove with: sh server-vlan5.sh --remove
auto eth0.5
iface eth0.5 inet static
    address 10.20.0.18/24
    pre-up modprobe 8021q
    post-up ip addr add 10.20.0.19/24 dev eth0.5 2>/dev/null || true
    post-up ip addr add 10.20.0.20/24 dev eth0.5 2>/dev/null || true
    post-up ip addr add 10.20.0.21/24 dev eth0.5 2>/dev/null || true
HAND
: >"$CALLS"; vlan_run --check >"$tmp/out" 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_vlan: an equivalent VLAN made by hand is adopted, not rewritten" || { cat "$tmp/out"; cat "$f"; bad "vlan adopt"; }

# the VLAN is assumed to be carried: a silent gateway is a note, not a failure
vlan_net; touch "$STATE/gw-silent"
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "silent gateway failed"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -q 'did not answer yet' "$tmp/out" && [ -f "$f" ] && ok "ncs_vlan: a gateway that does not answer is a note, the VLAN stays" || { cat "$tmp/out"; bad "vlan silent gateway"; }

# ifup fails, or the platform cannot be reached afterwards: the machine is put back
vlan_net; touch "$STATE/ifup-fail"
vlan_run >"$tmp/out" 2>&1 && bad "a failed ifup was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && grep -q 'ifup eth0.5 failed' "$tmp/out" && [ ! -e "$f" ] && ! grep -q '^eth0.5 ' "$STATE/ip-addrs" && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && ok "ncs_vlan: a failed ifup puts everything back" || { cat "$tmp/out"; bad "vlan ifup fail"; }
vlan_net; touch "$STATE/curl-fail"
vlan_run >"$tmp/out" 2>&1 && bad "an unproved VLAN was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && grep -q 'platform could not be reached' "$tmp/out" && [ ! -e "$f" ] && grep -q 'ifdown --force eth0.5' "$CALLS" && ! grep -q '^eth0.5 ' "$STATE/ip-addrs" && ok "ncs_vlan: when the platform cannot be reached afterwards the VLAN is taken off again" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan rollback"; }

# refusals
vlan_net; echo "eth1 10.20.0.19/24" >>"$STATE/ip-addrs"
vlan_run >"$tmp/out" 2>&1 && bad "an address on another interface was taken"
grep -q 'already on another interface' "$tmp/out" && ! grep -q 'systemd-run' "$CALLS" && ok "ncs_vlan: an address already on another interface is refused before anything changes" || { cat "$tmp/out"; bad "vlan duplicate"; }
vlan_net; touch "$STATE/active-NetworkManager"
vlan_run >"$tmp/out" 2>&1 && bad "NetworkManager ignored"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: a network managed by NetworkManager is left to a person" || { cat "$tmp/out"; bad "vlan NM"; }
vlan_net; touch "$STATE/no-ifstate"
vlan_run >"$tmp/out" 2>&1 && bad "a parent ifupdown does not hold up was disturbed"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && grep -q 'not held up by ifupdown' "$tmp/out" && ! grep -q 'systemd-run\|ifup' "$CALLS" && ok "ncs_vlan: a parent interface ifupdown does not hold up is left to a person (ifup would try to bring it up)" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan parent state"; }
vlan_net
env NCS_VLAN_ID=5 NCS_VLAN_PREFIX=24 NCS_VLAN_ADDRESS="10.20.0.18; reboot" NCS_VLAN_GATEWAY=10.20.0.1 NCS_VLAN_TENANT_ADDRESSES="10.20.0.19" sh "$here/bootstrap.sh" nuit-cloud-vlan >"$tmp/out" 2>&1 && bad "a bad address was accepted"
sh "$here/bootstrap.sh" nuit-cloud-vlan >"$tmp/out2" 2>&1 && bad "no slice was accepted"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && grep -q 'MANUAL  ncs_vlan' "$tmp/out2" && [ ! -s "$CALLS" ] && ok "ncs_vlan: no slice, or an invalid one, changes nothing" || { cat "$tmp/out"; bad "vlan inputs"; }

# 11. ncs_ssh_harden: passwords and root login off only when a key the platform names can still get in, with a timer until confirmed
ssh-keygen -q -t ed25519 -N '' -C test-admin -f "$tmp/adminkey" && FP="$(ssh-keygen -lf "$tmp/adminkey.pub" | awk '{print $2}')"
ssh-keygen -q -t ed25519 -N '' -C other -f "$tmp/otherkey" && OTHER="$(ssh-keygen -lf "$tmp/otherkey.pub" | awk '{print $2}')"
DROP="$ROOT/etc/ssh/sshd_config.d/00-nuit-cloud.conf"
stub getent 'echo "nuitcloud:x:1000:1000::$ROOT/home/nuitcloud:/bin/bash"'
stub stat 'echo nuitcloud'
stub ss 'echo "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*"'
stub sshd 'case "$1" in
  -t) [ -e "$STATE/sshd-bad" ] && { echo "bad config" >&2; exit 1; }; [ -e "$STATE/sshd-bad-after" ] && [ -e "$ROOT/etc/ssh/sshd_config.d/00-nuit-cloud.conf" ] && { echo "line 2: bad option" >&2; exit 1; }; exit 0;;
  -T) if [ -e "$ROOT/etc/ssh/sshd_config.d/00-nuit-cloud.conf" ] && [ ! -e "$STATE/another-file-wins" ]; then echo "permitrootlogin no"; echo "passwordauthentication no"; else echo "permitrootlogin without-password"; echo "passwordauthentication yes"; fi
      echo "pubkeyauthentication $([ -e "$STATE/pubkey-off" ] && echo no || echo yes)"; echo "port 22"; echo "authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2";;
esac'
ssh_net() {
  unset REQ; rm -rf "$ROOT/etc/ssh" "$ROOT/home/nuitcloud" "$ROOT/var/lib/nuit-cloud-agent/address/ssh"; mkdir -p "$ROOT/etc/ssh/sshd_config.d" "$ROOT/home/nuitcloud/.ssh"
  printf 'Include /etc/ssh/sshd_config.d/*.conf\nPort 22\n' >"$ROOT/etc/ssh/sshd_config"
  cat "$tmp/adminkey.pub" >"$ROOT/home/nuitcloud/.ssh/authorized_keys"; chmod 600 "$ROOT/home/nuitcloud/.ssh/authorized_keys"
  rm -f "$STATE/sshd-bad" "$STATE/sshd-bad-after" "$STATE/another-file-wins" "$STATE/pubkey-off"; : >"$CALLS"
}
ssh_run() { env NCS_SSH_REQUIRE_FINGERPRINTS="${REQ-$FP}" sh "$here/bootstrap.sh" nuit-cloud-ssh ${1:+"$1"}; }

ssh_net
ssh_run --check >"$tmp/out" 2>&1 && bad "a machine that allows passwords passed the check"
grep -q 'DRIFT   ncs_ssh_harden' "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$DROP" ] && ok "ncs_ssh_harden: --check reports password logins and changes nothing" || { cat "$tmp/out"; bad "ssh check"; }
ssh_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "ssh apply"; }
grep -q 'done    ncs_ssh_harden' "$tmp/out" && grep -qx 'PermitRootLogin no' "$DROP" && grep -qx 'PasswordAuthentication no' "$DROP" && grep -q 'systemd-run.*nuit-cloud-ssh-rollback' "$CALLS" && grep -q 'systemctl reload ssh' "$CALLS" \
  && ! grep -q 'systemctl stop nuit-cloud-ssh-rollback' "$CALLS" && [ -e "$ROOT/var/lib/nuit-cloud-agent/address/ssh/armed" ] && grep -q 'Waiting for the platform' "$tmp/out" \
  && ok "ncs_ssh_harden: the override is written and sshd reloaded, and the timer is left armed for the platform to cancel" || { cat "$tmp/out"; cat "$CALLS"; bad "ssh apply result"; }
: >"$CALLS"; ssh_run >"$tmp/out" 2>&1 && ssh_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_ssh_harden: a second run is a no-op" || { cat "$tmp/out"; cat "$CALLS"; bad "ssh idempotent"; }

# refusals: nothing is written, no timer is armed
refuse() { # refuse <description> <grep pattern>
  grep -q 'MANUAL  ncs_ssh_harden' "$tmp/out" && grep -q "$2" "$tmp/out" && [ ! -e "$DROP" ] && ! grep -q 'systemd-run' "$CALLS" && ok "ncs_ssh_harden: $1" || { cat "$tmp/out"; cat "$CALLS"; bad "ssh refusal: $1"; }
}
ssh_net; REQ="" ssh_run >"$tmp/out" 2>&1 && bad "no named key accepted"; refuse "no key named by the platform: nothing changes" 'named no administrator key'
ssh_net; REQ="$OTHER" ssh_run >"$tmp/out" 2>&1 && bad "a key that is not on the machine was accepted"; refuse "none of the named keys is on the machine: passwords stay" 'none of the keys the platform expects'
ssh_net; REQ="not-a-fingerprint" ssh_run >"$tmp/out" 2>&1 && bad "a bad fingerprint was accepted"; refuse "a fingerprint that is not one is refused" 'is not a key fingerprint'
ssh_net; chmod 666 "$ROOT/home/nuitcloud/.ssh/authorized_keys"; ssh_run >"$tmp/out" 2>&1 && bad "a writable keys file was accepted"; refuse "a keys file sshd would ignore is refused" 'writable by others'
ssh_net; : >"$ROOT/home/nuitcloud/.ssh/authorized_keys"; ssh_run >"$tmp/out" 2>&1 && bad "an empty keys file was accepted"; refuse "an empty keys file is refused" 'missing or empty'
ssh_net; touch "$STATE/pubkey-off"; ssh_run >"$tmp/out" 2>&1 && bad "keys-off sshd was accepted"; refuse "sshd that does not accept keys is refused" 'does not accept keys'
ssh_net; printf 'Port 22\n' >"$ROOT/etc/ssh/sshd_config"; ssh_run >"$tmp/out" 2>&1 && bad "a config without Include was accepted"; refuse "a config that ignores sshd_config.d is refused" 'does not include'
ssh_net; touch "$STATE/sshd-bad"; ssh_run >"$tmp/out" 2>&1 && bad "an already invalid config was accepted"; refuse "a configuration that is already invalid is left alone" 'not valid as it stands'

# the new file is rejected by sshd, or another file still wins: it is taken away at once
ssh_net; touch "$STATE/sshd-bad-after"
ssh_run >"$tmp/out" 2>&1 && bad "a rejected file was kept"
grep -q 'FAILED  ncs_ssh_harden' "$tmp/out" && grep -q 'rejected the new configuration' "$tmp/out" && [ ! -e "$DROP" ] && ok "ncs_ssh_harden: a file sshd rejects is taken away at once" || { cat "$tmp/out"; bad "ssh rejected"; }
ssh_net; touch "$STATE/another-file-wins"
ssh_run >"$tmp/out" 2>&1 && bad "an ineffective file was kept"
grep -q 'FAILED  ncs_ssh_harden' "$tmp/out" && grep -q 'another file wins' "$tmp/out" && [ ! -e "$DROP" ] && ok "ncs_ssh_harden: if sshd still allows passwords afterwards the change is taken away" || { cat "$tmp/out"; bad "ssh ineffective"; }
# 12. ncs_wifi_off: Wi-Fi goes down first, the platform must answer over the wired path, and only then is the saved network removed
stub ip 'case "$*" in
  "-4 route show default") cat "$STATE/ip-route";;
  "link set "*) echo "ip $*" >>"$CALLS"; if [ "$4" = down ]; then echo down >"$ROOT/sys/class/net/$3/operstate"; else echo up >"$ROOT/sys/class/net/$3/operstate"; fi;;
  *) exit 0;;
esac'
stub rfkill 'echo "rfkill $*" >>"$CALLS"'
stub curl '[ ! -e "$STATE/curl-fail" ] && echo "curl $*" >>"$CALLS"'
wifi_net() {
  rm -rf "$ROOT"; mkdir -p "$ROOT/etc/network/interfaces.d" "$ROOT/etc/wpa_supplicant" "$ROOT/sys/class/net/eth0" "$ROOT/sys/class/net/wlan0/wireless"
  echo up >"$ROOT/sys/class/net/eth0/operstate"; echo up >"$ROOT/sys/class/net/wlan0/operstate"
  printf 'source /etc/network/interfaces.d/*\nauto lo\niface lo inet loopback\n\nauto eth0\niface eth0 inet static\n    address 10.10.10.163/24\n\nallow-hotplug wlan0\niface wlan0 inet dhcp\n    wpa-ssid home\n    wpa-psk SECRETPASSPHRASE\n\nauto eth1\niface eth1 inet dhcp\n' >"$ROOT/etc/network/interfaces"
  printf 'network={\n  ssid="home"\n  psk="SECRETPASSPHRASE"\n}\n' >"$ROOT/etc/wpa_supplicant/wpa_supplicant-wlan0.conf"
  printf 'ctrl_interface=/run/wpa_supplicant\n' >"$ROOT/etc/wpa_supplicant/other.conf"
  printf 'default via 10.10.10.1 dev %s\n' "${1:-eth0}" >"$STATE/ip-route"; rm -f "$STATE/curl-fail" "$STATE/active-NetworkManager"; : >"$CALLS"
}
wifi_run() { env NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-wired ${1:+"$1"}; }
OLDROOT2="$ROOT"; export ROOT="$tmp/wifiroot"
wifi_net; wifi_run --check >"$tmp/out" 2>&1 && bad "a machine with Wi-Fi up passed the check"
grep -q 'DRIFT   ncs_wifi_off' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_wifi_off: --check reports Wi-Fi and changes nothing" || { cat "$tmp/out"; bad "wifi check"; }
wifi_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "wifi apply"; }
grep -q 'ip link set wlan0 down' "$CALLS" && grep -q 'rfkill block wifi' "$CALLS" && grep -q 'curl.*agent.example/api/agent/download/VERSION' "$CALLS" \
  && [ ! -e "$ROOT/etc/wpa_supplicant/wpa_supplicant-wlan0.conf" ] && [ -e "$ROOT/etc/wpa_supplicant/other.conf" ] \
  && ! grep -q 'SECRETPASSPHRASE\|wlan0\|wpa-' "$ROOT/etc/network/interfaces" && grep -q '^iface eth0 inet static$' "$ROOT/etc/network/interfaces" && grep -q '^iface eth1 inet dhcp$' "$ROOT/etc/network/interfaces" \
  && ok "ncs_wifi_off: Wi-Fi off, the platform asked, the passphrase and the wlan0 stanzas gone, other interfaces and files kept" || { cat "$tmp/out"; cat "$CALLS"; cat "$ROOT/etc/network/interfaces"; bad "wifi apply result"; }
: >"$CALLS"; wifi_run >"$tmp/out" 2>&1 && wifi_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_wifi_off: a second run is a no-op" || { cat "$tmp/out"; cat "$CALLS"; bad "wifi idempotent"; }
wifi_net; touch "$STATE/curl-fail"
wifi_run >"$tmp/out" 2>&1 && bad "Wi-Fi was removed although the platform could not be reached"
grep -q 'switched back on' "$tmp/out" && grep -q 'ip link set wlan0 up' "$CALLS" && grep -q 'SECRETPASSPHRASE' "$ROOT/etc/network/interfaces" && [ -e "$ROOT/etc/wpa_supplicant/wpa_supplicant-wlan0.conf" ] && ! grep -q rfkill "$CALLS" \
  && ok "ncs_wifi_off: when the platform is lost the interface comes back and nothing is removed" || { cat "$tmp/out"; cat "$CALLS"; bad "wifi rollback"; }
wifi_net wlan0; wifi_run >"$tmp/out" 2>&1 && bad "Wi-Fi was turned off while it carried the default route"
grep -q 'MANUAL  ncs_wifi_off' "$tmp/out" && ! grep -q 'ip link' "$CALLS" && ok "ncs_wifi_off: the default route on Wi-Fi is left to a person (connect a cable)" || { cat "$tmp/out"; bad "wifi default route"; }
export ROOT="$OLDROOT2"
# 13. ncs_login: the platform's public key is added to the login account's authorized_keys, and nothing else is touched
ssh-keygen -q -t ed25519 -N '' -C platform -f "$tmp/platkey"; PK="$(cat "$tmp/platkey.pub")"
stub id 'case "$*" in "-nG nuitcloud") echo nuitcloud;; "-gn nuitcloud") echo nuitcloud;; "nuitcloud") [ -e "$STATE/nouser" ] && exit 1; exit 0;; *) /usr/bin/id "$@";; esac'
stub chown 'echo "chown $*" >>"$CALLS"'
login_run() { env NCS_PLATFORM_SSH_KEY="$PK" sh "$here/bootstrap.sh" nuit-cloud-login ${1:+"$1"}; }
OLDROOT3="$ROOT"; export ROOT="$tmp/loginroot"; rm -rf "$ROOT" "$STATE/nouser"; mkdir -p "$ROOT/home/nuitcloud/.ssh"; : >"$CALLS"
printf 'ssh-ed25519 AAAAexisting admin@laptop' >"$ROOT/home/nuitcloud/.ssh/authorized_keys"
login_run --check >"$tmp/out" 2>&1 && bad "a missing key passed the check"
grep -q 'DRIFT   ncs_login' "$tmp/out" && ! grep -q platform "$ROOT/home/nuitcloud/.ssh/authorized_keys" && ok "ncs_login: --check reports the missing key and changes nothing" || { cat "$tmp/out"; bad "login check"; }
login_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; env NCS_PLATFORM_SSH_KEY="$PK" od -c "$ROOT/home/nuitcloud/.ssh/authorized_keys" | head -12; bad "login apply"; }

f="$ROOT/home/nuitcloud/.ssh/authorized_keys"
[ "$(wc -l <"$f")" -eq 2 ] && grep -q 'AAAAexisting admin@laptop' "$f" && grep -qxF "$PK" "$f" && [ "$(ls -l "$f" | cut -c1-10)" = "-rw-------" ] \
  && ok "ncs_login: the key is added on its own line (a file without a final newline too), the existing key kept, mode 600" || { cat "$tmp/out"; cat "$f"; bad "login apply result"; }
: >"$CALLS"; login_run >/dev/null 2>&1 && login_run --check >/dev/null 2>&1 && [ "$(wc -l <"$f")" -eq 2 ] && ok "ncs_login: a second run adds nothing" || bad "login idempotent"
touch "$STATE/nouser"; login_run >"$tmp/out" 2>&1 && bad "a missing account was accepted"
grep -q 'MANUAL  ncs_login' "$tmp/out" && grep -q 'does not exist' "$tmp/out" && ok "ncs_login: a missing account is left to a person" || { cat "$tmp/out"; bad "login no user"; }
rm -f "$STATE/nouser"; env NCS_PLATFORM_SSH_KEY="ssh-rsa AAAA x" sh "$here/bootstrap.sh" nuit-cloud-login >"$tmp/out" 2>&1 && bad "a non-ed25519 key was accepted"
grep -q 'MANUAL  ncs_login' "$tmp/out" && ok "ncs_login: only an ssh-ed25519 key is accepted" || bad "login key type"
export ROOT="$OLDROOT3"
# 14. ncs_firewall: a drop-by-default input table, syntax-checked, with a timer armed before it is loaded and left armed for the platform
stub nft 'echo "nft $*" >>"$CALLS"
case "$1 $2" in
  "-c -f") [ ! -e "$STATE/nft-bad" ] || { echo "syntax error" >&2; exit 1; };;
  "-f "*) [ ! -e "$STATE/nft-load-fail" ] || { echo "cannot load" >&2; exit 1; }; cp "$2" "$STATE/nft-loaded";;
  "list table") [ -e "$STATE/nft-loaded" ];;
  "delete table") rm -f "$STATE/nft-loaded";;
esac'
fw_net() { rm -rf "$ROOT"; mkdir -p "$ROOT/var/lib/nuit-cloud-agent"; rm -f "$STATE/nft-bad" "$STATE/nft-load-fail" "$STATE/nft-loaded" "$STATE/ss-no-ssh"; : >"$CALLS"; }
stub ss 'if [ -e "$STATE/ss-no-ssh" ]; then :; else echo "LISTEN 0 128 0.0.0.0:22 0.0.0.0:*"; fi'
fw_run() { env ${FW_TCP+NCS_FIREWALL_TCP="$FW_TCP"} ${FW_FROM+NCS_FIREWALL_SSH_FROM="$FW_FROM"} sh "$here/bootstrap.sh" nuit-cloud-firewall ${1:+"$1"}; }
OLDROOT4="$ROOT"; export ROOT="$tmp/fwroot"; RULES="$ROOT/etc/nuit-cloud/firewall.nft"
export FW_TCP="80,443" FW_FROM="10.10.10.0/24"
fw_net; fw_run --check >"$tmp/out" 2>&1 && bad "a machine with no firewall passed the check"
grep -q 'DRIFT   ncs_firewall' "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$RULES" ] && ok "ncs_firewall: --check reports no firewall and changes nothing" || { cat "$tmp/out"; bad "fw check"; }
fw_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "fw apply"; }
grep -q 'done    ncs_firewall' "$tmp/out" && grep -q 'policy drop' "$RULES" && grep -q 'tcp dport 22 ip saddr { 10.10.10.0/24 } accept' "$RULES" && grep -q 'tcp dport { 80,443 } accept' "$RULES" && grep -q 'ct state established,related accept' "$RULES" && grep -q '"ocisaas0"' "$RULES" \
  && grep -q 'nft -c -f' "$CALLS" && grep -q 'systemd-run.*nuit-cloud-firewall-rollback' "$CALLS" && ! grep -q 'systemctl stop nuit-cloud-firewall-rollback' "$CALLS" && [ -e "$ROOT/var/lib/nuit-cloud-agent/address/firewall/armed" ] && grep -q 'Waiting for the platform' "$tmp/out" \
  && ok "ncs_firewall: the rules are checked then loaded, SSH and the named ports open, pods' bridges kept, and the timer is left armed for the platform" || { cat "$tmp/out"; cat "$CALLS"; cat "$RULES"; bad "fw apply result"; }
awk '/systemd-run/ {r=NR} /nft -f / {l=NR} END { exit (r && l && r < l) ? 0 : 1 }' "$CALLS" && ok "ncs_firewall: the safety timer is armed before the rules are loaded" || { cat "$CALLS"; bad "fw timer order"; }
: >"$CALLS"; fw_run >"$tmp/out" 2>&1 && fw_run --check >/dev/null 2>&1 && ! grep -q 'nft -f' "$CALLS" && ok "ncs_firewall: a second run is a no-op" || { cat "$tmp/out"; cat "$CALLS"; bad "fw idempotent"; }
FW_TCP="80,443,8080" fw_run --check >/dev/null 2>&1 && bad "a different port list passed the check" || ok "ncs_firewall: a different port list is drift"
fw_net; touch "$STATE/nft-bad"; fw_run >"$tmp/out" 2>&1 && bad "rules nft rejects were loaded"
grep -q 'FAILED  ncs_firewall' "$tmp/out" && grep -q 'nft rejected the rules' "$tmp/out" && [ ! -e "$RULES" ] && ! grep -q 'systemd-run' "$CALLS" && ok "ncs_firewall: rules nft rejects change nothing (no timer, no file)" || { cat "$tmp/out"; cat "$CALLS"; bad "fw check fail"; }
fw_net; touch "$STATE/nft-load-fail"; fw_run >"$tmp/out" 2>&1 && bad "a failed load was kept"
grep -q 'nft could not load the rules' "$tmp/out" && grep -q 'nft delete table' "$CALLS" && [ ! -e "$RULES" ] && [ ! -e "$ROOT/var/lib/nuit-cloud-agent/address/firewall/armed" ] && ok "ncs_firewall: a failed load is undone at once" || { cat "$tmp/out"; cat "$CALLS"; bad "fw load fail"; }
fw_net; touch "$STATE/ss-no-ssh"; fw_run >"$tmp/out" 2>&1 && bad "a firewall was kept with sshd not listening"
grep -q 'sshd is not listening on port 22' "$tmp/out" && grep -q 'nft delete table' "$CALLS" && ok "ncs_firewall: if sshd is not listening afterwards the firewall is taken away" || { cat "$tmp/out"; bad "fw ssh listen"; }
fw_net; FW_TCP="80,abc" fw_run >"$tmp/out" 2>&1 && bad "an invalid port was accepted"
grep -q 'MANUAL  ncs_firewall' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_firewall: an invalid port or range changes nothing" || { cat "$tmp/out"; bad "fw bad port"; }
unset FW_TCP FW_FROM
fw_net; fw_run >"$tmp/out" 2>&1 || bad "default apply"
grep -q 'tcp dport 22 ip saddr { 10.0.0.0/8,172.16.0.0/12,192.168.0.0/16 } accept' "$RULES" && ! grep -q 'tcp dport {' "$RULES" && ok "ncs_firewall: with no ranges or ports named, SSH from the private ranges only" || { cat "$RULES"; bad "fw defaults"; }
export ROOT="$OLDROOT4"
# 15. ncs_housekeeping: a weekly timer, written once and never over someone else's
HKROOT="$tmp/hkroot"; mkdir -p "$HKROOT"; OLDROOT5="$ROOT"; export ROOT="$HKROOT"; : >"$CALLS"
sh -c 'ROOT="'"$ROOT"'"; NC_USER=nuitcloud; JOURNAL_MAX=50M; p() { printf "%s%s\n" "$ROOT" "$1"; }; write_if_missing() { f="$(p "$1")"; if [ -e "$f" ]; then cat >/dev/null; return 0; fi; mkdir -p "$(dirname "$f")"; cat >"$f"; }; systemctl() { echo "systemctl $*" >>"'"$CALLS"'"; }; . "'"$here"'/features.sh"; f_ncs_housekeeping_ok && exit 9; f_ncs_housekeeping_do; f_ncs_housekeeping_ok || exit 8; echo custom >"$(p /etc/systemd/system/nuit-cloud-housekeeping.timer)"; f_ncs_housekeeping_do; exit 0'
rc=$?
[ "$rc" = 0 ] && grep -q 'journalctl --vacuum-size=50M' "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.service" && grep -q 'OnCalendar=weekly' "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.timer" || true
[ "$rc" = 0 ] && grep -q 'systemctl enable --now nuit-cloud-housekeeping.timer' "$CALLS" && grep -qx custom "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.timer" && ok "ncs_housekeeping: the service and weekly timer are written, enabled, and a timer someone else wrote is not overwritten" || { echo "rc=$rc"; cat "$CALLS"; bad "housekeeping"; }
export ROOT="$OLDROOT5"
# 16. ncs_vlan on a minimal Debian: iptables is installed first instead of the feature giving up
vlan_net; mv "$tmp/bin/iptables" "$tmp/iptables.away"
vlan_run >"$tmp/out" 2>&1 && bad "the VLAN was applied with no iptables"
mv "$tmp/iptables.away" "$tmp/bin/iptables"
grep -q 'apt-get install -y -qq iptables' "$CALLS" && ok "ncs_vlan: iptables is installed when it is missing (a new machine has none before the runtime goes on)" || { cat "$CALLS"; bad "vlan iptables install"; }
export ROOT="$OLDROOT"
# 17. ncs_log_root: the shared log root exists, setgid, so logs written by root can be read by the platform's login
OLDROOT6="$ROOT"; export ROOT="$tmp/logroot"; mkdir -p "$ROOT"; : >"$CALLS"
stub chgrp 'echo "chgrp $*" >>"$CALLS"'
log_root() { sh -c 'ROOT="'"$ROOT"'"; p() { printf "%s%s\n" "$ROOT" "$1"; }; PATH="'"$tmp"'/bin:$PATH"; . "'"$here"'/features.sh"; '"$1"; }
log_root 'f_ncs_log_root_ok' && bad "the log root was reported present on a machine without it"
rc=0; log_root 'f_ncs_log_root_do' >/dev/null 2>&1 || rc=$?; [ "$rc" = 3 ] && [ ! -e "$ROOT/var/lib/oci-saas/logs" ] && ok "ncs_log_root: before the runtime is installed it waits and creates nothing" || bad "log root before runtime"
mkdir -p "$ROOT/etc/oci-saas"
log_root 'f_ncs_log_root_do && f_ncs_log_root_ok' || { cat "$CALLS"; bad "log root apply"; }
[ -g "$ROOT/var/lib/oci-saas/logs" ] && grep -q 'chgrp oci-saas .*/var/lib/oci-saas/logs' "$CALLS" && ok "ncs_log_root: the directory is made setgid and given the runtime's group" || { cat "$CALLS"; bad "log root mode"; }
: >"$CALLS"; log_root 'f_ncs_log_root_ok' && [ ! -s "$CALLS" ] && ok "ncs_log_root: once there it is satisfied and nothing is changed" || bad "log root idempotent"
export ROOT="$OLDROOT6"
grep -q 'ncs_log_root' "$here/profiles/nuit-container-service.sh" && ok "the nuit-container-service profile includes ncs_log_root" || bad "profile lacks ncs_log_root"
# 16. timesync: a clock service that is running is not enough, the clock has to be synchronised (a fresh install's was hours out and the agent refused every task)
sh -c '. "'"$here"'/features.sh"; f_timesync_ok' || bad "a running, synchronised clock should pass"
touch "$STATE/clock-unsynced"
sh -c '. "'"$here"'/features.sh"; f_timesync_ok' && bad "a running clock service whose clock is not synchronised passed"
sh -c 'TIMESYNC_WAIT_SECONDS=1; . "'"$here"'/features.sh"; f_timesync_do' 2>"$tmp/ts.err" && bad "applying should fail when the clock never synchronises"
grep -q 'not synchronising' "$tmp/ts.err" && ok "timesync: running is not enough; an unsynchronised clock is drift, and applying says why it could not fix it" || bad "timesync message"
rm -f "$STATE/clock-unsynced"
# 18. ncs_laptop_awake: a laptop is stopped from sleeping on its lid or idle (and sleep is masked); a machine that is not a laptop is left alone
LAPROOT="$tmp/laproot"; mkdir -p "$LAPROOT/sys/class/dmi/id" "$LAPROOT/sys/class/power_supply"; OLDROOT7="$ROOT"; export ROOT="$LAPROOT"; : >"$CALLS"
rm -f "$STATE/masked"
lap() { sh -c 'ROOT="'"$ROOT"'"; p() { printf "%s%s\n" "$ROOT" "$1"; }; systemctl() { case "$1" in is-enabled) [ -e "'"$STATE"'/masked" ] && echo masked || echo enabled;; mask) touch "'"$STATE"'/masked"; echo "systemctl $*" >>"'"$CALLS"'";; *) echo "systemctl $*" >>"'"$CALLS"'";; esac; }; . "'"$here"'/features.sh"; '"$1"; }
echo 3 >"$ROOT/sys/class/dmi/id/chassis_type"   # a desktop
lap 'f_ncs_laptop_awake_ok' && [ ! -e "$ROOT/etc/systemd/logind.conf.d/10-nuit-cloud-lid.conf" ] && ok "ncs_laptop_awake: a machine that is not a laptop is already as it should be, and nothing is written" || bad "not a laptop"
echo 9 >"$ROOT/sys/class/dmi/id/chassis_type"   # a laptop
lap 'f_ncs_laptop_awake_ok' && bad "a laptop that can still sleep was reported fine"
lap 'f_ncs_laptop_awake_do && f_ncs_laptop_awake_ok' || { cat "$CALLS"; bad "laptop apply"; }
c="$ROOT/etc/systemd/logind.conf.d/10-nuit-cloud-lid.conf"
grep -qx 'HandleLidSwitch=ignore' "$c" && grep -qx 'HandleLidSwitchExternalPower=ignore' "$c" && grep -qx 'HandleLidSwitchDocked=ignore' "$c" && grep -qx 'IdleAction=ignore' "$c" && grep -q 'systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target' "$CALLS" && grep -q 'kill -s HUP systemd-logind' "$CALLS" \
  && ok "ncs_laptop_awake: on a laptop the lid, idle and the sleep keys are ignored, the sleep targets are masked and logind is told to read it again" || { cat "$c" "$CALLS"; bad "laptop files"; }
: >"$CALLS"; lap 'f_ncs_laptop_awake_ok' && [ ! -s "$CALLS" ] && ok "ncs_laptop_awake: once done it is satisfied and nothing is changed" || bad "laptop idempotent"
rm -f "$STATE/masked"; lap 'f_ncs_laptop_awake_ok' && bad "a laptop whose sleep targets are not masked was reported fine"
echo 3 >"$ROOT/sys/class/dmi/id/chassis_type"; mkdir -p "$ROOT/sys/class/power_supply/BAT0"
lap 'ncs_is_laptop' && ok "ncs_laptop_awake: a battery makes it a laptop even where the chassis type says otherwise" || bad "battery fallback"
export ROOT="$OLDROOT7"
grep -q 'ncs_laptop_awake' "$here/profiles/nuit-cloud-server.sh" && ok "the base profile includes ncs_laptop_awake" || bad "profile lacks ncs_laptop_awake"

# 19. ncs_resolvers: resolv.conf lists exactly the platform's resolvers (a domain line kept), resolv.conf.head carries them, a symlink is left to a person
res_run() { env NCS_DNS_SERVERS="$1" sh "$here/bootstrap.sh" nuit-cloud-resolvers ${2:+"$2"}; }
OLDROOT9="$ROOT"; export ROOT="$tmp/resroot"; rm -rf "$ROOT"; mkdir -p "$ROOT/etc"
printf '# Generated by dhcpcd from enp0s31f6.dhcp\ndomain internal.example.com\nnameserver 10.10.10.151\nnameserver 10.10.10.153\n' >"$ROOT/etc/resolv.conf"
res_run "10.20.0.22,10.20.0.67" --check >"$tmp/out" 2>&1 && bad "dead resolvers passed the check"
grep -q 'DRIFT   ncs_resolvers' "$tmp/out" && grep -q '10.10.10.151' "$ROOT/etc/resolv.conf" && ok "ncs_resolvers: --check reports the old resolvers and changes nothing" || { cat "$tmp/out"; bad "resolvers check"; }
res_run "10.20.0.22,10.20.0.67" >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "resolvers apply"; }
[ "$(awk '$1=="nameserver" {print $2}' "$ROOT/etc/resolv.conf" | tr '\n' ' ')" = "10.20.0.22 10.20.0.67 " ] && grep -q '^domain internal.example.com' "$ROOT/etc/resolv.conf" && ! grep -q '10.10.10.15' "$ROOT/etc/resolv.conf" \
  && [ "$(grep -c '^nameserver' "$ROOT/etc/resolv.conf.head")" -eq 2 ] && ok "ncs_resolvers: the resolvers are exactly the platform's, in order, the domain line kept, and the same two are in resolv.conf.head" || { cat "$ROOT/etc/resolv.conf"; bad "resolvers result"; }
res_run "10.20.0.22,10.20.0.67" --check >/dev/null 2>&1 && ok "ncs_resolvers: once done it is satisfied" || bad "resolvers idempotent"
res_run "10.20.0.22" >/dev/null 2>&1 && [ "$(grep -c '^nameserver' "$ROOT/etc/resolv.conf")" -eq 1 ] && ok "ncs_resolvers: a shorter list replaces the longer one" || bad "resolvers shorter"
for bad_list in "dns.example.com" "10.0.0.1,10.0.0.2,10.0.0.3,10.0.0.4" "10.0.0.1;rm"; do
  res_run "$bad_list" >"$tmp/out" 2>&1 && bad "the list $bad_list was accepted"
  grep -q 'MANUAL  ncs_resolvers' "$tmp/out" || { cat "$tmp/out"; bad "resolvers list $bad_list"; }
done; ok "ncs_resolvers: a name, more than three addresses or anything else is refused and nothing is written"
rm -f "$ROOT/etc/resolv.conf"; ln -s /run/systemd/resolve/stub-resolv.conf "$ROOT/etc/resolv.conf"
res_run "10.20.0.22" >"$tmp/out" 2>&1 && bad "a symlinked resolv.conf was rewritten"
grep -q 'MANUAL  ncs_resolvers' "$tmp/out" && grep -q symlink "$tmp/out" && ok "ncs_resolvers: a symlinked resolv.conf is left to a person" || { cat "$tmp/out"; bad "resolvers symlink"; }
env -u NCS_DNS_SERVERS sh "$here/bootstrap.sh" nuit-cloud-resolvers --check >/dev/null 2>&1 && ok "ncs_resolvers: with nothing asked for there is nothing to change" || bad "resolvers unasked"
export ROOT="$OLDROOT9"
# 20. net_resolves: a feature about to download stops first when names do not resolve; a satisfied one never asks
stub getent 'echo "getent $*" >>"$STATE/getent-calls"; [ ! -e "$STATE/dns-down" ] || exit 2; case "$1" in hosts) echo "192.0.2.1 $2";; esac; exit 0'
feat() { sh -c 'ROOT="'"$ROOT"'"; p() { printf "%s%s\n" "$ROOT" "$1"; }; . "'"$here"'/features.sh"; '"$1"; }
OLDROOT10="$ROOT"; export ROOT="$tmp/dnsroot"; rm -rf "$ROOT"; mkdir -p "$ROOT/etc"; printf 'nameserver 10.10.10.151\n' >"$ROOT/etc/resolv.conf"
rm -f "$STATE"/pkg-*
touch "$STATE/dns-down"
for f in ncs_packages updates pi_zram; do
  : >"$CALLS"
  code="$(feat "f_${f}_do >\"$tmp/out\" 2>&1; echo \$?")"
  [ "$code" = 3 ] && grep -q 'cannot resolve names (its resolvers: 10.10.10.151): set them first (the platform' "$tmp/out" && [ ! -s "$CALLS" ] || { cat "$tmp/out" "$CALLS"; bad "$f without dns (code $code)"; }
done
ok "net_resolves: ncs_packages, updates and pi_zram return 3 with the message and run no apt-get"
rm -f "$STATE/dns-down"; : >"$CALLS"
feat 'f_ncs_packages_do' >/dev/null 2>&1 && grep -q 'apt-get install -y -qq ca-certificates curl' "$CALLS" && ok "net_resolves: with names resolving the download goes ahead as before" || bad "ncs_packages with dns"
touch "$STATE/dns-down" "$STATE/pkg-zram-tools" "$STATE/pkg-unattended-upgrades"; rm -f "$STATE/getent-calls"; : >"$CALLS"
feat 'f_updates_do; f_pi_zram_do' >/dev/null 2>&1 || true
[ ! -e "$STATE/getent-calls" ] && ok "net_resolves: packages already installed means no name lookup at all" || bad "getent called with packages present"
rm -f "$STATE/getent-calls"; touch "$STATE/pkg-ca-certificates" "$STATE/pkg-curl" "$STATE/pkg-iproute2" "$STATE/pkg-iputils-arping"
feat 'f_ncs_packages_ok' && [ ! -e "$STATE/getent-calls" ] && ok "net_resolves: a satisfied feature never asks (a machine that needs nothing downloaded is not blocked by DNS)" || bad "ok path asked dns"
rm -f "$STATE/dns-down" "$STATE/getent-calls" "$STATE"/pkg-*
export ROOT="$OLDROOT10"

# 21. control-plane services (nuit-cloud-control-plane): native systemd services, no container engine, started at boot with no platform.
# The agent has unpacked the bundle and written the secrets to NCS_CP_CONFIG_DIR before the scripts run; these tests plant distinctive fake
# secrets there and prove no script output, call, unit or generated file ever holds one.
for c in systemctl apt-get id; do cp -p "$tmp/bin/$c" "$tmp/$c.orig"; done
stub systemctl 'case "$1" in
  daemon-reload) echo "systemctl daemon-reload" >>"$CALLS";;
  enable) echo "systemctl $*" >>"$CALLS"; touch "$STATE/cp-enabled-$2";;
  restart|start) echo "systemctl $*" >>"$CALLS"; touch "$STATE/cp-active-$2";;
  reload) echo "systemctl $*" >>"$CALLS"; [ -e "$STATE/cp-active-$2" ] || exit 1;;
  is-active) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/cp-active-$1" ] && exit 0; exit 1;;
  is-enabled) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/cp-enabled-$1" ] && exit 0; exit 1;;
esac; exit 0'
stub apt-get 'echo "apt-get $*" >>"$CALLS"
for a in "$@"; do case "$a" in -*|update|install) ;; *)
  touch "$STATE/pkg-$a"
  if [ "$a" = nginx ]; then
    [ ! -e "$ROOT/usr/sbin/policy-rc.d" ] || touch "$STATE/policy-rc-seen"
    mkdir -p "$ROOT/etc/nginx/sites-enabled" "$ROOT/etc/nginx/conf.d"; touch "$ROOT/etc/nginx/sites-enabled/default"
  fi;; esac; done; exit 0'
stub id "case \"\$*\" in \"-u ncs-\"*) [ -e \"\$STATE/user-\$2\" ] && exit 0; exit 1;; *) exec $tmp/id.orig \"\$@\";; esac"
stub useradd 'echo "useradd $*" >>"$CALLS"; for a in "$@"; do u="$a"; done; touch "$STATE/user-$u"; exit 0'
stub nginx '[ "$1" = -t ] || exit 0; echo "nginx -t" >>"$CALLS"; if [ -e "$STATE/nginx-bad" ]; then echo "nginx: [emerg] unknown directive in conf:3" >&2; exit 1; fi; exit 0'
OLDROOT21="$ROOT"; export ROOT="$tmp/cproot"; rm -rf "$ROOT" "$STATE"/cp-* "$STATE"/user-* "$STATE"/pkg-* "$STATE"/nginx-bad "$STATE"/policy-rc-seen; mkdir -p "$ROOT/etc"
CPCONF="$tmp/cpconf"; CPOPT="$tmp/cpopt"; rm -rf "$CPCONF" "$CPOPT"; mkdir -p "$CPCONF/tls" "$CPOPT"
SECRET_SCHED="FIXTURE-SCHEDULER-TOKEN-7f3a91c2"; SECRET_KEY="FIXTURE-TLS-PRIVATE-KEY-b81d44e0"; SECRET_PROBE_U="FIXTURE-PROBE-UPDATE-5c0e22aa"; SECRET_PROBE_B="FIXTURE-PROBE-BOOTSTRAP-9d17f3b6"
printf 'RENEWAL_SWEEP_TOKEN="%s"\n' "$SECRET_SCHED" >"$CPCONF/scheduler.env"; chmod 600 "$CPCONF/scheduler.env"
printf -- '-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----\n' "$SECRET_KEY" >"$CPCONF/tls/key.pem"; chmod 600 "$CPCONF/tls/key.pem"
printf -- '-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n' >"$CPCONF/tls/cert.pem"
printf 'DYNDNS_UPDATE_TOKEN="%s"\nDYNDNS_BOOTSTRAP_TOKEN="%s"\n' "$SECRET_PROBE_U" "$SECRET_PROBE_B" >"$CPCONF/probe.env"; chmod 600 "$CPCONF/probe.env"
hex64() { printf "$1%.0s" $(seq 1 64); }
REV1="$(hex64 a)"; REV2="$(hex64 b)"
# mkbundle <version> <hex char>: what the agent leaves after unpacking a bundle
mkbundle() {
  d="$CPOPT/$1"; rm -rf "$d"; mkdir -p "$d/scheduler" "$d/probe"
  printf '%s\n' "$1" >"$d/VERSION"; hex64 "$2" >"$d/.bundle-sha256"; echo >>"$d/.bundle-sha256"
  printf 'ofelia binary %s\n' "$1" >"$d/ofelia"; chmod 755 "$d/ofelia"
  printf '[job-local "x"]\ncommand = sh /etc/ofelia/monitor-run.sh\n; %s\n' "$1" >"$d/scheduler/config.ini"
  printf '#!/bin/sh\necho monitor %s\n' "$1" >"$d/scheduler/monitor-run.sh"; printf '#!/bin/sh\necho report %s\n' "$1" >"$d/scheduler/report-job.sh"; printf '#!/bin/sh\necho http-post %s\n' "$1" >"$d/scheduler/http-post.sh"
  printf '#!/bin/sh\necho probe %s\n' "$1" >"$d/probe/probe.sh"
}
mkbundle 1.0.0 1
export NCS_CP_CONFIG_DIR="$CPCONF" NCS_CP_INSTALL_DIR="$CPOPT" NCS_CP_SERVICES="scheduler tls-front probe" NCS_CP_REVISION="$REV1" NCS_CP_MISSING="" NCS_CP_TLS_UPSTREAM="10.10.10.140:4173" NCS_CP_START_WAIT_SECONDS=2
export NCS_CP_BUNDLE_VERSION=1.0.0 NCS_CP_BUNDLE_SHA256="$(hex64 1)"
printf '%s\n' "$REV1" >"$CPCONF/revision"
cp_run() { sh "$here/bootstrap.sh" nuit-cloud-control-plane "$@"; }
cpout="$tmp/cp-all"; : >"$cpout"
UNITS="$ROOT/etc/systemd/system"; NGX="$ROOT/etc/nginx"; OFE="$ROOT/etc/ofelia"
count() { n="$(grep -c "$1" "$CALLS" || true)"; echo "$n"; }
syslog_line() { grep -n "$1" "$CALLS" | head -n 1 | cut -d: -f1; }

# the profile no longer carries anything of a container engine
! grep -q 'docker' "$here/profiles/nuit-cloud-control-plane.sh" && ! cp_run --list | grep -q 'docker_logs.*control' && ok "control plane: the profile names no docker feature" || bad "control plane profile still mentions docker"

# check on a fresh machine: drift for all three, and nothing is changed
: >"$CALLS"
if cp_run --check >"$tmp/out" 2>&1; then bad "control plane: a fresh machine passed the check"; fi; cat "$tmp/out" >>"$cpout"
grep -q 'DRIFT   ncs_cp_scheduler' "$tmp/out" && grep -q 'DRIFT   ncs_cp_tls_front' "$tmp/out" && grep -q 'DRIFT   ncs_cp_probe' "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$UNITS" ] && [ ! -e "$OFE" ] && [ ! -e "$CPOPT/current" ] \
  && ok "control plane: --check reports drift for all three services and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "control plane check"; }

cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane apply failed"; }; cat "$tmp/out" >>"$cpout"
grep -q 'done    ncs_cp_scheduler' "$tmp/out" && grep -q 'done    ncs_cp_tls_front' "$tmp/out" && grep -q 'done    ncs_cp_probe' "$tmp/out" || { cat "$tmp/out"; bad "control plane apply result"; }
[ "$(readlink "$CPOPT/current")" = 1.0.0 ] && ok "control plane: current points at the signed version" || bad "control plane current link"
grep -q 'useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ncs-scheduler' "$CALLS" && grep -q 'useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ncs-probe' "$CALLS" && ! grep -q 'useradd.*--password\|useradd.*-p ' "$CALLS" \
  && ok "control plane: the two system users have no login shell, no home and no password" || { cat "$CALLS"; bad "control plane users"; }
grep -q 'apt-get install -y -qq curl ca-certificates' "$CALLS" && grep -q 'apt-get install -y -qq nginx' "$CALLS" && [ "$(count 'apt-get install')" = 2 ] \
  && ok "control plane: curl and ca-certificates (scheduler) and nginx are installed when absent, no wget, and the probe asks for nothing already there" || { cat "$CALLS"; bad "control plane packages"; }
[ -e "$STATE/policy-rc-seen" ] && [ ! -e "$ROOT/usr/sbin/policy-rc.d" ] && [ ! -e "$NGX/sites-enabled/default" ] && ok "control plane: nginx is installed without starting it, and Debian's default site is removed" || bad "control plane nginx install"
for f in config.ini monitor-run.sh report-job.sh http-post.sh; do cmp -s "$CPOPT/1.0.0/scheduler/$f" "$OFE/$f" && [ "$(ls -l "$OFE/$f" | cut -c1-10)" = "-rw-r--r--" ] || { bad "control plane /etc/ofelia/$f"; }; done
ok "control plane: the scheduler's configuration and scripts are in /etc/ofelia, root-owned mode 0644 files"
# http-post.sh (sourced by the other scripts through $(dirname "$0")) sits beside them, and /etc/ofelia holds only files from the bundle's scheduler directory
[ -f "$OFE/http-post.sh" ] && ok "control plane: http-post.sh is installed in /etc/ofelia next to the scripts that source it" || bad "control plane http-post.sh not installed"
stray=0; for f in "$OFE"/* "$OFE"/.[!.]*; do [ -e "$f" ] || continue; [ -f "$CPOPT/1.0.0/scheduler/$(basename "$f")" ] || { echo "stray: $f" >&2; stray=1; }; done
[ "$stray" = 0 ] && [ "$(ls "$OFE" | wc -l | tr -d ' ')" = "$(ls "$CPOPT/1.0.0/scheduler" | wc -l | tr -d ' ')" ] && ok "control plane: every file in /etc/ofelia is a file from the bundle's scheduler directory, and every one of them is there" || bad "control plane /etc/ofelia holds something else"
! grep -rq 'wget' "$CALLS" && ok "control plane: wget is not installed or asked for" || bad "control plane still wants wget"

su="$UNITS/nuit-cloud-cp-scheduler.service"
for line in "User=ncs-scheduler" "EnvironmentFile=$CPCONF/scheduler.env" "ExecStart=$CPOPT/current/ofelia daemon --config /etc/ofelia/config.ini" "Restart=always" "RestartSec=10" "After=network-online.target" "Wants=network-online.target" "WantedBy=multi-user.target" \
  "NoNewPrivileges=yes" "ProtectSystem=strict" "ProtectHome=yes" "PrivateTmp=yes" "PrivateDevices=yes" "ProtectKernelTunables=yes" "ProtectKernelModules=yes" "ProtectControlGroups=yes" "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX" "CapabilityBoundingSet=" "MemoryMax=128M" "TasksMax=64"; do
  grep -qxF "$line" "$su" || { cat "$su"; bad "control plane scheduler unit lacks: $line"; }
done
pu="$UNITS/nuit-cloud-cp-probe.service"
for line in "User=ncs-probe" "EnvironmentFile=$CPCONF/probe.env" "StateDirectory=nuit-cloud-probe" "StateDirectoryMode=0700" "Environment=TOKEN_STATE_FILE=/var/lib/nuit-cloud-probe/update-token" "Environment=PROBE_VERSION=1.0.0" "ExecStart=/bin/sh $CPOPT/current/probe/probe.sh" "Restart=always" "WantedBy=multi-user.target" \
  "NoNewPrivileges=yes" "ProtectSystem=strict" "ProtectHome=yes" "PrivateTmp=yes" "PrivateDevices=yes" "ProtectKernelTunables=yes" "ProtectKernelModules=yes" "ProtectControlGroups=yes" "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX" "CapabilityBoundingSet=" "MemoryMax=64M" "TasksMax=32"; do
  grep -qxF "$line" "$pu" || { cat "$pu"; bad "control plane probe unit lacks: $line"; }
done
! grep -q 'docker\|--env-file' "$su" "$pu" && ok "control plane: the scheduler and probe units are the specified native units, fully hardened, with no container engine in them" || bad "control plane unit mentions docker"

conf="$NGX/conf.d/nuit-cloud-agent-front.conf"; dropin="$UNITS/nginx.service.d/10-nuit-cloud.conf"
[ "$(grep -c 'listen ' "$conf")" -eq 1 ] && grep -q 'listen 8443 ssl;' "$conf" && grep -q 'proxy_pass http://10.10.10.140:4173;' "$conf" && grep -q "ssl_certificate     $CPCONF/tls/cert.pem;" "$conf" && grep -q "ssl_certificate_key $CPCONF/tls/key.pem;" "$conf" \
  && [ "$(grep -c 'location ' "$conf")" -eq 2 ] && grep -q 'location /api/agent/' "$conf" && grep -q 'location / { return 404; }' "$conf" \
  && ok "control plane: nginx listens on 8443 only, forwards only /api/agent/ to the named upstream, 404 for the rest, with the agent's certificate and key" || { cat "$conf"; bad "control plane nginx.conf"; }
for line in "NoNewPrivileges=yes" "PrivateTmp=yes" "ProtectHome=yes" "MemoryMax=64M" "TasksMax=64"; do grep -qxF "$line" "$dropin" || bad "control plane nginx drop-in lacks $line"; done
! grep -q 'ProtectSystem\|CapabilityBoundingSet\|PrivateDevices\|RestrictAddressFamilies\|ProtectKernel\|ProtectControlGroups\|ReadWrite' "$dropin" && ok "control plane: the nginx drop-in is the modest set and nothing more" || { cat "$dropin"; bad "control plane drop-in too strong"; }
[ "$(syslog_line 'nginx -t')" -lt "$(syslog_line 'systemctl restart nginx.service')" ] && ok "control plane: nginx -t ran before nginx was started" || { cat "$CALLS"; bad "control plane nginx -t order"; }
[ "$(count 'systemctl enable')" = 3 ] && [ "$(count 'systemctl restart')" = 3 ] && ok "control plane: each service is enabled at boot and started" || { cat "$CALLS"; bad "control plane enable/restart"; }
cp_run --check >"$tmp/out" 2>&1 && ok "control plane: after the apply the check is clean" || { cat "$tmp/out"; bad "control plane check after apply"; }
cat "$tmp/out" >>"$cpout"

# idempotent: nothing is installed, written, reloaded or restarted
: >"$CALLS"; before="$(cat "$UNITS"/nuit-cloud-cp-*.service "$conf" "$dropin")"
cp_run >"$tmp/out" 2>&1 && ! grep -q applying "$tmp/out" && [ ! -s "$CALLS" ] && [ "$before" = "$(cat "$UNITS"/nuit-cloud-cp-*.service "$conf" "$dropin")" ] && ok "control plane: a second apply is a no-op (no install, reload or restart)" || { cat "$tmp/out" "$CALLS"; bad "control plane idempotence"; }
cat "$tmp/out" >>"$cpout"

# a new configuration revision: all three drift; the two bundle services restart, nginx only reloads, nothing is reloaded in systemd
export NCS_CP_REVISION="$REV2"
if cp_run --check >"$tmp/out" 2>&1; then bad "control plane: a new revision passed the check"; fi
[ "$(grep -c 'DRIFT   ncs_cp_' "$tmp/out")" = 3 ] && ok "control plane: --check reports all three as drifted when the platform's revision differs from the one on disk" || { cat "$tmp/out"; bad "control plane revision drift"; }
printf '%s\n' "$REV2" >"$CPCONF/revision"; : >"$CALLS"
cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane apply with new revision"; }; cat "$tmp/out" >>"$cpout"
[ "$(count 'restart nuit-cloud-cp-')" = 2 ] && [ "$(count 'reload nginx.service')" = 1 ] && [ "$(count 'restart nginx.service')" = 0 ] && [ "$(count 'daemon-reload')" = 0 ] && [ "$(count 'nginx -t')" = 1 ] && [ "$(count 'apt-get')" = 0 ] \
  && ok "control plane: a changed configuration restarts the two services and only reloads nginx (after nginx -t), with no reinstall" || { cat "$CALLS"; bad "control plane revision apply"; }

# a new bundle: drift for the two services that use it (not for nginx), current moves, previous is kept, older versions are deleted
mkbundle 1.1.0 2; export NCS_CP_BUNDLE_VERSION=1.1.0 NCS_CP_BUNDLE_SHA256="$(hex64 2)"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: a new bundle passed the check"
grep -q 'DRIFT   ncs_cp_scheduler' "$tmp/out" && grep -q 'DRIFT   ncs_cp_probe' "$tmp/out" && ! grep -q 'DRIFT   ncs_cp_tls_front' "$tmp/out" && ok "control plane: --check reports the services whose bundle on disk is not the signed one" || { cat "$tmp/out"; bad "control plane bundle drift"; }
: >"$CALLS"; cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane apply new bundle"; }; cat "$tmp/out" >>"$cpout"
[ "$(readlink "$CPOPT/current")" = 1.1.0 ] && [ "$(readlink "$CPOPT/previous")" = 1.0.0 ] && [ -d "$CPOPT/1.0.0" ] && grep -q '1.1.0' "$OFE/config.ini" && grep -qxF 'Environment=PROBE_VERSION=1.1.0' "$pu" && [ "$(count 'restart nuit-cloud-cp-')" = 2 ] && [ "$(count 'daemon-reload')" = 1 ] \
  && ok "control plane: a new bundle switches current, keeps the old one as previous, refreshes /etc/ofelia and the probe's version, and restarts both" || { cat "$CALLS"; bad "control plane bundle apply"; }
mkbundle 1.2.0 3; export NCS_CP_BUNDLE_VERSION=1.2.0 NCS_CP_BUNDLE_SHA256="$(hex64 3)"
cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane apply third bundle"; }; cat "$tmp/out" >>"$cpout"
[ "$(readlink "$CPOPT/current")" = 1.2.0 ] && [ "$(readlink "$CPOPT/previous")" = 1.1.0 ] && [ ! -e "$CPOPT/1.0.0" ] && [ -d "$CPOPT/1.1.0" ] && ok "control plane: only the current and the previous version are kept; older ones are deleted" || { ls "$CPOPT"; bad "control plane prune"; }
cp_run --check >/dev/null 2>&1 || bad "control plane: not clean after the third bundle"

# the signed bundle is not unpacked: the apply refuses and moves nothing; a bundle rebuilt under the same version has another checksum
export NCS_CP_BUNDLE_VERSION=9.0.0 NCS_CP_BUNDLE_SHA256="$(hex64 9)"; : >"$CALLS"
cp_run >"$tmp/out" 2>&1 && bad "control plane: an apply without the signed bundle succeeded"; cat "$tmp/out" >>"$cpout"
grep -q 'FAILED  ncs_cp_scheduler' "$tmp/out" && grep -q 'is not unpacked' "$tmp/out" && [ "$(readlink "$CPOPT/current")" = 1.2.0 ] && [ "$(count 'restart nuit-cloud-cp-')" = 0 ] && ok "control plane: a bundle the agent did not unpack is refused and current stays where it was" || { cat "$tmp/out" "$CALLS"; bad "control plane unpacked gate"; }
export NCS_CP_BUNDLE_VERSION=1.2.0 NCS_CP_BUNDLE_SHA256="$(hex64 4)"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: another checksum passed"
grep -q 'DRIFT   ncs_cp_scheduler' "$tmp/out" && ok "control plane: the same version with another checksum is drift" || bad "control plane checksum drift"
export NCS_CP_BUNDLE_SHA256="$(hex64 3)"

# an installed script or unit that differs from the bundle's is drift, and an apply puts it right
echo '# tampered' >>"$OFE/monitor-run.sh"; echo '# tampered' >>"$su"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: tampering passed the check"
grep -q 'DRIFT   ncs_cp_scheduler' "$tmp/out" && ok "control plane: an edited script or unit is drift" || bad "control plane tamper drift"
: >"$CALLS"; cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane repair tamper"; }; cat "$tmp/out" >>"$cpout"
! grep -q tampered "$OFE/monitor-run.sh" "$su" && [ "$(count 'daemon-reload')" = 1 ] && grep -q 'restart nuit-cloud-cp-scheduler.service' "$CALLS" && ok "control plane: an apply puts the script and unit back, reloads systemd and restarts" || { cat "$CALLS"; bad "control plane tamper apply"; }

# a service that is not active, and a missing user, are drift
rm -f "$STATE/cp-active-nuit-cloud-cp-probe.service"; rm -f "$STATE/user-ncs-scheduler"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: an inactive unit passed"
grep -q 'DRIFT   ncs_cp_probe' "$tmp/out" && grep -q 'DRIFT   ncs_cp_scheduler' "$tmp/out" && ok "control plane: an inactive unit and a missing system user are drift" || { cat "$tmp/out"; bad "control plane inactive/user"; }
: >"$CALLS"; cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane repair apply"; }; cat "$tmp/out" >>"$cpout"
grep -q 'useradd.*ncs-scheduler' "$CALLS" && grep -q 'restart nuit-cloud-cp-probe.service' "$CALLS" && cp_run --check >/dev/null 2>&1 && ok "control plane: an apply recreates the user, starts the unit, and the check is clean" || { cat "$CALLS"; bad "control plane repair"; }

# nginx -t gates everything: a configuration it refuses is taken out again, and nothing is reloaded or restarted
before_conf="$(cat "$conf")"; export NCS_CP_TLS_UPSTREAM="10.10.10.141:4173"; touch "$STATE/nginx-bad"; : >"$CALLS"
cp_run >"$tmp/out" 2>&1 && bad "control plane: a bad nginx configuration was applied"; cat "$tmp/out" >>"$cpout"
grep -q 'FAILED  ncs_cp_tls_front' "$tmp/out" && grep -q 'nginx -t refused the configuration' "$tmp/out" && [ "$(cat "$conf")" = "$before_conf" ] && [ "$(count 'reload nginx')" = 0 ] && [ "$(count 'restart nginx')" = 0 ] && [ ! -e "$conf.nuit-old" ] \
  && ok "control plane: nginx -t that fails is FAILED with a plain message, the old configuration is put back and nginx is not reloaded or restarted" || { cat "$tmp/out" "$CALLS"; bad "control plane nginx -t gate"; }
rm -f "$STATE/nginx-bad"; : >"$CALLS"; cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane nginx recovered apply"; }; cat "$tmp/out" >>"$cpout"
grep -q 'proxy_pass http://10.10.10.141:4173;' "$conf" && [ "$(count 'reload nginx.service')" = 1 ] && [ "$(count 'restart nginx.service')" = 0 ] && ok "control plane: a changed upstream is applied with a reload, not a restart" || { cat "$CALLS"; bad "control plane nginx upstream"; }
export NCS_CP_TLS_UPSTREAM="10.10.10.140:4173"; cp_run >/dev/null 2>&1 || bad "control plane upstream restored"

# an nginx the package system does not have yet, a default site that comes back, and a drop-in change that needs a restart
echo '# nginx default' >"$NGX/sites-enabled/default"; echo '# edited' >>"$dropin"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: a returned default site passed"
grep -q 'DRIFT   ncs_cp_tls_front' "$tmp/out" && ok "control plane: Debian's default site coming back, or an edited drop-in, is drift" || bad "control plane default site drift"
: >"$CALLS"; cp_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "control plane repair nginx"; }; cat "$tmp/out" >>"$cpout"
[ ! -e "$NGX/sites-enabled/default" ] && ! grep -q edited "$dropin" && [ "$(count 'restart nginx.service')" = 1 ] && [ "$(count 'daemon-reload')" = 1 ] && ok "control plane: an apply removes the site, restores the drop-in and restarts nginx once" || { cat "$CALLS"; bad "control plane nginx repair"; }

# a secret the platform does not have: that service is MANUAL and names the secret, never a failure; the others go on
export NCS_CP_MISSING="control-plane/probe"; : >"$CALLS"
cp_run --check >"$tmp/out" 2>&1 && bad "control plane: a missing secret passed the check"
cp_run >"$tmp/out" 2>&1 && bad "control plane: a missing secret did not need a person"; cat "$tmp/out" >>"$cpout"
grep -q 'MANUAL  ncs_cp_probe' "$tmp/out" && grep -q 'control-plane/probe' "$tmp/out" && ! grep -q 'FAILED' "$tmp/out" && grep -q 'ok      ncs_cp_scheduler' "$tmp/out" \
  && ok "control plane: a service whose secret the platform lacks is MANUAL with the secret's name, not FAILED" || { cat "$tmp/out"; bad "control plane missing"; }
export NCS_CP_MISSING=""

# the https front needs to know where to forward to; without it, a person is told
unset NCS_CP_TLS_UPSTREAM
cp_run >"$tmp/out" 2>&1 && bad "control plane: no upstream did not need a person"; cat "$tmp/out" >>"$cpout"
grep -q 'MANUAL  ncs_cp_tls_front' "$tmp/out" && grep -q 'tlsFront.upstream' "$tmp/out" && ok "control plane: an https front with no upstream is MANUAL, saying what is missing" || { cat "$tmp/out"; bad "control plane upstream"; }
export NCS_CP_TLS_UPSTREAM="10.10.10.140:4173"

# only the services the task names are touched
export NCS_CP_SERVICES="probe"; : >"$CALLS"; rm -f "$UNITS/nuit-cloud-cp-scheduler.service"
cp_run >"$tmp/out" 2>&1 && [ ! -e "$UNITS/nuit-cloud-cp-scheduler.service" ] && [ ! -s "$CALLS" ] && ok "control plane: a service the task does not name is left alone" || { cat "$tmp/out" "$CALLS"; bad "control plane subset"; }
export NCS_CP_SERVICES="scheduler tls-front probe"
export NCS_CP_BUNDLE_VERSION="../evil" ; cp_run >"$tmp/out" 2>&1 && bad "control plane: an unusable bundle version was accepted"
grep -q 'MANUAL  ncs_cp_probe' "$tmp/out" && grep -q 'no usable bundle' "$tmp/out" && ok "control plane: a bundle version that is not a plain version is refused" || { cat "$tmp/out"; bad "control plane bad version"; }
export NCS_CP_BUNDLE_VERSION=1.2.0

# no secret in anything the scripts printed, ran or generated
leaks=0
for s in "$SECRET_SCHED" "$SECRET_KEY" "$SECRET_PROBE_U" "$SECRET_PROBE_B"; do
  grep -rqF -- "$s" "$cpout" "$CALLS" "$UNITS" "$NGX" "$OFE" "$CPOPT" "$STATE" 2>/dev/null && { echo "leaked: $(printf '%s' "$s" | cut -c1-12)…" >&2; leaks=1; }
done
[ "$leaks" = 0 ] && ok "control plane: no secret value appears in any output, command, unit, drop-in, script or generated file" || bad "control plane leaked a secret"
rm -rf "$STATE"/cp-* "$STATE"/user-* "$STATE"/pkg-* "$STATE"/nginx-bad "$STATE"/policy-rc-seen
unset NCS_CP_CONFIG_DIR NCS_CP_INSTALL_DIR NCS_CP_SERVICES NCS_CP_REVISION NCS_CP_MISSING NCS_CP_TLS_UPSTREAM NCS_CP_START_WAIT_SECONDS NCS_CP_BUNDLE_VERSION NCS_CP_BUNDLE_SHA256
for c in systemctl apt-get id; do cp -p "$tmp/$c.orig" "$tmp/bin/$c"; done; rm -f "$tmp/bin/useradd" "$tmp/bin/nginx"
export ROOT="$OLDROOT21"
echo "all passed"
