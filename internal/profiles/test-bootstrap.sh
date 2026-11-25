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

# no feature fetches a script from the internet and runs it (docker comes from Debian's docker.io; nuit-cloud/nuit-cloud#129)
! grep -Eq 'get\.docker\.com|curl[^|]*\|[[:space:]]*(sh|bash)' "$src/bootstrap/features.sh" && ok "no script piped from the internet" || bad "a feature pipes a download into a shell"
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
# the file-secrets directory is the platform user's: chown records the owner for that one path, and stat answers it (the scratch root's files
# all belong to whoever runs the test); every other path goes to the real commands
REALCHOWN="$(command -v chown)"; REALSTAT="$(command -v stat)"
stub chown "for a in \"\$@\"; do last=\"\$a\"; done
case \"\$last\" in */run/nuit-cloud/kubelet) echo \"chown \$*\" >>\"\$CALLS\"; echo \"\$1\" >\"\$STATE/kubelet-owner\";; *) exec $REALCHOWN \"\$@\";; esac"
stub stat "for a in \"\$@\"; do last=\"\$a\"; done
case \"\$last\" in */run/nuit-cloud/kubelet) [ -d \"\$last\" ] || exit 1; m=\"\$($REALSTAT -c %a \"\$last\" 2>/dev/null || $REALSTAT -f %Lp \"\$last\")\"; echo \"\$(cat \"\$STATE/kubelet-owner\" 2>/dev/null || echo root) \$m\";; *) exec $REALSTAT \"\$@\";; esac"

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
stub systemd-version 'true'; stub systemctl 'case "$1" in --version) echo "systemd 257 (257.9-1)";; is-enabled|is-active|show) ;; *) echo "systemctl $*" >>"$CALLS";; esac
case "$1" in
  enable) shift; [ "$1" = --now ] && shift; touch "$STATE/enabled-$1"; touch "$STATE/active-$1";;
  is-enabled) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/enabled-$1" ];;
  is-active) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/active-$1" ];;
  show) for a in "$@"; do u="$a"; done; cat "$STATE/mainpid-$u" 2>/dev/null || echo 0;;
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

# File-mounted secrets (nuit-cloud/nuit-cloud#222): a directory of the platform user's under /run (made again at boot), and the runtime's
# containerd told it is the kubelet dir. containerd is restarted only when the one running lacks the setting. A profile of that one feature.
printf 'PROFILE_FEATURES="ncs_file_secrets"\n' >"$here/profiles/test-file-secrets.sh"
KD="$ROOT/run/nuit-cloud/kubelet"; KDROP="$ROOT/etc/systemd/system/containerd-ocisaas.service.d/20-nuit-kubelet-dir.conf"; KTMP="$ROOT/etc/tmpfiles.d/nuit-cloud-kubelet.conf"
fs_old_env() { mkdir -p "$ROOT/proc/4242"; printf 'PATH=/usr/bin\0OCI_SAAS_ARTIFACTS_DIR=/etc/oci-saas/artifacts\0' >"$ROOT/proc/4242/environ"; echo 4242 >"$STATE/mainpid-containerd-ocisaas"; }
fs_new_env() { mkdir -p "$ROOT/proc/4242"; printf 'PATH=/usr/bin\0OCI_SAAS_KUBELET_DIR=/run/nuit-cloud/kubelet\0' >"$ROOT/proc/4242/environ"; echo 4242 >"$STATE/mainpid-containerd-ocisaas"; }
rm -rf "$KD" "$KDROP" "$KTMP" "$STATE/kubelet-owner"; fs_old_env; : >"$CALLS"
run test-file-secrets --check >"$tmp/out" 2>&1 && bad "file secrets: a machine without them passed the check"
grep -q 'DRIFT   ncs_file_secrets' "$tmp/out" && grep -q 'is not a directory of nuitcloud' "$tmp/out" && grep -q 'not told OCI_SAAS_KUBELET_DIR=/run/nuit-cloud/kubelet' "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$KD" ] \
  && ok "file secrets: a machine without them is drift, says what is missing, and --check changes nothing" || { cat "$tmp/out" "$CALLS"; bad "file secrets check"; }
# a running runtime started without the setting: everything is made and containerd (only) is restarted, saying pods keep running
run test-file-secrets >"$tmp/out" 2>&1 || true
[ -d "$KD" ] && [ "$(cat "$STATE/kubelet-owner")" = nuitcloud ] && [ "$(stat "$KD" | awk '{print $2}')" = 700 ] \
  && grep -qx 'd /run/nuit-cloud 0755 root root -' "$KTMP" && grep -qx 'd /run/nuit-cloud/kubelet 0700 nuitcloud - -' "$KTMP" \
  && grep -qx '\[Service\]' "$KDROP" && grep -qx 'Environment=OCI_SAAS_KUBELET_DIR=/run/nuit-cloud/kubelet' "$KDROP" \
  && grep -qx 'systemctl daemon-reload' "$CALLS" && [ "$(grep -c 'systemctl restart containerd-ocisaas' "$CALLS")" = 1 ] && grep -q 'running pods keep running' "$tmp/out" \
  && ok "file secrets: the directory is nuitcloud's with 0700, made again at boot, the drop-in names it, and containerd is restarted once" || { cat "$tmp/out" "$CALLS"; ls -la "$KD" 2>&1; bad "file secrets apply"; }
# the restarted containerd has it: a second apply and the check change nothing
fs_new_env; : >"$CALLS"
run test-file-secrets >"$tmp/out" 2>&1 && run test-file-secrets --check >/dev/null 2>&1 && grep -q 'ok      ncs_file_secrets' "$tmp/out" && [ ! -s "$CALLS" ] \
  && ok "file secrets: once in place, apply and check change nothing (no restart)" || { cat "$tmp/out" "$CALLS"; bad "file secrets idempotent"; }
# a wrong mode or owner is drift, and is put right without restarting anything
chmod 755 "$KD"; run test-file-secrets --check >"$tmp/out" 2>&1 && bad "file secrets: mode 0755 passed"
grep -q 'DRIFT   ncs_file_secrets' "$tmp/out" && ok "file secrets: a directory others can read is drift" || { cat "$tmp/out"; bad "file secrets mode"; }
echo root >"$STATE/kubelet-owner"; run test-file-secrets --check >/dev/null 2>&1 && bad "file secrets: a directory of root's passed"
: >"$CALLS"; run test-file-secrets >"$tmp/out" 2>&1 || true
grep -q 'done    ncs_file_secrets' "$tmp/out" && [ "$(stat "$KD")" = "nuitcloud 700" ] && ! grep -q 'restart\|daemon-reload' "$CALLS" \
  && ok "file secrets: a wrong owner and mode are put right without a restart" || { cat "$tmp/out" "$CALLS"; bad "file secrets owner"; }
# the boot entry or the drop-in missing is drift; a drop-in written again with containerd already having the value restarts nothing
rm -f "$KTMP"; run test-file-secrets --check >/dev/null 2>&1 && bad "file secrets: no tmpfiles entry passed"
rm -f "$KDROP"; : >"$CALLS"; run test-file-secrets >"$tmp/out" 2>&1 || true
grep -q 'done    ncs_file_secrets' "$tmp/out" && [ -s "$KTMP" ] && [ -s "$KDROP" ] && grep -qx 'systemctl daemon-reload' "$CALLS" && ! grep -q 'restart' "$CALLS" \
  && ok "file secrets: a missing boot entry and drop-in are written again, with no restart when containerd already has the value" || { cat "$tmp/out" "$CALLS"; bad "file secrets rewrite"; }
# every file in place but the running containerd without the value (a restart that never happened): drift, and the apply restarts it
fs_old_env; run test-file-secrets --check >"$tmp/out" 2>&1 && bad "file secrets: a containerd without the value passed"
grep -q 'started without OCI_SAAS_KUBELET_DIR' "$tmp/out" || { cat "$tmp/out"; bad "file secrets live note"; }
: >"$CALLS"; run test-file-secrets >"$tmp/out" 2>&1 || true
grep -q 'systemctl restart containerd-ocisaas' "$CALLS" && ! grep -q 'daemon-reload' "$CALLS" && ok "file secrets: a running containerd without the value is drift and is restarted" || { cat "$tmp/out" "$CALLS"; bad "file secrets live"; }
# a machine whose runtime is not running yet (before the first install): files written, nothing restarted
rm -rf "$KD" "$KDROP" "$KTMP" "$STATE/kubelet-owner" "$STATE/mainpid-containerd-ocisaas"; rm -f "$STATE/active-containerd-ocisaas"; : >"$CALLS"
run test-file-secrets >"$tmp/out" 2>&1 || true
grep -q 'done    ncs_file_secrets' "$tmp/out" && [ -s "$KDROP" ] && grep -qx 'systemctl daemon-reload' "$CALLS" && ! grep -q 'restart' "$CALLS" \
  && ok "file secrets: before the runtime runs, the setting is written and nothing is restarted" || { cat "$tmp/out" "$CALLS"; bad "file secrets before runtime"; }
# a symbolic link where the directory goes is never followed
rm -rf "$KD"; mkdir -p "$tmp/elsewhere"; ln -s "$tmp/elsewhere" "$KD"; : >"$CALLS"
run test-file-secrets >"$tmp/out" 2>&1 && bad "file secrets: a symbolic link was accepted"
grep -q 'FAILED  ncs_file_secrets' "$tmp/out" && grep -q 'is not a directory: left alone' "$tmp/out" && [ -L "$KD" ] && ! grep -q chown "$CALLS" \
  && ok "file secrets: a symbolic link in the directory's place is left alone and fails" || { cat "$tmp/out" "$CALLS"; bad "file secrets symlink"; }
rm -f "$KD" "$here/profiles/test-file-secrets.sh"; touch "$STATE/active-containerd-ocisaas"
grep -q 'ncs_volumes ncs_file_secrets oci_saas_runtime' "$here/profiles/nuit-container-service.sh" && ok "the nuit-container-service profile sets up file secrets before the runtime" || bad "profile lacks ncs_file_secrets"

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
mk_net() { # mk_net <current address> : a Debian with ifupdown whose $NETIF (eth0) got <current> from DHCP, with the tenant VLAN on $TVIF (<NETIF>.5)
  _mn="${NETIF:-eth0}"; _mt="${TVIF:-$_mn.5}"
  rm -rf "$ROOT"; mkdir -p "$ROOT/etc/network/interfaces.d" "$ROOT/var/lib" "$ROOT/proc/net/vlan"
  printf 'source /etc/network/interfaces.d/*\n' >"$ROOT/etc/network/interfaces"
  printf 'auto lo\niface lo inet loopback\nauto %s\niface %s inet dhcp\n    hwaddress ether 02:00:00:00:00:01\n\n# a second interface that must stay\niface eth1 inet static\n    address 192.168.9.2/24\n' "$_mn" "$_mn" >"$ROOT/etc/network/interfaces.d/$_mn"
  printf '%s %s/24\n%s 10.20.0.2/24\n%s 10.20.0.3/24\n' "$_mn" "$1" "$_mt" "$_mt" >"$STATE/ip-addrs"; printf 'default via 10.10.10.1 dev %s\n' "$_mn" >"$STATE/ip-route"
  printf '10.10.10.0/24 dev %s proto kernel scope link src %s\n10.20.0.0/24 dev %s proto kernel scope link src 10.20.0.2\n' "$_mn" "$1" "$_mt" >"$STATE/ip-routes"; printf '0:\tfrom all lookup local\n100:\tfrom all fwmark 0x5 lookup 105\n' >"$STATE/ip-rules"; : >"$STATE/iptables"
  printf 'lo\n%s\neth1\n%s\n' "$_mn" "$_mt" >"$STATE/ip-links"
  # the kernel's own list of VLAN interfaces (what the features read to find a VLAN's interface and its parent, whatever its name)
  printf 'VLAN Dev name\t | VLAN ID\nName-Type: VLAN_NAME_TYPE_RAW_PLUS_VID_NO_PAD\n%s | 5 | %s\n' "$_mt" "$_mn" >"$ROOT/proc/net/vlan/config"
  rm -f "$STATE/active-NetworkManager" "$STATE/arping-dupe" "$STATE/curl-fail" "$STATE/lose-vlan" "$STATE/stubborn" "$STATE/gw-silent"; : >"$CALLS"
  rm -f "$STATE/model" "$STATE/ip-t105" "$STATE/watch" "$STATE/gaps" "$STATE/curl-fail-after"
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
  "route replace default "*) echo "ip $*" >>"$CALLS"; r="$*"; echo "${r#route replace }" >"$STATE/ip-route";;
  "route replace "*) echo "ip $*" >>"$CALLS";;
  "link show dev "*) grep -Fqx "$4" "$STATE/ip-links";;
  "link add "*) echo "ip $*" >>"$CALLS"; echo "$6" >>"$STATE/ip-links"; [ ! -d "$ROOT/proc/net/vlan" ] || echo "$6 | ${10} | $4" >>"$ROOT/proc/net/vlan/config";;
  "link del dev "*) echo "ip $*" >>"$CALLS"; grep -Fvx "$4" "$STATE/ip-links" >"$STATE/ip-links.new"; mv "$STATE/ip-links.new" "$STATE/ip-links"
    grep -v "^$4 " "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"
    if [ -f "$ROOT/proc/net/vlan/config" ]; then grep -v "^$4 " "$ROOT/proc/net/vlan/config" >"$STATE/vlan-config.new"; mv "$STATE/vlan-config.new" "$ROOT/proc/net/vlan/config"; fi;;
  *) echo "ip $*" >>"$CALLS";;
esac'
stub ping 'echo "ping $*" >>"$CALLS"; [ ! -e "$STATE/gw-silent" ]'
stub ifup 'echo "ifup $*" >>"$CALLS"'
stub systemd-run 'echo "systemd-run $*" >>"$CALLS"'
stub arping 'echo "arping $*" >>"$CALLS"; [ ! -e "$STATE/arping-dupe" ]'
# sysctl: promote_secondaries is kept where the kernel shows it, $ROOT/proc/sys/net/ipv4/conf/<interface>/promote_secondaries
stub sysctl 'echo "sysctl $*" >>"$CALLS"
for a in "$@"; do case "$a" in net.ipv4.conf.*.promote_secondaries=*)
  [ ! -e "$STATE/no-promote" ] || exit 1
  k="${a%%=*}"; dv="${k#net.ipv4.conf.}"; dv="$(echo "${dv%.promote_secondaries}" | tr / .)"
  mkdir -p "$ROOT/proc/sys/net/ipv4/conf/$dv"; echo "${a#*=}" >"$ROOT/proc/sys/net/ipv4/conf/$dv/promote_secondaries";; esac; done'
stub sleep 'true'
stub curl '[ ! -e "$STATE/curl-fail" ] && echo "curl $*" >>"$CALLS"'
# policy rules are kept in $STATE/ip-rules the way "ip -4 rule show" prints them; a rule on ocisaas0 before the bridge exists shows
# "[detached]", as on a real kernel (checked with iproute2 6.15)
mv "$tmp/bin/ip" "$tmp/bin/ip-base"
# The tenant VLAN's tests (section 10 on, with $STATE/model) use a small model of how the kernel keeps IPv4 addresses, checked on a real
# kernel (Linux 7.0, #197): one address may be on an interface with two prefixes at once, each its own entry; the first entry of a
# network (address and prefix) is its primary and the source of the network's connected route; removing a primary removes the network's
# secondaries with it unless promote_secondaries is on, when the next one becomes primary (and the route's source, if it was the
# primary's); the connected route goes with the network's last entry. With $STATE/watch (addresses), every change checks that each of
# them is still on the machine, with some prefix, and writes to $STATE/gaps when one is not.
cat >"$tmp/bin/netmodel" <<'NETMODEL'
#!/bin/sh
A="$STATE/ip-addrs"; R="$STATE/ip-routes"; T="$STATE/ip-t105"; touch "$R" "$T"
net() { awk -v ip="${1%/*}" -v pl="${1#*/}" 'BEGIN {split(ip, o, "."); v = ((o[1] * 256 + o[2]) * 256 + o[3]) * 256 + o[4]; b = 2 ^ (32 - pl); v = int(v / b) * b
  printf "%d.%d.%d.%d/%d\n", int(v / 16777216) % 256, int(v / 65536) % 256, int(v / 256) % 256, v % 256, pl}'; }
in_net() { while read -r d c; do [ "$d" = "$1" ] && [ "$(net "$c")" = "$2" ] && echo "$c"; done <"$A"; }
promote_on() { [ "$(cat "$ROOT/proc/sys/net/ipv4/conf/$1/promote_secondaries" 2>/dev/null)" = 1 ]; }
without() { awk -v k="$1" '$0 != k' "$2" >"$2.new"; mv "$2.new" "$2"; }
drop_route() { awk -v n="$1" -v d="$2" '!($1 == n && $2 == "dev" && $3 == d)' "$R" >"$R.new"; mv "$R.new" "$R"; }
set_route() { drop_route "$1" "$2"; echo "$1 dev $2 proto kernel scope link src $3" >>"$R"; }
route_src() { awk -v n="$1" -v d="$2" '$1 == n && $3 == d {for (i = 4; i < NF; i++) if ($i == "src") print $(i + 1)}' "$R"; }
op="$1"; shift
case "$op" in
  add) c="$1"; dev="$2"; n="$(net "$c")"
    grep -Fqx "$dev $c" "$A" && { echo "RTNETLINK answers: File exists" >&2; exit 2; }
    echo "$dev $c" >>"$A"
    [ -n "$(route_src "$n" "$dev")" ] || set_route "$n" "$dev" "${c%/*}" ;;
  del) c="$1"; dev="$2"; n="$(net "$c")"
    grep -Fqx "$dev $c" "$A" || { echo "RTNETLINK answers: Cannot assign requested address" >&2; exit 2; }
    if [ "$(in_net "$dev" "$n" | head -n 1)" = "$c" ] && ! promote_on "$dev"; then
      for s in $(in_net "$dev" "$n"); do without "$dev $s" "$A"; done
    else
      without "$dev $c" "$A"
    fi
    next="$(in_net "$dev" "$n" | head -n 1)"
    if [ -z "$next" ]; then drop_route "$n" "$dev"; elif [ "$(route_src "$n" "$dev")" = "${c%/*}" ]; then set_route "$n" "$dev" "${next%/*}"; fi ;;
  route-replace)
    case " $* " in
      *" table 105 "*) l="$(echo "$*" | sed 's/ table 105//')"; awk -v d="$1" '$1 != d' "$T" >"$T.new"; echo "$l" >>"$T.new"; mv "$T.new" "$T" ;;
      *) drop_route "$1" "$3"; echo "$*" >>"$R" ;;
    esac ;;
  route-del)
    case " $* " in *" table 105 "*) awk -v d="$1" '$1 != d' "$T" >"$T.new"; mv "$T.new" "$T" ;; *) drop_route "$1" "$3" ;; esac ;;
esac
if [ -f "$STATE/watch" ]; then
  for w in $(cat "$STATE/watch"); do awk -v w="$w" '{split($2, x, "/"); if (x[1] == w) f = 1} END {exit !f}' "$A" || echo "$w missing after $op $*" >>"$STATE/gaps"; done
fi
NETMODEL
chmod +x "$tmp/bin/netmodel"
cat >"$tmp/bin/ip" <<'IPSTUB'
#!/bin/sh
case "$*" in
  "-4 route show table 105") cat "$STATE/ip-t105" 2>/dev/null; exit 0 ;;
esac
if [ -e "$STATE/model" ]; then
  case "$1 ${2:-}" in
    "addr add"|"addr del") echo "ip $*" >>"$CALLS"; exec netmodel "$2" "$3" "$5" ;;
    "route del") echo "ip $*" >>"$CALLS"; shift 2; exec netmodel route-del "$@" ;;
    "route replace") case "$*" in
        "route replace default "*" table "*) echo "ip $*" >>"$CALLS"; shift 2; exec netmodel route-replace "$@" ;;
        "route replace default "*) ;;
        "route replace "*" dev "*) echo "ip $*" >>"$CALLS"; shift 2; exec netmodel route-replace "$@" ;;
      esac ;;
  esac
fi
case "$1 ${2:-}" in
  "rule add")
    echo "ip $*" >>"$CALLS"; shift 2; r="$*"; pr="${r##* priority }"; sel="${r% priority *}"
    grep -Fqx ocisaas0 "$STATE/ip-links" || sel="$(printf '%s\n' "$sel" | sed 's/iif ocisaas0 /iif ocisaas0 [detached] /')"
    printf '%s:\tfrom all %s\n' "$pr" "$sel" >>"$STATE/ip-rules" ;;
  "rule del")
    echo "ip $*" >>"$CALLS"; shift 2; r="$*"; sel="${r% priority *}"
    awk -v s="from all $sel" '{ l = $0; sub(/ \[detached\]/, "", l); sub(/^[0-9]+:\t/, "", l); if (!gone && l == s) { gone = 1; next } print }' "$STATE/ip-rules" >"$STATE/ip-rules.new"
    mv "$STATE/ip-rules.new" "$STATE/ip-rules" ;;
  *) exec "$(dirname "$0")/ip-base" "$@" ;;
esac
IPSTUB
chmod +x "$tmp/bin/ip"
# the mangle table's PREROUTING chain, kept in $STATE/iptables one rule per line as given after "-A PREROUTING" (and listed by -S as given)
cat >"$tmp/bin/iptables" <<'IPTSTUB'
#!/bin/sh
touch "$STATE/iptables"
[ "$1 $2 $4" = "-t mangle PREROUTING" ] || { echo "iptables $*" >>"$CALLS"; exit 0; }
op="$3"; shift 4; spec="$*"
case "$op" in
  -S) echo "-P PREROUTING ACCEPT"; sed 's/^/-A PREROUTING /' "$STATE/iptables" ;;
  -C) grep -Fqx -- "$spec" "$STATE/iptables" ;;
  -A) echo "iptables -t mangle -A PREROUTING $spec" >>"$CALLS"; printf '%s\n' "$spec" >>"$STATE/iptables" ;;
  -D) echo "iptables -t mangle -D PREROUTING $spec" >>"$CALLS"; grep -Fqx -- "$spec" "$STATE/iptables" || exit 1
      awk -v s="$spec" '!gone && $0 == s { gone = 1; next } { print }' "$STATE/iptables" >"$STATE/iptables.new"; mv "$STATE/iptables.new" "$STATE/iptables" ;;
esac
IPTSTUB
chmod +x "$tmp/bin/iptables"
# the pod routing items (nuit-cloud/nuit-cloud#197) in place: the three policy rules
routing_items() { grep -c 'iif ocisaas0' "$STATE/ip-rules" || true; }
# what the agent had for the old management mark (before nuit-cloud/nuit-cloud#205) in place: rule 1030 and the two mangle rules
mgmt_items() { echo $(( $(grep -c 'fwmark 0x31' "$STATE/ip-rules" || true) + $(grep -c '0x31' "$STATE/iptables" 2>/dev/null || true) )); }
pod_rules() { grep -c 'iif ocisaas0' "$STATE/ip-rules" || true; }
# pod_persist <management interface>: the items as a hand-made VLAN file would carry them; pod_live <management interface>: in place
pod_persist() {
  printf '    post-up ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040 2>/dev/null || true\n'
  printf '    post-up ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041 2>/dev/null || true\n'
  printf '    post-up ip rule add iif ocisaas0 lookup 105 priority 1045 2>/dev/null || true\n'
}
pod_live() {
  printf '1040:\tfrom all to 10.89.0.0/16 iif ocisaas0 [detached] lookup main\n1041:\tfrom all to 10.20.0.0/24 iif ocisaas0 [detached] lookup main\n1045:\tfrom all iif ocisaas0 [detached] lookup 105\n' >>"$STATE/ip-rules"
}
# route_of <interface a connection arrived on | pod> [destination]: the routing table a packet coming from a pod on ocisaas0 is routed
# by: a reply to a connection that arrived on that interface (after DNAT to the pod), or, for "pod", traffic the pod started. With no
# destination it goes to an address outside every connected network; a destination given is one inside a connected network (the
# management VLAN's, the tenant VLAN's, the pods' own). A small model of the kernel over the stubs' state: the connection's CONNMARK
# (set-mark rules, on its first packet), the packet mark (restore-mark rules, on the packet from ocisaas0), then the policy rules by
# priority (local and default never hold such an address; a "to" rule matches only a destination inside its network; the main table
# with suppress_prefixlength 0 is passed over for an address outside every connected network, and used for one inside).
route_of() {
  awk -v arr="$1" -v dst="${2:-}" -v ipt="$STATE/iptables" '
    function num(a,  o) { split(a, o, "."); return ((o[1] * 256 + o[2]) * 256 + o[3]) * 256 + o[4] }
    function inside(a, net,  b) { b = 2 ^ (32 - substr(net, index(net, "/") + 1)); return int(num(a) / b) == int(num(substr(net, 1, index(net, "/") - 1)) / b) }
    FILENAME == ipt { T[++nt] = $0; next }
    { l = $0; sub(/ \[detached\]/, "", l); pr = substr(l, 1, index(l, ":") - 1) + 0; R[pr] = substr(l, index(l, ":") + 1); P[++np] = pr }
    END {
      cm = ""; fw = ""
      for (i = 1; i <= nt; i++) { n = split(T[i], w, " "); neg = (w[1] == "!"); ifc = w[neg + 2]
        if (T[i] ~ /--set-mark/ && arr != "pod" && ((!neg && ifc == arr) || (neg && ifc != arr))) cm = w[n] }
      for (i = 1; i <= nt; i++) { n = split(T[i], w, " "); neg = (w[1] == "!"); ifc = w[neg + 2]; m = ""
        for (j = 1; j <= n; j++) if (w[j] == "--mark") m = w[j + 1]
        if (T[i] ~ /--restore-mark/ && cm != "" && m == cm && ((!neg && ifc == "ocisaas0") || (neg && ifc != "ocisaas0"))) fw = cm }
      for (i = 1; i <= np; i++) for (j = i + 1; j <= np; j++) if (P[j] < P[i]) { t = P[i]; P[i] = P[j]; P[j] = t }
      for (i = 1; i <= np; i++) { k = split(R[P[i]], w, " "); iif = ""; fm = ""; tb = ""; sup = 0; to = ""
        for (j = 1; j <= k; j++) { if (w[j] == "iif") iif = w[j + 1]; if (w[j] == "fwmark") fm = w[j + 1]; if (w[j] == "lookup") tb = w[j + 1]; if (w[j] == "suppress_prefixlength") sup = 1; if (w[j] == "to") to = w[j + 1] }
        if (iif != "" && iif != "ocisaas0") continue
        if (fm != "" && fm != fw) continue
        if (to != "" && (dst == "" || !inside(dst, to))) continue
        if (tb == "local" || tb == "default" || (tb == "main" && sup && dst == "")) continue
        print tb; exit }
      print "main"
    }' "$STATE/iptables" "$STATE/ip-rules"
}
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

# without a management VLAN nothing changes: the same-subnet move never makes a VLAN interface or moves the gateway
mk_net 10.10.10.170
addr_run 10.10.10.171 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "same-subnet move"; }
grep -q 'done    ncs_address' "$tmp/out" && ! grep -q 'ip link' "$CALLS" && ! grep -q 'vlan-raw-device' "$ROOT/etc/network/interfaces.d/nuit-cloud-address" && [ "$(cat "$STATE/ip-route")" = "default via 10.10.10.1 dev eth0" ] \
  && ok "ncs_address: without a management VLAN the move stays within the subnet, with no VLAN interface and the same gateway" || { cat "$tmp/out"; cat "$CALLS"; bad "address same subnet unchanged"; }

# 9b. ncs_address onto a tagged VLAN (nuit-cloud/nuit-cloud#197): the address moves onto eth0.31 with the default route; the old one
#     goes only after the proof; a failed proof puts everything back
mvlan_run() { env NCS_MANAGEMENT_ADDRESS="${MADDR-10.31.0.18}" NCS_MANAGEMENT_VLAN="${MVLAN-31}" NCS_MANAGEMENT_PREFIX="${MPFX-24}" NCS_MANAGEMENT_GATEWAY="${MGW-10.31.0.1}" NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-address ${1:+"$1"}; }
sf="$ROOT/etc/network/interfaces.d/nuit-cloud-address"
mk_net 10.10.10.170
mvlan_run --check >"$tmp/out" 2>&1 && bad "a machine still on the old network passed the VLAN check"
grep -q 'DRIFT   ncs_address' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): a machine still on the old network is drift, and --check changes nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan check before"; }
mvlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan apply"; }
line_of() { grep -n "$1" "$CALLS" | head -1 | cut -d: -f1; }
grep -q 'done    ncs_address' "$tmp/out" && grep -q 'systemd-run.*--unit=nuit-cloud-address-vlan-rollback' "$CALLS" && ! grep -q 'nuit-cloud-vlan-rollback' "$CALLS" \
  && grep -q 'ip link add link eth0 name eth0.31 type vlan id 31' "$CALLS" && grep -q 'ip link set dev eth0.31 up' "$CALLS" && grep -q 'arping .*-I eth0.31 10.31.0.18' "$CALLS" \
  && grep -q 'ip addr add 10.31.0.18/24 dev eth0.31' "$CALLS" && grep -q 'ip route replace default via 10.31.0.1 dev eth0.31' "$CALLS" && grep -q 'ping .*-I eth0.31 10.31.0.1' "$CALLS" \
  && grep -q 'curl .*--interface 10.31.0.18 .*agent.example/api/agent/download/VERSION' "$CALLS" && grep -q 'ip addr del 10.10.10.170/24 dev eth0' "$CALLS" \
  && [ "$(line_of '^systemd-run')" -lt "$(line_of 'ip link add')" ] && [ "$(line_of 'ip addr add 10.31.0.18')" -lt "$(line_of 'ip route replace default')" ] \
  && [ "$(line_of '^curl ')" -lt "$(line_of 'ip addr del 10.10.10.170')" ] && [ "$(line_of '^ping ')" -lt "$(line_of 'ip addr del 10.10.10.170')" ] \
  && grep -q 'systemctl stop nuit-cloud-address-vlan-rollback.timer' "$CALLS" && [ ! -e "$ROOT/var/lib/nuit-cloud-agent/address/management-vlan/armed" ] && [ -f "$ROOT/var/lib/nuit-cloud-agent/address/management-vlan/backup/files" ] \
  && ok "ncs_address (VLAN): timer first, then eth0.31 with the address beside the old one, the default route moved, proved by the gateway and the platform, the old address removed last" || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan apply calls"; }
grep -qx 'eth0.31 10.31.0.18/24' "$STATE/ip-addrs" && ! grep -q '^eth0 ' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.2/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.3/24' "$STATE/ip-addrs" \
  && [ "$(cat "$STATE/ip-route")" = "default via 10.31.0.1 dev eth0.31" ] && ! grep -q 'table 105\|fwmark' "$CALLS" && [ ! -e "$ROOT/etc/network/interfaces.d/nuit-cloud-vlan31" ] \
  && ok "ncs_address (VLAN): the machine is left with the address on eth0.31, the default route by 10.31.0.1, and the tenant VLAN eth0.5 as it was" || { cat "$STATE/ip-addrs"; cat "$STATE/ip-route"; bad "mvlan apply state"; }
grep -q '^# nuit-cloud: iface eth0 inet dhcp$' "$ROOT/etc/network/interfaces.d/eth0" && grep -q '^auto eth0$' "$ROOT/etc/network/interfaces.d/eth0" && grep -q '^iface eth1 inet static$' "$ROOT/etc/network/interfaces.d/eth0" \
  && grep -qx 'iface eth0 inet manual' "$sf" && ! grep -qx 'auto eth0' "$sf" && grep -qx 'auto eth0.31' "$sf" && grep -qx 'iface eth0.31 inet static' "$sf" && grep -qx '    address 10.31.0.18/24' "$sf" && grep -qx '    gateway 10.31.0.1' "$sf" \
  && grep -qx '    vlan-raw-device eth0' "$sf" && grep -q 'pre-up ip link show eth0.31 >/dev/null 2>&1 || ip link add link eth0 name eth0.31 type vlan id 31' "$sf" && grep -qx '8021q' "$ROOT/etc/modules-load.d/8021q.conf" \
  && ok "ncs_address (VLAN): eth0's DHCP stanza is commented out, eth0 stays up with no address, and eth0.31 is written with the address, gateway and its link" || { cat "$sf"; cat "$ROOT/etc/network/interfaces.d/eth0"; bad "mvlan files"; }
: >"$CALLS"; mvlan_run >"$tmp/out" 2>&1 && mvlan_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): once moved, a second run is a no-op and --check is clean" || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan idempotent"; }
(MGW=10.31.0.254; mvlan_run --check >/dev/null 2>&1) && bad "a default route by another gateway passed the check" || ok "ncs_address (VLAN): a default route by another gateway is drift"
printf 'default via 10.10.10.1 dev eth0\n' >"$STATE/ip-route"
mvlan_run --check >/dev/null 2>&1 && bad "the default route still on the old network passed the check" || ok "ncs_address (VLAN): the address on eth0.31 with the default route still on eth0 is drift"
printf 'default via 10.31.0.1 dev eth0.31\n' >"$STATE/ip-route"
# a machine that holds the tenant VLAN on eth0.5 keeps hanging it off eth0
vlan_check() { env NCS_VLAN_ID=5 NCS_VLAN_PREFIX=24 NCS_VLAN_ADDRESS=10.20.0.2 NCS_VLAN_GATEWAY=10.20.0.1 NCS_VLAN_TENANT_ADDRESSES=10.20.0.3 sh "$here/bootstrap.sh" nuit-cloud-vlan --check; }
cat >"$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5" <<HAND
auto eth0.5
iface eth0.5 inet static
    address 10.20.0.2/24
    post-up ip addr add 10.20.0.3/24 dev eth0.5 2>/dev/null || true
$(pod_persist eth0.31)
HAND
pod_live eth0.31
vlan_check >"$tmp/out" 2>&1 && ok "ncs_vlan: with the default route on eth0.31 the tenant VLAN is still eth0.5 (its parent is eth0)" || { cat "$tmp/out"; bad "vlan check after the management move"; }

# the platform cannot be reached from the new address: the files, the old address, the old route and DHCP come back; eth0.31 goes
mk_net 10.10.10.170; cp "$ROOT/etc/network/interfaces.d/eth0" "$tmp/eth0.before"; touch "$STATE/curl-fail"
mvlan_run >"$tmp/out" 2>&1 && bad "an unproved VLAN move was kept"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'platform could not be reached from 10.31.0.18 on eth0.31, so the old configuration was put back' "$tmp/out" \
  && cmp -s "$tmp/eth0.before" "$ROOT/etc/network/interfaces.d/eth0" && [ ! -e "$sf" ] && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && ! grep -q '^eth0.31 ' "$STATE/ip-addrs" && ! grep -qx 'eth0.31' "$STATE/ip-links" \
  && grep -qx 'eth0.5 10.20.0.3/24' "$STATE/ip-addrs" && [ "$(cat "$STATE/ip-route")" = "default via 10.10.10.1 dev eth0" ] && grep -q 'ip link del dev eth0.31' "$CALLS" && grep -q 'ifup --force eth0' "$CALLS" \
  && ! grep -q 'ip addr del 10.10.10.170' "$CALLS" && grep -q 'systemctl stop nuit-cloud-address-vlan-rollback.timer' "$CALLS" \
  && ok "ncs_address (VLAN): a failed proof restores the files, removes eth0.31, keeps the old address, puts the default route back and restarts DHCP" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs"; cat "$STATE/ip-route"; bad "mvlan rollback"; }
# the new gateway does not answer on the VLAN: the same, and the platform is not even asked
mk_net 10.10.10.170; touch "$STATE/gw-silent"
mvlan_run >"$tmp/out" 2>&1 && bad "a VLAN move whose gateway is silent was kept"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'gateway 10.31.0.1 did not answer on eth0.31' "$tmp/out" && ! grep -q '^curl ' "$CALLS" && [ ! -e "$sf" ] && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" \
  && ! grep -q '^eth0.31 ' "$STATE/ip-addrs" && [ "$(cat "$STATE/ip-route")" = "default via 10.10.10.1 dev eth0" ] \
  && ok "ncs_address (VLAN): a gateway that does not answer on the VLAN undoes the move" || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan silent gateway"; }
# a static address applied before (the same-subnet move) is what comes back
mk_net 10.10.10.170; addr_run 10.10.10.170 >/dev/null 2>&1 || bad "same-subnet apply before the VLAN move"
cp "$sf" "$tmp/static.before"; cp "$ROOT/etc/network/interfaces.d/eth0" "$tmp/eth0.before"; touch "$STATE/curl-fail"; : >"$CALLS"
mvlan_run >"$tmp/out" 2>&1 && bad "an unproved VLAN move over a static address was kept"
cmp -s "$tmp/static.before" "$sf" && cmp -s "$tmp/eth0.before" "$ROOT/etc/network/interfaces.d/eth0" && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" \
  && ok "ncs_address (VLAN): a failed move over an earlier static address puts that static file back as it was" || { cat "$tmp/out"; cat "$sf"; bad "mvlan rollback static"; }
# the move loses the tenant VLAN's addresses: they are put back and the move stands
mk_net 10.10.10.170; touch "$STATE/lose-vlan"
mvlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "mvlan heal"; }
grep -q 'done    ncs_address' "$tmp/out" && grep -qx 'eth0.5 10.20.0.2/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.3/24' "$STATE/ip-addrs" && grep -qx 'eth0.31 10.31.0.18/24' "$STATE/ip-addrs" \
  && ok "ncs_address (VLAN): tenant VLAN addresses lost by the move are put back, and the move stands" || { cat "$tmp/out"; cat "$STATE/ip-addrs"; bad "mvlan heal result"; }
# another device answers for the address on the VLAN: refused, and the VLAN interface made for the check is taken away again
mk_net 10.10.10.170; touch "$STATE/arping-dupe"
mvlan_run >"$tmp/out" 2>&1 && bad "a duplicate address on the VLAN was taken"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'another device already answers for 10.31.0.18 on VLAN 31' "$tmp/out" && ! grep -q 'ip addr add 10.31.0.18' "$CALLS" && ! grep -q 'route replace default via 10.31' "$CALLS" \
  && ! grep -qx 'eth0.31' "$STATE/ip-links" && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && [ ! -e "$sf" ] && ok "ncs_address (VLAN): an address another device answers for on the VLAN is refused and nothing is kept" || { cat "$tmp/out"; cat "$CALLS"; bad "mvlan duplicate"; }
# what a person must decide is left alone, and hostile or inconsistent values change nothing
mk_net 10.10.10.170; : >"$ROOT/etc/network/interfaces.d/nuit-cloud-vlan31"
mvlan_run >"$tmp/out" 2>&1 && bad "the tenant VLAN was taken for the management address"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q "tenant VLAN" "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): the tenant VLAN's own id is refused" || { cat "$tmp/out"; bad "mvlan tenant id"; }
mk_net 10.10.10.170; printf 'default via 10.40.0.1 dev eth0.40\n' >"$STATE/ip-route"; printf 'eth0.40 10.40.0.7/24\n' >>"$STATE/ip-addrs"
mvlan_run >"$tmp/out" 2>&1 && bad "a move from one VLAN to another was made"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q 'from one VLAN to another' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): a move from one VLAN to another is left to a person" || { cat "$tmp/out"; bad "mvlan vlan to vlan"; }
mk_net 10.10.10.170
(MADDR=10.10.10.200 MGW=10.10.10.1; mvlan_run) >"$tmp/out" 2>&1 && bad "a VLAN overlapping the current network was accepted"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q 'overlaps' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): a VLAN subnet that holds the current address is refused" || { cat "$tmp/out"; bad "mvlan overlap"; }
for hostile in "MVLAN=0" "MVLAN=4095" "MVLAN=31;reboot" "MVLAN=031" "MPFX=8" "MPFX=31" "MPFX=24x" "MGW=10.10.10.1" "MGW=10.31.0.18" "MGW=10.31.0.1;reboot" "MGW=" "MADDR=10.31.0.999"; do
  mk_net 10.10.10.170
  (export "$hostile"; mvlan_run) >"$tmp/out" 2>&1 && bad "hostile $hostile accepted"
  [ ! -s "$CALLS" ] && [ ! -e "$sf" ] && grep -q 'MANUAL  ncs_address\|FAILED  ncs_address' "$tmp/out" || { cat "$tmp/out"; cat "$CALLS"; bad "hostile $hostile changed something"; }
  (export "$hostile"; mvlan_run --check) >/dev/null 2>&1 && bad "hostile $hostile passed the check"
done; ok "ncs_address (VLAN): a bad VLAN id, prefix, gateway or address changes nothing and never passes the check"

# 9c. a parent whose name is already 15 characters, the kernel's limit (a Raspberry Pi's enx<mac>; cps-pi-hnu7, nuit-cloud/nuit-cloud#197):
#     <parent>.30 would not fit, so the VLAN interface is vlan30, tied to its parent by vlan-raw-device; a parent with room keeps <parent>.<id>
PI=enxb827ebdbcb96
ifname() { sh -c '. "$1"; vlan_ifname "$2" "$3"' ifname "$here/features.sh" "$1" "$2"; }
[ "$(ifname eth0 31)" = eth0.31 ] && [ "$(ifname enp0s31f6 5)" = enp0s31f6.5 ] && [ "$(ifname enp0s31f6 4094)" = enp0s31f6.4094 ] && [ "$(ifname enxb827ebdbcb 5)" = enxb827ebdbcb.5 ] \
  && [ "$(ifname enxb827ebdbcb 31)" = vlan31 ] && [ "$(ifname "$PI" 30)" = vlan30 ] && [ "$(ifname "$PI" 5)" = vlan5 ] && [ "$(ifname 'a;b' 5)" = vlan5 ] \
  && ok "VLAN names: <parent>.<id> whenever it fits in 15 characters (eth0.31, enxb827ebdbcb.5), vlan<id> when it does not (vlan31, vlan30)" || bad "vlan_ifname"
pi_net() { (NETIF=$PI; TVIF=vlan5; mk_net "$1"); }
pi_run() { (MADDR=10.30.0.11; MVLAN=30; MGW=10.30.0.1; mvlan_run ${1:+"$1"}); }
pi_net 10.10.10.170
pi_run --check >"$tmp/out" 2>&1 && bad "a Pi still on the old network passed the VLAN check"
pi_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "the management VLAN move on a 15-character parent"; }
grep -q 'done    ncs_address' "$tmp/out" && grep -q "ip link add link $PI name vlan30 type vlan id 30" "$CALLS" && grep -q 'ip addr add 10.30.0.11/24 dev vlan30' "$CALLS" \
  && grep -q 'ip route replace default via 10.30.0.1 dev vlan30' "$CALLS" && grep -q 'ping .*-I vlan30 10.30.0.1' "$CALLS" && grep -q "ip addr del 10.10.10.170/24 dev $PI" "$CALLS" && ! grep -q "$PI\\.30" "$CALLS" \
  && [ "$(cat "$STATE/ip-route")" = "default via 10.30.0.1 dev vlan30" ] && grep -qx 'vlan30 10.30.0.11/24' "$STATE/ip-addrs" && grep -qx 'vlan5 10.20.0.3/24' "$STATE/ip-addrs" \
  && grep -qx "iface $PI inet manual" "$sf" && grep -qx 'auto vlan30' "$sf" && grep -qx 'iface vlan30 inet static' "$sf" && grep -qx '    address 10.30.0.11/24' "$sf" && grep -qx "    vlan-raw-device $PI" "$sf" \
  && grep -q "pre-up ip link show vlan30 >/dev/null 2>&1 || ip link add link $PI name vlan30 type vlan id 30" "$sf" && grep -q 'post-down ip link del vlan30' "$sf" && ! grep -q "$PI\\.30" "$sf" \
  && grep -q "^# nuit-cloud: iface $PI inet dhcp\$" "$ROOT/etc/network/interfaces.d/$PI" \
  && ok "ncs_address (VLAN): on a 15-character parent the address moves onto vlan30 (vlan-raw-device $PI) with the default route, not onto an impossible $PI.30" || { cat "$tmp/out"; cat "$CALLS"; cat "$sf"; bad "pi mvlan apply"; }
: >"$CALLS"; pi_run >"$tmp/out" 2>&1 && pi_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): a machine moved onto vlan30 is recognised as done: a second run is a no-op and --check is clean" || { cat "$tmp/out"; cat "$CALLS"; bad "pi mvlan idempotent"; }
mv "$ROOT/proc/net/vlan/config" "$tmp/vlan-config"
pi_run --check >/dev/null 2>&1 && ok "ncs_address (VLAN): without the kernel's VLAN list, vlan30's parent is read from its vlan-raw-device line and the check still passes" || bad "pi mvlan check from ifupdown"
mv "$tmp/vlan-config" "$ROOT/proc/net/vlan/config"
lower() { sh -c 'p() { printf "%s%s\n" "$ROOT" "$1"; }; . "$1"; addr_lower "$2"' lower "$here/features.sh" "$1"; }
wifidev() { sh -c 'p() { printf "%s%s\n" "$ROOT" "$1"; }; . "$1"; wifi_default_dev' wifidev "$here/features.sh"; }
[ "$(lower vlan30)" = "$PI" ] && [ "$(lower vlan5)" = "$PI" ] && [ "$(lower "$PI")" = "$PI" ] && [ "$(lower eth0.31)" = eth0 ] && [ "$(lower vlan99)" = vlan99 ] && [ "$(wifidev)" = "$PI" ] \
  && ok "the parent behind vlan30 and vlan5 is the physical $PI (from the kernel), and the wireless check sees the default route's parent" || { lower vlan30; wifidev; bad "addr_lower behind vlan<id>"; }
# the move cannot be proved: everything is put back, and it is vlan30 that is removed
pi_net 10.10.10.170; cp "$ROOT/etc/network/interfaces.d/$PI" "$tmp/pi.before"; touch "$STATE/curl-fail"
pi_run >"$tmp/out" 2>&1 && bad "an unproved move onto vlan30 was kept"
grep -q 'FAILED  ncs_address' "$tmp/out" && grep -q 'ip link del dev vlan30' "$CALLS" && ! grep -qx 'vlan30' "$STATE/ip-links" && ! grep -q '^vlan30 ' "$STATE/ip-addrs" && ! grep -q '^vlan30 ' "$ROOT/proc/net/vlan/config" \
  && [ "$(cat "$STATE/ip-route")" = "default via 10.10.10.1 dev $PI" ] && grep -qx "$PI 10.10.10.170/24" "$STATE/ip-addrs" && grep -qx 'vlan5 10.20.0.3/24' "$STATE/ip-addrs" \
  && cmp -s "$tmp/pi.before" "$ROOT/etc/network/interfaces.d/$PI" && [ ! -e "$sf" ] && grep -q "ifup --force $PI" "$CALLS" \
  && ok "ncs_address (VLAN): a failed move on a 15-character parent removes vlan30 and puts the address, the route and the files back" || { cat "$tmp/out"; cat "$CALLS"; bad "pi mvlan rollback"; }
# a vlan30 that is someone else's (another parent, or not a VLAN at all) is left alone
pi_net 10.10.10.170; echo vlan30 >>"$STATE/ip-links"; echo "vlan30 | 30 | eth1" >>"$ROOT/proc/net/vlan/config"
pi_run >"$tmp/out" 2>&1 && bad "a vlan30 on another parent was taken"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q "vlan30 exists already and is not VLAN 30 on $PI" "$tmp/out" && [ ! -s "$CALLS" ] || { cat "$tmp/out"; cat "$CALLS"; bad "pi vlan30 on another parent"; }
pi_net 10.10.10.170; echo vlan30 >>"$STATE/ip-links"
pi_run >"$tmp/out" 2>&1 && bad "a vlan30 that is not a VLAN was taken"
grep -q 'MANUAL  ncs_address' "$tmp/out" && grep -q 'vlan30 exists already' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_address (VLAN): a vlan30 on another parent, or one that is not a VLAN, is left to a person and nothing changes" || { cat "$tmp/out"; cat "$CALLS"; bad "pi vlan30 not a VLAN"; }

# 10. ncs_vlan: the tenant VLAN slice goes on the machine as a sub-interface, additive and undone if the platform is lost
# ifup brings the interface up as its file says: the address line, then each tenant line, table 105 by the gateway, the 0x5 marks
cat >"$tmp/bin/ifup" <<'IFUP'
#!/bin/sh
echo "ifup $*" >>"$CALLS"; [ -e "$STATE/ifup-fail" ] && { echo "ifup: boom" >&2; exit 1; }
case "$1" in -*) exit 0 ;; esac
f="$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5"
grep -q 'promote_secondaries=1' "$f" && { mkdir -p "$ROOT/proc/sys/net/ipv4/conf/$1"; echo 1 >"$ROOT/proc/sys/net/ipv4/conf/$1/promote_secondaries"; }
for c in $(awk '$1 == "address" {print $2}' "$f") $(grep -v '^[[:space:]]*#' "$f" | sed -n 's/.*ip addr add \([0-9.]*\/[0-9]*\) .*/\1/p'); do netmodel add "$c" "$1" 2>/dev/null; done
sed -n 's/.*post-up ip route replace \(default via [0-9.]* dev [^ ]*\) table 105.*/\1/p' "$f" >"$STATE/ip-t105"
printf "%s\n" "-i $1 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5" "! -i $1 -m connmark --mark 0x5 -j CONNMARK --restore-mark" >>"$STATE/iptables"
IFUP
chmod +x "$tmp/bin/ifup"
stub ifdown 'echo "ifdown $*" >>"$CALLS"; grep -v "^$2 " "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"'
stub ping 'echo "ping $*" >>"$CALLS"; [ ! -e "$STATE/gw-silent" ]'
stub ifquery '[ ! -e "$STATE/no-ifstate" ]'
vlan_run() { env NCS_VLAN_ID=5 NCS_VLAN_PREFIX=24 NCS_VLAN_ADDRESS=10.20.0.18 NCS_VLAN_GATEWAY=10.20.0.1 NCS_VLAN_TENANT_ADDRESSES="${TENANTS-10.20.0.19 10.20.0.20 10.20.0.21}" NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-vlan ${1:+"$1"}; }
vlan_net() {
  mk_net 10.10.10.170; _tv="${TVIF:-${NETIF:-eth0}.5}"; sed -i.bak "/^$_tv /d" "$STATE/ip-addrs"; sed -i.bak "/ dev $_tv /d" "$STATE/ip-routes"
  rm -f "$STATE/ifup-fail" "$STATE/gw-silent" "$STATE/no-ifstate"; : >"$CALLS"; rm -f "$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5" "$ROOT/etc/modules-load.d/8021q.conf"
  touch "$STATE/model"; : >"$STATE/ip-t105"; mkdir -p "$ROOT/proc/sys/net/ipv4/conf/$_tv"; echo 0 >"$ROOT/proc/sys/net/ipv4/conf/$_tv/promote_secondaries"
}

vlan_net
vlan_run --check >"$tmp/out" 2>&1 && bad "a machine without the VLAN passed the check"
grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: --check reports a missing VLAN and changes nothing" || { cat "$tmp/out"; bad "vlan check"; }
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "vlan apply"; }
f="$ROOT/etc/network/interfaces.d/nuit-cloud-vlan5"
grep -q 'done    ncs_vlan' "$tmp/out" && grep -q '^iface eth0.5 inet static$' "$f" && grep -q '^    address 10.20.0.18/24$' "$f" && grep -q 'post-up ip addr add 10.20.0.20/24 dev eth0.5' "$f" && grep -q 'ip link add link eth0 name eth0.5 type vlan id 5' "$f" \
  && grep -q 'ip rule add fwmark 0x5 lookup 105' "$f" && grep -q 'ip route replace default via 10.20.0.1 dev eth0.5 table 105' "$f" && grep -qx '8021q' "$ROOT/etc/modules-load.d/8021q.conf" \
  && grep -q 'systemd-run.*nuit-cloud-vlan-rollback' "$CALLS" && grep -q 'ifup eth0.5' "$CALLS" && grep -q 'systemctl stop nuit-cloud-vlan-rollback.timer' "$CALLS" && grep -qx 'eth0 10.10.10.170/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.19/24' "$STATE/ip-addrs" \
  && grep -q 'VLAN gateway 10.20.0.1 answers' "$tmp/out" && ok "ncs_vlan: the sub-interface is written like server-vlan5.sh, brought up, the machine's own address untouched, the timer cancelled" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "vlan apply result"; }
# pod routing (nuit-cloud/nuit-cloud#197): pods reach their own range and the tenant VLAN directly (1040, 1041) and nothing else of the
# main table; everything else they start goes by the tenant VLAN (1045); replies to connections that arrived on the tenant VLAN go back
# by the tenant VLAN (mark 0x5); in the file for boot, and in place now even though ocisaas0 does not exist yet (the runtime may come
# later). Nothing for connections that arrived on the management interface: none reaches a pod since nuit-cloud/nuit-cloud#205.
grep -qxF "    post-up ip -4 rule show | grep -Ewq 'to 10\.89\.0\.0/16 iif ocisaas0 ([[]detached[]] )?lookup main' || ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040" "$f" \
  && grep -qxF "    post-up ip -4 rule show | grep -Ewq 'to 10\.20\.0\.0/24 iif ocisaas0 ([[]detached[]] )?lookup main' || ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041" "$f" \
  && grep -qxF "    post-up ip -4 rule show | grep -Ewq 'iif ocisaas0 ([[]detached[]] )?lookup 105' || ip rule add iif ocisaas0 lookup 105 priority 1045" "$f" \
  && grep -qx '    pre-down ip rule del iif ocisaas0 lookup 105 2>/dev/null || true' "$f" && grep -qx '    pre-down ip rule del to 10.89.0.0/16 iif ocisaas0 lookup main 2>/dev/null || true' "$f" \
  && grep -qx '    pre-down ip rule del to 10.20.0.0/24 iif ocisaas0 lookup main 2>/dev/null || true' "$f" && ! grep -q suppress_prefixlength "$f" \
  && grep -qxF "    post-up iptables -t mangle -C PREROUTING -i eth0.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5 2>/dev/null || iptables -t mangle -A PREROUTING -i eth0.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5" "$f" \
  && grep -qxF "    post-up iptables -t mangle -C PREROUTING ! -i eth0.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || iptables -t mangle -A PREROUTING ! -i eth0.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark" "$f" \
  && grep -qxF '    post-up ip rule show | grep -q "fwmark 0x5 lookup 105" || ip rule add fwmark 0x5 lookup 105 priority 1050' "$f" \
  && ! grep -q '0x31\|1030' "$f" \
  && grep -qxF "$(printf '1040:\tfrom all to 10.89.0.0/16 iif ocisaas0 [detached] lookup main')" "$STATE/ip-rules" \
  && grep -qxF "$(printf '1041:\tfrom all to 10.20.0.0/24 iif ocisaas0 [detached] lookup main')" "$STATE/ip-rules" && ! grep -q suppress_prefixlength "$STATE/ip-rules" && grep -qxF "$(printf '1045:\tfrom all iif ocisaas0 [detached] lookup 105')" "$STATE/ip-rules" \
  && [ "$(routing_items)" = 3 ] && [ "$(mgmt_items)" = 0 ] && ! grep -q '^1030:' "$STATE/ip-rules" \
  && [ "$(cat "$STATE/iptables")" = "$(printf '%s\n' '-i eth0.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5' '! -i eth0.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark')" ] \
  && ok "ncs_vlan: the pod routing rules (1040 to the pods' range, 1041 to the tenant VLAN, 1045) are in the file and in place, before the bridge exists; nothing with the old management mark 0x31 (no rule 1030, no mangle rule), the 0x5 marks as before" || { cat "$f"; cat "$STATE/ip-rules" "$STATE/iptables"; bad "vlan pod rules"; }
[ "$(route_of eth0.5)" = 105 ] && ok "ncs_vlan: a reply from a pod to a connection that arrived on the tenant VLAN still goes by table 105 (mark 0x5)" || bad "tenant VLAN reply by $(route_of eth0.5)"
[ "$(route_of pod)" = 105 ] && ok "ncs_vlan: traffic a pod starts goes by table 105, the tenant VLAN gateway" || bad "pod traffic by $(route_of pod)"
[ "$(route_of pod 10.89.0.9)" = main ] && [ "$(route_of pod 10.20.0.50)" = main ] && ok "ncs_vlan: pods reach their own range and the tenant VLAN directly, by the main table" || bad "pod to its own range by $(route_of pod 10.89.0.9), to the tenant VLAN by $(route_of pod 10.20.0.50)"
[ "$(route_of pod 10.10.10.11)" = 105 ] && ok "ncs_vlan: a pod never reaches the management network directly (another machine's management address goes by the tenant VLAN gateway)" || bad "a pod reaches the management network by $(route_of pod 10.10.10.11)"
: >"$CALLS"; vlan_run >"$tmp/out" 2>&1 && vlan_run --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && [ "$(routing_items)" = 3 ] && ok "ncs_vlan: a second run is a no-op and --check is clean" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan idempotent"; }
# a machine in service (#197): the VLAN is up with its tenant addresses, and the file and the kernel lack the routing items (an older agent)
cp "$f" "$tmp/vlan5.full"; cp "$STATE/ip-rules" "$tmp/rules.full"; cp "$STATE/iptables" "$tmp/ipt.full"
old_agent() { cp "$tmp/vlan5.full" "$f"; sed -i.bak '/iif ocisaas0/d;/0x31/d' "$f" "$STATE/ip-rules" "$STATE/iptables"; : >"$CALLS"; rm -f "$STATE/curl-fail"; }
old_agent
[ "$(route_of pod)" = main ] && [ "$(route_of eth0.5)" = 105 ] || bad "the older agent's routing is not what #197 found"
vlan_run --check >"$tmp/out" 2>&1 && bad "a VLAN without the pod routing passed the check"
grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: a VLAN without the pod routing (an older agent: pods leave by the management network) is drift, and the check changes nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan check pod rules"; }
for item in 'lookup 105 priority 1045' 'lookup main priority 1040' 'lookup main priority 1041'; do
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; sed -i.bak "/$item/d" "$f"
  vlan_run --check >/dev/null 2>&1 && bad "the item '$item' missing from the file passed the check"
done
for item in 'lookup 105$' 'to 10.89.0.0' 'to 10.20.0.0'; do
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; sed -i.bak "/$item/d" "$STATE/ip-rules"
  vlan_run --check >/dev/null 2>&1 && bad "the rule '$item' missing from the kernel passed the check"
done
ok "ncs_vlan: any routing item missing, in place or in the file, is drift"
old_agent; before_addrs="$(sort "$STATE/ip-addrs")"
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "vlan apply on an up VLAN"; }
grep -q 'done    ncs_vlan' "$tmp/out" && ! grep -q '^ifup\|^ifdown\|ip link' "$CALLS" && [ "$(sort "$STATE/ip-addrs")" = "$before_addrs" ] \
  && grep -qx 'ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040' "$CALLS" && grep -qx 'ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041' "$CALLS" \
  && grep -qx 'ip rule add iif ocisaas0 lookup 105 priority 1045' "$CALLS" \
  && ! grep -q 'iptables' "$CALLS" \
  && cmp -s "$f" "$tmp/vlan5.full" && grep -q 'systemctl stop nuit-cloud-vlan-rollback.timer' "$CALLS" && grep -q 'eth0.5 was up already and was left up; added as it runs: 3 pod routing item' "$tmp/out" && vlan_run --check >/dev/null 2>&1 \
  && [ "$(route_of eth0.5)" = 105 ] && [ "$(route_of pod)" = 105 ] \
  && ok "ncs_vlan: on a VLAN that is up, the routing items are added as it runs and the file rewritten, with no ifdown or ifup and every address kept" || { cat "$tmp/out"; cat "$CALLS"; diff "$tmp/vlan5.full" "$f"; bad "vlan live pod rules"; }
: >"$CALLS"; vlan_run >/dev/null 2>&1 && [ ! -s "$CALLS" ] && [ "$(routing_items)" = 3 ] && ok "ncs_vlan: run again, nothing is added twice" || { cat "$CALLS"; cat "$STATE/ip-rules" "$STATE/iptables"; bad "vlan live idempotent"; }
# the rules there already under other priorities (made by hand) are found by what they select, not added again
old_agent; printf '900:\tfrom all to 10.89.0.0/16 iif ocisaas0 lookup main\n901:\tfrom all to 10.20.0.0/24 iif ocisaas0 lookup main\n1200:\tfrom all iif ocisaas0 lookup 105\n' >>"$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
vlan_run >"$tmp/out" 2>&1 && ! grep -q 'ip rule add\|iptables -t mangle -A' "$CALLS" && [ "$(routing_items)" = 3 ] && grep -q 'added as it runs: 0 pod routing item' "$tmp/out" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: routing rules there already under other priorities are not doubled" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules"; bad "vlan pod rules other priorities"; }
# the change cannot be proved: the items it added go, the file comes back, and the VLAN stays up as it was
old_agent; cp "$f" "$tmp/vlan5.old"; touch "$STATE/curl-fail"
vlan_run >"$tmp/out" 2>&1 && bad "an unproved pod routing change was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && [ "$(routing_items)" = 0 ] && grep -qx 'ip rule del iif ocisaas0 lookup 105 priority 1045' "$CALLS" && cmp -s "$f" "$tmp/vlan5.old" \
  && ! grep -q '^ifup\|^ifdown\|ip link del' "$CALLS" && [ "$(sort "$STATE/ip-addrs")" = "$before_addrs" ] && grep -q 'fwmark 0x5 lookup 105' "$STATE/ip-rules" \
  && ok "ncs_vlan: on a VLAN that is up, a rollback removes only the items it added and leaves the interface and its addresses up" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules"; bad "vlan live rollback"; }
# a rollback leaves alone what was there before it
old_agent; printf '1045:\tfrom all iif ocisaas0 lookup 105\n' >>"$STATE/ip-rules"; touch "$STATE/curl-fail"
vlan_run >/dev/null 2>&1 && bad "an unproved pod routing change was kept"
[ "$(routing_items)" = 1 ] && grep -q 'iif ocisaas0 lookup 105' "$STATE/ip-rules" && ok "ncs_vlan: a rollback keeps routing items that were there before" || { cat "$STATE/ip-rules" "$STATE/iptables"; bad "vlan rollback keeps old rule"; }
# a machine an agent applied before 22 Nov 2026 (nuit-cloud/nuit-cloud#197): rule 1040 was "iif ocisaas0 lookup main suppress_prefixlength 0",
# which gave pods every specific route of the main table, the management network's connected route among them
OLD_SEL='iif ocisaas0 lookup main suppress_prefixlength 0'
old_fix() {
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; rm -f "$STATE/curl-fail"
  awk -v o="$OLD_SEL" '/to 10\.20\.0\.0\/24 iif ocisaas0/ {next}
    /post-up .*to 10\.89\.0\.0\/16 iif ocisaas0/ {print "    post-up ip -4 rule show | grep -Ewq '\''iif ocisaas0 ([[]detached[]] )?lookup main suppress_prefixlength 0'\'' || ip rule add " o " priority 1040"; next}
    /pre-down .*to 10\.89\.0\.0\/16 iif ocisaas0/ {print "    pre-down ip rule del " o " 2>/dev/null || true"; next} {print}' "$f" >"$f.new"; mv "$f.new" "$f"
  sed -i.bak '/iif ocisaas0 .*lookup main/d' "$STATE/ip-rules"; printf '1040:\tfrom all %s\n' "$(echo "$OLD_SEL" | sed 's/iif ocisaas0 /iif ocisaas0 [detached] /')" >>"$STATE/ip-rules"
  : >"$CALLS"
}
old_fix; cp "$f" "$tmp/vlan5.oldfix"; cp "$STATE/ip-rules" "$tmp/rules.oldfix"
[ "$(route_of pod 10.10.10.11)" = main ] && [ "$(grep -c 'iif ocisaas0' "$f")" -ge 2 ] || { cat "$f" "$STATE/ip-rules"; bad "the pre-fix fixture is not what #197 found (a pod reaches the management network by $(route_of pod 10.10.10.11))"; }
vlan_run --check >"$tmp/out" 2>&1 && bad "a machine with the old suppress_prefixlength rule passed the check"
grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: the old rule (pods use every route of the main table) is drift, and the check changes nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan check old rule"; }
# only the kernel, or only the file, still has the old form: drift too
cp "$tmp/vlan5.full" "$f"; vlan_run --check >/dev/null 2>&1 && bad "the old rule in place (with the new file) passed the check"
cp "$tmp/rules.full" "$STATE/ip-rules"; printf '1040:\tfrom all %s\n' "$OLD_SEL" >>"$STATE/ip-rules"; vlan_run --check >/dev/null 2>&1 && bad "the old rule in place beside the new ones passed the check"
cp "$tmp/vlan5.oldfix" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; vlan_run --check >/dev/null 2>&1 && bad "the old rule in the file (the new ones in place) passed the check"
cp "$tmp/vlan5.full" "$f"; printf '    post-up ip rule add %s priority 1040 2>/dev/null || true\n' "$OLD_SEL" >>"$f"; vlan_run --check >/dev/null 2>&1 && bad "the old rule added to the file beside the new ones passed the check"
cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; printf '1041:\tfrom all to 10.20.0.0/16 iif ocisaas0 lookup main\n' >>"$STATE/ip-rules"; vlan_run --check >/dev/null 2>&1 && bad "a pod rule to another tenant VLAN network passed the check"
ok "ncs_vlan: the old rule in place or in the file, or a pod rule to a network that is not the tenant VLAN's, is drift"
# apply over it: the new rules first, then the old one goes; the file is rewritten; pods no longer reach the management network
old_fix; vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "vlan apply over the old rule"; }
add_p1="$(grep -nx 'ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040' "$CALLS" | cut -d: -f1)"
add_pv="$(grep -nx 'ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041' "$CALLS" | cut -d: -f1)"
del_old="$(grep -nx "ip rule del $OLD_SEL priority 1040" "$CALLS" | cut -d: -f1)"
grep -q 'done    ncs_vlan' "$tmp/out" && [ -n "$add_p1" ] && [ -n "$add_pv" ] && [ -n "$del_old" ] && [ "$add_p1" -lt "$del_old" ] && [ "$add_pv" -lt "$del_old" ] \
  && ! grep -q suppress_prefixlength "$STATE/ip-rules" && ! grep -v '^[[:space:]]*#' "$f" | grep -q suppress_prefixlength && cmp -s "$f" "$tmp/vlan5.full" \
  && grep -qxF "$(printf '1040:\tfrom all to 10.89.0.0/16 iif ocisaas0 [detached] lookup main')" "$STATE/ip-rules" && grep -qxF "$(printf '1041:\tfrom all to 10.20.0.0/24 iif ocisaas0 [detached] lookup main')" "$STATE/ip-rules" \
  && grep -q 'stale rules removed: 1 policy, 0 mangle' "$tmp/out" && ! grep -q '^ifup\|^ifdown\|ip link' "$CALLS" && vlan_run --check >/dev/null 2>&1 \
  && [ "$(route_of pod 10.10.10.11)" = 105 ] && [ "$(route_of pod 10.20.0.50)" = main ] && [ "$(route_of pod 10.89.0.9)" = main ] && [ "$(route_of pod)" = 105 ] \
  && ok "ncs_vlan: an apply over the old rule adds 1040 (pods' range) and 1041 (tenant VLAN) first, then removes the old rule, rewrites the file, and pods no longer reach the management network" \
  || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules"; diff "$tmp/vlan5.full" "$f"; bad "vlan apply over the old rule"; }
: >"$CALLS"; vlan_run >/dev/null 2>&1 && [ ! -s "$CALLS" ] && [ "$(routing_items)" = 3 ] && ok "ncs_vlan: run again after the migration, nothing changes" || { cat "$CALLS"; cat "$STATE/ip-rules"; bad "vlan migration idempotent"; }
# the old rule under another priority (made by hand) goes too
old_fix; sed -i.bak 's/^1040:\(.*suppress_prefixlength.*\)$/900:\1/' "$STATE/ip-rules"
vlan_run >"$tmp/out" 2>&1 && grep -qx "ip rule del $OLD_SEL priority 900" "$CALLS" && ! grep -q suppress_prefixlength "$STATE/ip-rules" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: the old rule under another priority is found by what it selects and removed" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules"; bad "vlan old rule other priority"; }
# the migration cannot be proved: the old rule comes back, the new ones go, the file is the old one; pods are never without a route
old_fix; touch "$STATE/curl-fail"
vlan_run >"$tmp/out" 2>&1 && bad "an unproved migration was kept"
readd="$(grep -nx "ip rule add $OLD_SEL priority 1040" "$CALLS" | cut -d: -f1)"; del_p1="$(grep -nx 'ip rule del to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040' "$CALLS" | cut -d: -f1)"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && cmp -s "$f" "$tmp/vlan5.oldfix" && [ -n "$readd" ] && [ -n "$del_p1" ] && [ "$readd" -lt "$del_p1" ] \
  && [ "$(grep -c "iif ocisaas0 .*lookup main suppress_prefixlength 0" "$STATE/ip-rules")" = 1 ] && ! grep -q 'to 10\.89\.0\.0/16\|to 10\.20\.0\.0/24' "$STATE/ip-rules" \
  && grep -q 'iif ocisaas0 .*lookup 105' "$STATE/ip-rules" && ! grep -q '^ifup\|^ifdown\|ip link' "$CALLS" \
  && ok "ncs_vlan: a migration that cannot be proved puts the old rule back before taking the new ones away, and the old file with it" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules"; diff "$tmp/vlan5.oldfix" "$f"; bad "vlan migration rollback"; }
rm -f "$STATE/curl-fail"
# a hostile rule from ocisaas0 to the main table that cannot be read as plain words is never passed to a command: drift, and the apply fails
cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
printf '1039:\tfrom all iif ocisaas0 lookup main $(touch /tmp/nuit-pwned-rule)\n' >>"$STATE/ip-rules"; : >"$CALLS"
vlan_run --check >/dev/null 2>&1 && bad "an unreadable pod rule to the main table passed the check"
vlan_run >"$tmp/out" 2>&1 && bad "an unreadable pod rule to the main table was accepted"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && ! grep -q 'pwned' "$CALLS" && [ ! -e /tmp/nuit-pwned-rule ] && ok "ncs_vlan: a pod rule to the main table that is not plain words is drift, never reaches a command, and fails the apply" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan hostile rule"; }
# a machine an agent applied before nuit-cloud/nuit-cloud#205 (24 Nov 2026): replies from pods to connections that arrived on the
# management interface went back by the main table: rule 1030 "fwmark 0x31 lookup main" and two mangle rules, in place and in the file.
# No such connection reaches a pod any more (container-management publishes on the tenant VLAN address), so all three are stale.
# old_mgmt <management interface> [xmark]: that machine; with "xmark" its mangle rules are as iptables -S prints them on a real machine.
OLD_M1='fwmark 0x31 lookup main'
old_mgmt() {
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; rm -f "$STATE/curl-fail"
  _m2="-i $1 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x31"; _m3='-i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark'
  awk -v m2="$_m2" -v m3="$_m3" -v m1="$OLD_M1" '
    /pre-down ip rule del fwmark 0x5 lookup 105/ {print "    pre-down ip rule del " m1 " 2>/dev/null || true"; print "    pre-down iptables -t mangle -D PREROUTING " m3 " 2>/dev/null || true"; print "    pre-down iptables -t mangle -D PREROUTING " m2 " 2>/dev/null || true"}
    {print}
    /post-up .*ip rule add fwmark 0x5 lookup 105 priority 1050/ {print "    post-up iptables -t mangle -C PREROUTING " m2 " 2>/dev/null || iptables -t mangle -A PREROUTING " m2; print "    post-up iptables -t mangle -C PREROUTING " m3 " 2>/dev/null || iptables -t mangle -A PREROUTING " m3; print "    post-up ip -4 rule show | grep -Ewq '\''" m1 "'\'' || ip rule add " m1 " priority 1030"}' "$f" >"$f.new"; mv "$f.new" "$f"
  printf '1030:\tfrom all %s\n' "$OLD_M1" >>"$STATE/ip-rules"
  if [ "${2:-}" = xmark ]; then
    printf '%s\n' "-i $1 -m conntrack --ctstate NEW -j CONNMARK --set-xmark 0x31/0xffffffff" '-i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark --nfmask 0xffffffff --ctmask 0xffffffff' >>"$STATE/iptables"
  else
    printf '%s\n' "$_m2" "$_m3" >>"$STATE/iptables"
  fi
  cp "$f" "$tmp/vlan5.mgmt"; cp "$STATE/ip-rules" "$tmp/rules.mgmt"; cp "$STATE/iptables" "$tmp/ipt.mgmt"; : >"$CALLS"
}
old_mgmt eth0
[ "$(mgmt_items)" = 3 ] && [ "$(grep -c '0x31' "$f")" = 6 ] && [ "$(route_of eth0)" = main ] || { cat "$f" "$STATE/ip-rules" "$STATE/iptables"; bad "the pre-#205 fixture is not what the old agent left"; }
vlan_run --check >"$tmp/out" 2>&1 && bad "a machine with the old management mark passed the check"
grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && cmp -s "$f" "$tmp/vlan5.mgmt" && cmp -s "$STATE/iptables" "$tmp/ipt.mgmt" \
  && ok "ncs_vlan: the old management mark's rules (1030, the two 0x31 mangle rules) are drift, and the check changes nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan check old mark"; }
# any one of them alone, in place or in the file, is drift
for item in "$(printf '1030:\tfrom all %s' "$OLD_M1")"; do
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; printf '%s\n' "$item" >>"$STATE/ip-rules"
  vlan_run --check >/dev/null 2>&1 && bad "rule 1030 alone in place passed the check"
done
for item in '-i eth0 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x31' '-i eth0.31 -m conntrack --ctstate NEW -j CONNMARK --set-xmark 0x31/0xffffffff' \
  '-i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark' '-i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark --nfmask 0xffffffff --ctmask 0xffffffff'; do
  cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; printf '%s\n' "$item" >>"$STATE/iptables"
  vlan_run --check >/dev/null 2>&1 && bad "the mangle rule '$item' alone in place passed the check"
done
for item in 'fwmark 0x31' 'set-mark 0x31' 'mark 0x31 -j'; do
  cp "$tmp/vlan5.mgmt" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
  grep -v '0x31' "$f" >"$f.new"; grep "$item" "$tmp/vlan5.mgmt" | head -n 1 >>"$f.new"; mv "$f.new" "$f"
  vlan_run --check >/dev/null 2>&1 && bad "the old line '$item' alone in the file passed the check"
done
cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
printf '%s\n' '-i eth0 -m connmark --mark 0x310 -j CONNMARK --restore-mark' '-i eth1 -m mark --mark 0x31 -j ACCEPT' >>"$STATE/iptables"; printf '1031:\tfrom all fwmark 0x5 lookup main\n' >>"$STATE/ip-rules"
vlan_run --check >/dev/null 2>&1 && ok "ncs_vlan: the old management mark's rules, any one of them in place or in the file, are drift; a rule with another mark is not the agent's and is left alone" \
  || bad "a rule with another mark was taken for the old management mark's"
# apply: the pod rules are there already, so nothing is added; the three old items go, the file is rewritten; the 0x5 marks are untouched
old_mgmt eth0; vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "vlan apply over the old management mark"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -qx "ip rule del $OLD_M1 priority 1030" "$CALLS" \
  && grep -qx 'iptables -t mangle -D PREROUTING -i eth0 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x31' "$CALLS" \
  && grep -qx 'iptables -t mangle -D PREROUTING -i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark' "$CALLS" \
  && ! grep -q 'ip rule add\|iptables -t mangle -A\|^ifup\|^ifdown\|ip link' "$CALLS" && [ "$(mgmt_items)" = 0 ] \
  && cmp -s "$f" "$tmp/vlan5.full" && cmp -s "$STATE/ip-rules" "$tmp/rules.full" && cmp -s "$STATE/iptables" "$tmp/ipt.full" \
  && grep -q 'stale rules removed: 1 policy, 2 mangle' "$tmp/out" && vlan_run --check >/dev/null 2>&1 \
  && [ "$(route_of eth0.5)" = 105 ] && [ "$(route_of pod)" = 105 ] && [ "$(route_of pod 10.20.0.50)" = main ] \
  && ok "ncs_vlan: an apply removes rule 1030 and the two 0x31 mangle rules, in place and from the file; the pod rules and the tenant VLAN's 0x5 marks and rule 1050 are exactly as before" \
  || { cat "$tmp/out"; cat "$CALLS"; diff "$tmp/vlan5.full" "$f"; diff "$tmp/rules.full" "$STATE/ip-rules"; diff "$tmp/ipt.full" "$STATE/iptables"; bad "vlan apply over the old management mark"; }
: >"$CALLS"; vlan_run >/dev/null 2>&1 && [ ! -s "$CALLS" ] && [ "$(mgmt_items)" = 0 ] && ok "ncs_vlan: a second apply after removing the old management mark changes nothing" || { cat "$CALLS"; bad "vlan old mark idempotent"; }
# the management address moved onto eth0.31 since, and the kernel lists the rules as iptables -S does (--set-xmark, the masks): found
# whatever their interface and removed by what iptables lists
sed -i.bak 's/^eth0 10.10.10.170\/24$/eth0.31 10.31.0.18\/24/' "$STATE/ip-addrs"; printf 'default via 10.31.0.1 dev eth0.31\n' >"$STATE/ip-route"; echo eth0.31 >>"$STATE/ip-links"
old_mgmt eth0.31 xmark; vlan_run >"$tmp/out" 2>&1 \
  && grep -qx 'iptables -t mangle -D PREROUTING -i eth0.31 -m conntrack --ctstate NEW -j CONNMARK --set-xmark 0x31/0xffffffff' "$CALLS" \
  && grep -qx 'iptables -t mangle -D PREROUTING -i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark --nfmask 0xffffffff --ctmask 0xffffffff' "$CALLS" \
  && [ "$(mgmt_items)" = 0 ] && cmp -s "$STATE/iptables" "$tmp/ipt.full" && cmp -s "$f" "$tmp/vlan5.full" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: after a management move onto eth0.31, the old rules as iptables -S lists them (--set-xmark 0x31/0xffffffff) are found and removed" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/iptables"; bad "vlan old mark xmark"; }
# the change cannot be proved: exactly what the run removed comes back, and the old file with it
old_mgmt eth0.31 xmark; touch "$STATE/curl-fail"
vlan_run >"$tmp/out" 2>&1 && bad "an unproved removal of the old management mark was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && cmp -s "$f" "$tmp/vlan5.mgmt" && cmp -s "$STATE/iptables" "$tmp/ipt.mgmt" && [ "$(sort "$STATE/ip-rules")" = "$(sort "$tmp/rules.mgmt")" ] \
  && grep -qx "ip rule add $OLD_M1 priority 1030" "$CALLS" && ! grep -q '^ifup\|^ifdown\|ip link' "$CALLS" \
  && ok "ncs_vlan: a removal of the old management mark that cannot be proved puts back exactly what it removed (rule 1030, both mangle rules as they were listed) and the old file" \
  || { cat "$tmp/out"; cat "$CALLS"; diff "$tmp/vlan5.mgmt" "$f"; diff "$tmp/ipt.mgmt" "$STATE/iptables"; sort "$STATE/ip-rules" | diff "$(sort "$tmp/rules.mgmt" >"$tmp/rs"; echo "$tmp/rs")" -; bad "vlan old mark rollback"; }
rm -f "$STATE/curl-fail"
# a machine from before #197 and #205 both: the old suppress_prefixlength rule and the old mark go in one apply, and come back together
vlan_net; cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
for a in 18 19 20 21; do netmodel add "10.20.0.$a/24" eth0.5; done; echo 'default via 10.20.0.1 dev eth0.5' >"$STATE/ip-t105"
both_old() { old_fix; printf '1030:\tfrom all %s\n' "$OLD_M1" >>"$STATE/ip-rules"; printf '%s\n' '-i eth0 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x31' '-i ocisaas0 -m connmark --mark 0x31 -j CONNMARK --restore-mark' >>"$STATE/iptables"
  cp "$STATE/ip-rules" "$tmp/rules.both"; cp "$STATE/iptables" "$tmp/ipt.both"; cp "$f" "$tmp/vlan5.both"; : >"$CALLS"; }
both_old; vlan_run >"$tmp/out" 2>&1 && grep -qx "ip rule del $OLD_SEL priority 1040" "$CALLS" && grep -qx "ip rule del $OLD_M1 priority 1030" "$CALLS" \
  && ! grep -q 'suppress_prefixlength' "$STATE/ip-rules" && [ "$(mgmt_items)" = 0 ] && [ "$(routing_items)" = 3 ] && cmp -s "$STATE/iptables" "$tmp/ipt.full" && cmp -s "$f" "$tmp/vlan5.full" \
  && grep -q 'stale rules removed: 2 policy, 2 mangle' "$tmp/out" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: a machine with the old suppress_prefixlength rule and the old management mark has both removed in one apply" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules" "$STATE/iptables"; bad "vlan both old"; }
both_old; touch "$STATE/curl-fail"; vlan_run >"$tmp/out" 2>&1 && bad "an unproved removal of both old forms was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && [ "$(sort "$STATE/ip-rules")" = "$(sort "$tmp/rules.both")" ] && cmp -s "$STATE/iptables" "$tmp/ipt.both" && cmp -s "$f" "$tmp/vlan5.both" \
  && ok "ncs_vlan: and a rollback puts both back as they were" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-rules" "$STATE/iptables"; bad "vlan both old rollback"; }
rm -f "$STATE/curl-fail"
# a hostile mangle rule with the old mark is never passed to a command: drift, and the apply fails
cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"
printf '%s\n' '-i eth0 -m comment --comment $(touch /tmp/nuit-pwned-mangle) -j CONNMARK --set-mark 0x31' >>"$STATE/iptables"; : >"$CALLS"
vlan_run --check >/dev/null 2>&1 && bad "an unreadable mangle rule with the old mark passed the check"
vlan_run >"$tmp/out" 2>&1 && bad "an unreadable mangle rule with the old mark was accepted"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && ! grep -q 'pwned\|iptables -t mangle -D' "$CALLS" && [ ! -e /tmp/nuit-pwned-mangle ] && grep -q 'pwned' "$STATE/iptables" \
  && ok "ncs_vlan: a mangle rule with the old mark that is not plain words is drift, never reaches a command, and fails the apply" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan hostile mangle rule"; }
vlan_net; cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; : >"$CALLS"
for a in 18 19 20 21; do netmodel add "10.20.0.$a/24" eth0.5; done; echo 'default via 10.20.0.1 dev eth0.5' >"$STATE/ip-t105"
# a given tenant address missing from an up VLAN is added as it runs, not by ifup
old_agent; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; sed -i.bak '/^eth0.5 10.20.0.21\/24$/d' "$STATE/ip-addrs"
vlan_run >"$tmp/out" 2>&1 && grep -qx 'ip addr add 10.20.0.21/24 dev eth0.5' "$CALLS" && ! grep -q '^ifup' "$CALLS" && grep -qx 'eth0.5 10.20.0.21/24' "$STATE/ip-addrs" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: a given tenant address missing from an up VLAN is added as it runs" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan live tenant address"; }
cp "$tmp/vlan5.full" "$f"; cp "$tmp/rules.full" "$STATE/ip-rules"; cp "$tmp/ipt.full" "$STATE/iptables"; rm -f "$STATE/curl-fail"
# tenant addresses are leases the tenant-address task moves (nuit-cloud/nuit-cloud#189): a line in the file that was not given is the task's
(TENANTS="10.20.0.19 10.20.0.20"; vlan_run --check >/dev/null 2>&1) && ok "ncs_vlan: a tenant line in the file that was not given is allowed (the tenant-address task's)" || bad "an extra tenant line failed the check"
(TENANTS=""; vlan_run --check >/dev/null 2>&1) && ok "ncs_vlan: with no tenant addresses given the check is the interface and the node's own address" || bad "an empty tenant list failed the check"
(TENANTS="10.20.0.19 10.20.0.22"; vlan_run --check >/dev/null 2>&1) && bad "a given tenant address missing from the file passed the check" || ok "ncs_vlan: a given tenant address that is not in the file is drift"
sed -i.bak '/ip addr add 10.20.0.20\//d' "$f"
(TENANTS="10.20.0.19 10.20.0.20"; vlan_run --check >/dev/null 2>&1) && bad "a given tenant address missing from the file (but live) passed the check" || ok "ncs_vlan: a given tenant address live but not in the file is drift"
grep -v '^eth0.5 10.20.0.21/24$' "$STATE/ip-addrs" >"$STATE/ip-addrs.new"; mv "$STATE/ip-addrs.new" "$STATE/ip-addrs"
(TENANTS="10.20.0.21"; vlan_run --check >/dev/null 2>&1) && bad "a given tenant address in the file but not live passed the check" || ok "ncs_vlan: a given tenant address in the file but not live is drift"
echo "eth0.5 10.20.0.21/24" >>"$STATE/ip-addrs"
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
$(pod_persist eth0)
HAND
: >"$CALLS"; vlan_run --check >"$tmp/out" 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_vlan: an equivalent VLAN made by hand is adopted, not rewritten" || { cat "$tmp/out"; cat "$f"; bad "vlan adopt"; }

# an empty tenant list is accepted: the file carries the interface and the node's own address only
vlan_net
(TENANTS=""; vlan_run) >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "vlan apply with no tenant addresses"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -q '^    address 10.20.0.18/24$' "$f" && ! grep -q 'ip addr add' "$f" && (TENANTS=""; vlan_run --check >/dev/null 2>&1) \
  && ok "ncs_vlan: an empty tenant list is accepted, and the file then has no tenant lines" || { cat "$tmp/out"; cat "$f"; bad "vlan empty tenants"; }
# an apply keeps every tenant line already in the file (the tenant-address task's, in an old agent file or a hand-made one) and adds the given ones
for kind in agent hand; do
  vlan_net
  if [ "$kind" = agent ]; then
    # what the feature wrote for 10.20.0.19, then two lines the tenant-address task added around it
    (TENANTS="10.20.0.19"; vlan_run >/dev/null 2>&1) || bad "vlan apply ($kind setup)"
    awk '/ip addr add 10.20.0.19\//{print "    post-up ip addr add 10.20.0.40/24 dev eth0.5 2>/dev/null || true"; print; print "    post-up ip addr add 10.20.0.41/24 dev eth0.5 2>/dev/null || true"; next} {print}' "$f" >"$f.new"; mv "$f.new" "$f"
  else
    cat >"$f" <<HAND
# Managed by deploy/server-vlan5.sh (nuit-cloud). Remove with: sh server-vlan5.sh --remove
auto eth0.5
iface eth0.5 inet static
    address 10.20.0.18/24
    pre-up modprobe 8021q
    # secondary addresses for tenants' DNS
    post-up ip addr add 10.20.0.40/24 dev eth0.5 2>/dev/null || true
    post-up ip addr add 10.20.0.19/24 dev eth0.5 2>/dev/null || true
    post-up ip addr add 10.20.0.41/24 dev eth0.5 2>/dev/null || true
    # post-up ip addr add 10.20.0.20/24 dev eth0.5 2>/dev/null || true
HAND
  fi
  sed -i.bak '/^eth0.5 /d' "$STATE/ip-addrs"; : >"$CALLS"
  (TENANTS="10.20.0.19 10.20.0.20 10.20.0.21"; vlan_run) >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "vlan apply over a $kind file"; }
  [ "$(grep -o 'ip addr add [0-9.]*/24 dev eth0.5' "$f" | grep -v '^#' | awk '{print $4}' | tr '\n' ' ')" = "10.20.0.40/24 10.20.0.19/24 10.20.0.41/24 10.20.0.20/24 10.20.0.21/24 " ] \
    && [ "$(grep -c 'ip addr add 10.20.0.19/' "$f")" = 1 ] && ! grep -q '^[[:space:]]*#.*ip addr add' "$f" && grep -q 'done    ncs_vlan' "$tmp/out" && grep -q '^# Managed by the nuit-cloud agent (ncs_vlan)' "$f" \
    && ok "ncs_vlan: an apply over a $kind file keeps every tenant line there, in order, and adds the given ones once" || { cat "$tmp/out"; cat "$f"; bad "vlan keeps tenant lines ($kind)"; }
done

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
grep -q 'FAILED  ncs_vlan' "$tmp/out" && grep -q 'platform could not be reached' "$tmp/out" && [ ! -e "$f" ] && grep -q 'ifdown --force eth0.5' "$CALLS" && ! grep -q '^eth0.5 ' "$STATE/ip-addrs" && [ "$(routing_items)" = 0 ] && ok "ncs_vlan: when the platform cannot be reached afterwards the VLAN is taken off again" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan rollback"; }

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
(TENANTS='10.20.0.19 $(touch /tmp/nuit-pwned) 10.20.0.999'; vlan_run) >"$tmp/out3" 2>&1 && bad "a bad tenant address was accepted"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && grep -q 'MANUAL  ncs_vlan' "$tmp/out2" && grep -q 'MANUAL  ncs_vlan' "$tmp/out3" && [ ! -s "$CALLS" ] && [ ! -e "$f" ] && ok "ncs_vlan: no slice, or an invalid one, changes nothing" || { cat "$tmp/out"; bad "vlan inputs"; }

# the management address on VLAN 31 (nuit-cloud/nuit-cloud#197): the default route is on eth0.31, and the tenant VLAN still hangs off eth0
vlan_net; sed -i.bak 's/^eth0 10.10.10.170\/24$/eth0.31 10.31.0.18\/24/' "$STATE/ip-addrs"; printf 'default via 10.31.0.1 dev eth0.31\n' >"$STATE/ip-route"; echo eth0.31 >>"$STATE/ip-links"
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "vlan apply with the default route on eth0.31"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -q '^iface eth0.5 inet static$' "$f" && grep -q 'ip link add link eth0 name eth0.5 type vlan id 5' "$f" && ! grep -q 'eth0\.31\.5' "$f" && grep -q 'ifup eth0.5' "$CALLS" \
  && grep -qx 'eth0.31 10.31.0.18/24' "$STATE/ip-addrs" && grep -qx 'eth0.5 10.20.0.19/24' "$STATE/ip-addrs" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: with the default route on the management VLAN eth0.31, the tenant VLAN is eth0.5 off eth0, and the management address is untouched" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "vlan off the lower link"; }
vlan_net; printf 'default via 10.20.0.1 dev eth0.5\n' >"$STATE/ip-route"
vlan_run >"$tmp/out" 2>&1 && bad "a tenant VLAN carrying the default route was rewritten"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_vlan: a default route on the tenant VLAN's own interface is left to a person" || { cat "$tmp/out"; bad "vlan default on eth0.5"; }

# 10b. a 15-character parent (nuit-cloud/nuit-cloud#197): the tenant VLAN is vlan5, tied to the parent by vlan-raw-device
pi_vlan_net() { (NETIF=$PI; TVIF=vlan5; vlan_net); sed -i.bak '/^vlan5/d' "$STATE/ip-links" "$ROOT/proc/net/vlan/config"; }
pi_vlan_net
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "the tenant VLAN on a 15-character parent"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -qx 'auto vlan5' "$f" && grep -qx 'iface vlan5 inet static' "$f" && grep -qx "    vlan-raw-device $PI" "$f" && grep -q "ip link add link $PI name vlan5 type vlan id 5" "$f" \
  && grep -q 'post-up ip addr add 10.20.0.20/24 dev vlan5' "$f" && grep -q 'net.ipv4.conf.vlan5.rp_filter=2' "$f" && grep -q 'post-down ip link del vlan5' "$f" && ! grep -q "$PI\\.5" "$f" \
  && grep -q '|| ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040$' "$f" && grep -q '|| ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041$' "$f" \
  && grep -q '|| ip rule add iif ocisaas0 lookup 105 priority 1045$' "$f" && [ "$(routing_items)" = 3 ] \
  && ! grep -q '0x31' "$f" && [ "$(route_of vlan5)" = 105 ] && [ "$(route_of pod)" = 105 ] \
  && grep -q 'ifup vlan5' "$CALLS" && grep -qx 'vlan5 10.20.0.19/24' "$STATE/ip-addrs" && grep -qx "$PI 10.10.10.170/24" "$STATE/ip-addrs" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: on a 15-character parent the tenant VLAN is vlan5 (vlan-raw-device $PI), brought up, and then passes the check" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "pi tenant vlan"; }
pi_vlan_net; touch "$STATE/curl-fail"
vlan_run >"$tmp/out" 2>&1 && bad "an unproved vlan5 was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && grep -q 'ifdown --force vlan5' "$CALLS" && grep -q 'ip link del vlan5' "$CALLS" && [ ! -e "$f" ] && ! grep -q '^vlan5 ' "$STATE/ip-addrs" \
  && ok "ncs_vlan: a tenant VLAN on vlan5 that cannot be proved is taken down by that name" || { cat "$tmp/out"; cat "$CALLS"; bad "pi tenant vlan rollback"; }
# after the Pi's management move: the default route is on vlan30, whose parent (from the kernel) carries the tenant VLAN too
pi_vlan_net; sed -i.bak "s/^$PI 10.10.10.170\/24\$/vlan30 10.30.0.11\/24/" "$STATE/ip-addrs"; printf 'default via 10.30.0.1 dev vlan30\n' >"$STATE/ip-route"
echo vlan30 >>"$STATE/ip-links"; echo "vlan30 | 30 | $PI" >>"$ROOT/proc/net/vlan/config"
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "the tenant VLAN with the default route on vlan30"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -qx 'iface vlan5 inet static' "$f" && grep -qx "    vlan-raw-device $PI" "$f" && ! grep -q 'vlan30' "$f" && grep -q 'ifup vlan5' "$CALLS" \
  && [ "$(route_of vlan5)" = 105 ] \
  && grep -qx 'vlan30 10.30.0.11/24' "$STATE/ip-addrs" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: with the default route on vlan30 the tenant VLAN is vlan5 off $PI, beside it, and the management address is untouched" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "pi tenant vlan behind vlan30"; }
# a VLAN 5 the kernel already has on eth0 under the name vlan5 (made by hand) is used by that name, not doubled with eth0.5
(TVIF=vlan5; vlan_net)
vlan_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "the tenant VLAN over a hand-made vlan5"; }
grep -q 'done    ncs_vlan' "$tmp/out" && grep -qx 'iface vlan5 inet static' "$f" && grep -qx '    vlan-raw-device eth0' "$f" && ! grep -q 'eth0\.5' "$f" && grep -q 'ifup vlan5' "$CALLS" && vlan_run --check >/dev/null 2>&1 \
  && ok "ncs_vlan: a VLAN 5 the kernel has on eth0 as vlan5 is found by its VLAN id and used as it is, not doubled with eth0.5" || { cat "$tmp/out"; cat "$f"; cat "$CALLS"; bad "vlan5 on a short parent"; }
# a vlan5 that belongs to another parent is someone else's; a hostile interface name never reaches a file
pi_vlan_net; echo vlan5 >>"$STATE/ip-links"; echo "vlan5 | 5 | eth1" >>"$ROOT/proc/net/vlan/config"
vlan_run >"$tmp/out" 2>&1 && bad "a vlan5 on another parent was taken"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && grep -q "vlan5 exists already and is not VLAN 5 on $PI" "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$f" ] || { cat "$tmp/out"; cat "$CALLS"; bad "vlan5 on another parent"; }
vlan_net; printf 'default via 10.10.10.1 dev eth0;reboot\n' >"$STATE/ip-route"
vlan_run >"$tmp/out" 2>&1 && bad "a hostile interface name was used"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && grep -q 'cannot carry a VLAN sub-interface' "$tmp/out" && [ ! -s "$CALLS" ] && [ ! -e "$f" ] \
  && ok "ncs_vlan: a vlan5 on another parent, or a hostile interface name, is left to a person and nothing changes" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan hostile parent"; }
# a hostile management interface name (the default route's device, which the mark names) never reaches a file or a command
vlan_net; printf 'default via 10.10.10.1 dev eth0.31;reboot\n' >"$STATE/ip-route"
vlan_run >"$tmp/out" 2>&1 && bad "a hostile management interface name was used"
grep -q 'MANUAL  ncs_vlan' "$tmp/out" && ! grep -q 'reboot' "$CALLS" && ! grep -q 'iptables' "$CALLS" && [ ! -e "$f" ] \
  && ok "ncs_vlan: a hostile management interface name is left to a person and reaches nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "vlan hostile management interface"; }

# 10c. renumbering a tenant VLAN in service (nuit-cloud/nuit-cloud#197): the machine's own address moves to 10.20.255.x and the network
#      grows from /24 to /16, as it runs: no ifdown, no ifup, no tenant address ever missing, the old own address gone only once the
#      platform was reached, the Ingress Service's 10.20.0.34 kept as a tenant address; a rollback never takes the VLAN down
vlan16() { env NCS_VLAN_ID=5 NCS_VLAN_PREFIX="${PFX:-16}" NCS_VLAN_ADDRESS="${OWN:-10.20.255.18}" NCS_VLAN_GATEWAY=10.20.0.1 NCS_VLAN_TENANT_ADDRESSES="${TENANTS-10.20.0.19 10.20.0.20 10.20.0.21}" NCS_PLATFORM_URL=https://agent.example sh "$here/bootstrap.sh" nuit-cloud-vlan ${1:+"$1"}; }
# in_service <interface> [own address] [tenant addresses]: a VLAN an agent before #197 set up, at /24, with a tenant address the
# tenant-address task added (10.20.0.40, a line in the file and live) and the pod routing
in_service() {
  (TVIF="$1"; vlan_net)
  (TENANTS="${3-10.20.0.19 10.20.0.20 10.20.0.21}"; OWN="${2:-10.20.0.18}"; PFX=24; vlan16) >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "in-service setup on $1"; }
  # (an agent before #197 wrote no promote_secondaries line, and the machine has it off)
  sed -i.bak '/promote_secondaries/d' "$f"; echo 0 >"$ROOT/proc/sys/net/ipv4/conf/$1/promote_secondaries"
  awk -v v="$1" '/ip addr add / {last = NR} {line[NR] = $0} END {for (i = 1; i <= NR; i++) {print line[i]; if (i == last) printf "    post-up ip addr add 10.20.0.40/24 dev %s 2>/dev/null || true\n", v}}' "$f" >"$f.new"; mv "$f.new" "$f"
  netmodel add 10.20.0.40/24 "$1"; : >"$CALLS"; rm -f "$STATE/gaps"
  cp "$f" "$tmp/svc.file"; sort "$STATE/ip-addrs" >"$tmp/svc.addrs"; sort "$STATE/ip-routes" >"$tmp/svc.routes"
}
vif_addrs() { awk -v v="$1" '$1 == v {print $2}' "$STATE/ip-addrs" | sort | tr '\n' ' '; }
line_of() { grep -n -- "$1" "$CALLS" | head -n 1 | cut -d: -f1; }
for V in eth0.5 vlan5; do
  in_service "$V"
  [ "$(vif_addrs "$V")" = "10.20.0.18/24 10.20.0.19/24 10.20.0.20/24 10.20.0.21/24 10.20.0.40/24 " ] && grep -qx "10.20.0.0/24 dev $V proto kernel scope link src 10.20.0.18" "$STATE/ip-routes" || { cat "$STATE/ip-addrs" "$STATE/ip-routes"; bad "the in-service fixture on $V"; }
  vlan16 --check >"$tmp/out" 2>&1 && bad "a VLAN with the old own address and prefix passed the check ($V)"
  grep -q 'DRIFT   ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] || { cat "$tmp/out"; bad "renumber check ($V)"; }
  echo "10.20.0.19 10.20.0.20 10.20.0.21 10.20.0.40" >"$STATE/watch"
  vlan16 >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "renumbering a VLAN in service ($V)"; }
  grep -q 'done    ncs_vlan' "$tmp/out" && ! grep -q '^ifup\|^ifdown\|ip link' "$CALLS" \
    && [ "$(vif_addrs "$V")" = "10.20.0.19/16 10.20.0.20/16 10.20.0.21/16 10.20.0.40/16 10.20.255.18/16 " ] \
    && grep -qx "10.20.0.0/16 dev $V proto kernel scope link src 10.20.255.18" "$STATE/ip-routes" && ! grep -q "^10.20.0.0/24 dev $V " "$STATE/ip-routes" \
    && grep -qx "default via 10.20.0.1 dev $V" "$STATE/ip-t105" && [ ! -s "$STATE/gaps" ] \
    && ok "ncs_vlan ($V): a VLAN in service moves to its new own address and /16 as it runs, with no ifdown or ifup, the connected route from the new own address and table 105 by the gateway" \
    || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs" "$STATE/ip-routes" "$STATE/gaps" 2>/dev/null; bad "renumber live ($V)"; }
  grep -qx '    address 10.20.255.18/16' "$f" && [ "$(grep -o 'ip addr add [0-9.]*/[0-9]*' "$f" | awk '{print $4}' | tr '\n' ' ')" = "10.20.0.19/16 10.20.0.20/16 10.20.0.21/16 10.20.0.40/16 " ] \
    && grep -q "pre-up sysctl -qw net.ipv4.conf.$(echo "$V" | tr . /).promote_secondaries=1" "$f" \
    && ok "ncs_vlan ($V): the file says the new own address and /16 for the next boot, and the tenant-address task's line has the new prefix too" || { cat "$f"; bad "renumber file ($V)"; }
  add_own="$(line_of "^ip addr add 10.20.255.18/16 dev $V\$")"; add_t="$(line_of "^ip addr add 10.20.0.19/16 dev $V\$")"; del_t="$(line_of "^ip addr del 10.20.0.19/24 dev $V\$")"
  proof="$(line_of '^curl ')"; del_own="$(line_of "^ip addr del 10.20.0.18/24 dev $V\$")"
  [ -n "$add_own" ] && [ -n "$del_own" ] && [ "$add_own" -lt "$add_t" ] && [ "$add_t" -lt "$del_t" ] && [ "$del_t" -lt "$proof" ] && [ "$proof" -lt "$del_own" ] \
    && grep -q "sysctl -qw net.ipv4.conf.$(echo "$V" | tr . /).promote_secondaries=1" "$CALLS" \
    && ok "ncs_vlan ($V): the order: the new own address first, each tenant address with /16 before its /24 goes (never missing), the old own address only after the platform answered" \
    || { cat "$CALLS"; bad "renumber order ($V)"; }
  [ "$(route_of pod)" = 105 ] && [ "$(route_of "$V")" = 105 ] && ok "ncs_vlan ($V): pods still leave by table 105, replies on the tenant VLAN by it" || bad "renumber pod routing ($V)"
  grep -qxF "$(printf '1041:\tfrom all to 10.20.0.0/16 iif ocisaas0 [detached] lookup main')" "$STATE/ip-rules" && ! grep -q 'to 10\.20\.0\.0/24' "$STATE/ip-rules" \
    && grep -qx 'ip rule del to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041' "$CALLS" && grep -q '|| ip rule add to 10.20.0.0/16 iif ocisaas0 lookup main priority 1041$' "$f" && ! grep -q 'to 10\.20\.0\.0/24' "$f" \
    && [ "$(route_of pod 10.20.200.9)" = main ] && [ "$(grep -c 'iif ocisaas0' "$STATE/ip-rules")" = 3 ] \
    && ok "ncs_vlan ($V): the pods' tenant VLAN rule follows the network to 10.20.0.0/16, the /24 one removed, in place and in the file" || { cat "$CALLS"; cat "$STATE/ip-rules"; bad "renumber pod subnet rule ($V)"; }
  : >"$CALLS"; vlan16 >"$tmp/out" 2>&1 && vlan16 --check >/dev/null 2>&1 && [ ! -s "$CALLS" ] && ok "ncs_vlan ($V): after the renumbering --check is clean and a second run changes nothing" || { cat "$tmp/out"; cat "$CALLS"; bad "renumber idempotent ($V)"; }
done

# the Ingress Service machine: its old own address 10.20.0.34 stays, now a tenant address with /16; it is never missing
in_service eth0.5 10.20.0.34 "10.20.0.3"
echo "10.20.0.34 10.20.0.3 10.20.0.40" >"$STATE/watch"
(OWN=10.20.255.34; TENANTS="10.20.0.34 10.20.0.3"; vlan16) >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$CALLS"; bad "renumbering the ingress machine"; }
grep -q 'done    ncs_vlan' "$tmp/out" && [ "$(vif_addrs eth0.5)" = "10.20.0.3/16 10.20.0.34/16 10.20.0.40/16 10.20.255.34/16 " ] && [ ! -s "$STATE/gaps" ] \
  && grep -qx '10.20.0.0/16 dev eth0.5 proto kernel scope link src 10.20.255.34' "$STATE/ip-routes" && ! grep -q '^10.20.0.0/24 ' "$STATE/ip-routes" \
  && grep -qx '    address 10.20.255.34/16' "$f" && grep -q 'post-up ip addr add 10.20.0.34/16 dev eth0.5' "$f" && ! grep -q '/24' "$f" \
  && ! grep -q '^ip addr del 10.20.0.34/16' "$CALLS" && [ "$(line_of '^ip addr add 10.20.0.34/16')" -lt "$(line_of '^ip addr del 10.20.0.34/24')" ] \
  && (OWN=10.20.255.34; TENANTS="10.20.0.34 10.20.0.3"; vlan16 --check >/dev/null 2>&1) \
  && ok "ncs_vlan: on the Ingress Service machine the old own address 10.20.0.34 is kept as a tenant address, moved to /16 without ever being missing" \
  || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs" "$STATE/ip-routes" "$STATE/gaps" "$f" 2>/dev/null; bad "renumber ingress"; }

# only the prefix changes (the own address stays): the own address and every tenant address move to /16 as they run
in_service eth0.5; echo "10.20.0.18 10.20.0.19 10.20.0.20 10.20.0.21 10.20.0.40" >"$STATE/watch"
(OWN=10.20.0.18; vlan16 --check >/dev/null 2>&1) && bad "a prefix change alone passed the check"
(OWN=10.20.0.18; vlan16) >"$tmp/out" 2>&1 && [ "$(vif_addrs eth0.5)" = "10.20.0.18/16 10.20.0.19/16 10.20.0.20/16 10.20.0.21/16 10.20.0.40/16 " ] && [ ! -s "$STATE/gaps" ] \
  && grep -qx '10.20.0.0/16 dev eth0.5 proto kernel scope link src 10.20.0.18' "$STATE/ip-routes" && ! grep -q '^ifup\|^ifdown' "$CALLS" && (OWN=10.20.0.18; vlan16 --check >/dev/null 2>&1) \
  && ok "ncs_vlan: a prefix change alone (/24 to /16) moves the own address and the tenant addresses as they run, none ever missing" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs" "$STATE/gaps" 2>/dev/null; bad "prefix change only"; }

# promote_secondaries cannot be turned on: the order alone keeps every tenant address (a network's secondaries go before its primary)
in_service eth0.5; touch "$STATE/no-promote"; echo "10.20.0.19 10.20.0.20 10.20.0.21 10.20.0.40" >"$STATE/watch"
vlan16 >"$tmp/out" 2>&1 && [ "$(vif_addrs eth0.5)" = "10.20.0.19/16 10.20.0.20/16 10.20.0.21/16 10.20.0.40/16 10.20.255.18/16 " ] && [ ! -s "$STATE/gaps" ] \
  && ok "ncs_vlan: with promote_secondaries off, no tenant address is lost either: secondaries go before their primary" || { cat "$tmp/out"; cat "$CALLS"; cat "$STATE/ip-addrs" "$STATE/gaps" 2>/dev/null; bad "renumber without promote"; }
rm -f "$STATE/no-promote"

# the platform cannot be reached after the change: everything comes back as it was, the VLAN never taken down, no tenant address missing
in_service eth0.5; touch "$STATE/curl-fail"; echo "10.20.0.19 10.20.0.20 10.20.0.21 10.20.0.40" >"$STATE/watch"
vlan16 >"$tmp/out" 2>&1 && bad "an unproved renumbering was kept"
grep -q 'FAILED  ncs_vlan' "$tmp/out" && grep -q 'platform could not be reached after the change' "$tmp/out" && cmp -s "$f" "$tmp/svc.file" \
  && [ "$(sort "$STATE/ip-addrs")" = "$(cat "$tmp/svc.addrs")" ] && [ "$(sort "$STATE/ip-routes")" = "$(cat "$tmp/svc.routes")" ] && [ ! -s "$STATE/gaps" ] \
  && ! grep -q '^ifup\|^ifdown\|ip link del' "$CALLS" && [ "$(cat "$ROOT/proc/sys/net/ipv4/conf/eth0.5/promote_secondaries")" = 0 ] && ! grep -q '^ip addr del 10.20.0.18/24' "$CALLS" \
  && grep -qx 'default via 10.20.0.1 dev eth0.5' "$STATE/ip-t105" \
  && grep -q 'to 10\.20\.0\.0/24 iif ocisaas0' "$STATE/ip-rules" && ! grep -q 'to 10\.20\.0\.0/16' "$STATE/ip-rules" \
  && ok "ncs_vlan: a renumbering that cannot be proved is undone as it runs: the old addresses and prefixes, routes, file and promote_secondaries come back, the old own address was never removed, no ifdown" \
  || { cat "$tmp/out"; cat "$CALLS"; diff "$tmp/svc.file" "$f"; sort "$STATE/ip-addrs" | diff "$tmp/svc.addrs" -; sort "$STATE/ip-routes" | diff "$tmp/svc.routes" -; cat "$STATE/gaps" 2>/dev/null; bad "renumber rollback"; }
rm -f "$STATE/curl-fail"
# the platform answers until the old own address goes, then not: it comes back with everything else
stub curl 'n="$(cat "$STATE/curl-ok" 2>/dev/null || echo 99)"; [ "$n" -gt 0 ] || exit 22; echo $((n - 1)) >"$STATE/curl-ok"; echo "curl $*" >>"$CALLS"'
in_service eth0.5; echo 1 >"$STATE/curl-ok"; echo "10.20.0.19 10.20.0.20 10.20.0.21 10.20.0.40" >"$STATE/watch"
vlan16 >"$tmp/out" 2>&1 && bad "a renumbering that lost the platform with the old own address was kept"
grep -q 'once the old addresses were removed' "$tmp/out" && grep -q '^ip addr del 10.20.0.18/24' "$CALLS" && cmp -s "$f" "$tmp/svc.file" \
  && [ "$(sort "$STATE/ip-addrs")" = "$(cat "$tmp/svc.addrs")" ] && grep -qx '10.20.0.0/24 dev eth0.5 proto kernel scope link src 10.20.0.18' "$STATE/ip-routes" && ! grep -q '^10.20.0.0/16' "$STATE/ip-routes" \
  && [ ! -s "$STATE/gaps" ] && ! grep -q '^ifup\|^ifdown\|ip link del' "$CALLS" \
  && ok "ncs_vlan: when the platform is lost once the old own address is removed, it all comes back: the old own address as the primary again, the /24 addresses and route" \
  || { cat "$tmp/out"; cat "$CALLS"; sort "$STATE/ip-addrs" | diff "$tmp/svc.addrs" -; cat "$STATE/ip-routes" "$STATE/gaps" 2>/dev/null; bad "renumber rollback after the old address"; }
rm -f "$STATE/curl-ok"; stub curl '[ ! -e "$STATE/curl-fail" ] && echo "curl $*" >>"$CALLS"'

# drift on a renumbered VLAN: an old prefix left in the file or live, the old own address left live, a tenant as the route's source
in_service eth0.5; vlan16 >/dev/null 2>&1 && vlan16 --check >/dev/null 2>&1 || bad "renumbered fixture"
cp "$f" "$tmp/r.file"; cp "$STATE/ip-addrs" "$tmp/r.addrs"; cp "$STATE/ip-routes" "$tmp/r.routes"
back() { cp "$tmp/r.file" "$f"; cp "$tmp/r.addrs" "$STATE/ip-addrs"; cp "$tmp/r.routes" "$STATE/ip-routes"; : >"$CALLS"; }
back; sed -i.bak 's#10.20.0.40/16#10.20.0.40/24#' "$f"; vlan16 --check >/dev/null 2>&1 && bad "a tenant line with the old prefix passed the check"
vlan16 >/dev/null 2>&1 && grep -q 'ip addr add 10.20.0.40/16 dev' "$f" && vlan16 --check >/dev/null 2>&1 || bad "a tenant line with the old prefix was not put right"
back; sed -i.bak 's#10.20.0.20/16#10.20.0.20/24#' "$f"; vlan16 --check >/dev/null 2>&1 && bad "a given tenant line with the old prefix passed the check"
back; netmodel add 10.20.0.19/24 eth0.5; vlan16 --check >/dev/null 2>&1 && bad "a tenant address also live with the old prefix passed the check"
vlan16 >/dev/null 2>&1 && [ "$(vif_addrs eth0.5)" = "10.20.0.19/16 10.20.0.20/16 10.20.0.21/16 10.20.0.40/16 10.20.255.18/16 " ] && vlan16 --check >/dev/null 2>&1 || { cat "$STATE/ip-addrs"; bad "an old prefix left live was not taken away"; }
back; netmodel add 10.20.0.18/24 eth0.5; vlan16 --check >/dev/null 2>&1 && bad "the old own address left live passed the check"
vlan16 >/dev/null 2>&1 && ! grep -q '10.20.0.18/' "$STATE/ip-addrs" && vlan16 --check >/dev/null 2>&1 || { cat "$STATE/ip-addrs"; bad "the old own address left live was not taken away"; }
back; sed -i.bak 's#^10.20.0.0/16 dev eth0.5 proto kernel scope link src .*#10.20.0.0/16 dev eth0.5 proto kernel scope link src 10.20.0.19#' "$STATE/ip-routes"; vlan16 --check >/dev/null 2>&1 && bad "a tenant address as the connected route's source passed the check"
vlan16 >/dev/null 2>&1 && grep -qx 'ip route replace 10.20.0.0/16 dev eth0.5 proto kernel scope link src 10.20.255.18' "$CALLS" && grep -qx '10.20.0.0/16 dev eth0.5 proto kernel scope link src 10.20.255.18' "$STATE/ip-routes" && vlan16 --check >/dev/null 2>&1 \
  || { cat "$CALLS" "$STATE/ip-routes"; bad "the connected route's source was not put right"; }
back; (OWN=10.20.255.19; vlan16 --check >/dev/null 2>&1) && bad "another own address passed the check"
back; (PFX=17; vlan16 --check >/dev/null 2>&1) && bad "another prefix passed the check"
ok "ncs_vlan: drift is an old prefix in the file or live, the old own address live, a tenant address as the route's source, another own address or prefix; an apply puts each right"

# what does not fit one network is refused before anything changes: a tenant address or the gateway outside it, the own address as a tenant
in_service eth0.5
for bad_slice in "TENANTS=10.21.0.3" "TENANTS=10.20.255.18" "OWN=10.21.255.18" "PFX=31" "PFX=8"; do
  (export "$bad_slice"; vlan16) >"$tmp/out" 2>&1 && bad "the slice $bad_slice was accepted"
  grep -q 'MANUAL  ncs_vlan' "$tmp/out" && [ ! -s "$CALLS" ] && cmp -s "$f" "$tmp/svc.file" || { cat "$tmp/out"; bad "the slice $bad_slice changed something"; }
done
ok "ncs_vlan: a slice that is not one network (a tenant address or gateway outside it, the own address also a tenant, a prefix outside 16-30) changes nothing"
rm -f "$STATE/watch" "$STATE/gaps"

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
# 13b. ncs_login with the key registry (nuit-cloud/nuit-cloud#92): the marked lines are kept equal to the list, nothing else is touched
for k in reg1 reg2 reg3 mine; do ssh-keygen -q -t ed25519 -N '' -C "$k@somewhere" -f "$tmp/$k"; done
b64() { awk '{print $2}' "$tmp/$1.pub"; }
R1="$(b64 reg1)" R2="$(b64 reg2)" R3="$(b64 reg3)" MINE="$(b64 mine)"
reg_run() { env NCS_LOGIN_KEYS="$KEYS" sh "$here/bootstrap.sh" nuit-cloud-login ${1:+"$1"}; }
reg_net() { rm -rf "$ROOT/home/nuitcloud"; mkdir -p "$ROOT/home/nuitcloud/.ssh"; chmod 755 "$ROOT/home/nuitcloud/.ssh"; : >"$CALLS"; }
f="$ROOT/home/nuitcloud/.ssh/authorized_keys"
reg_net
{ echo '# my own notes'; echo "ssh-ed25519 $MINE mine@laptop"; echo "from=\"10.0.0.0/8\" ssh-ed25519 $R2 restricted"; echo "ssh-ed25519 $R1 platform-key-added-by-hand"
  echo "ssh-ed25519 $R3 nuit-cloud:revoked-key"; echo "ssh-ed25519 $MINE nuit-cloud:old-id"; printf 'ssh-rsa AAAAB3Nza unrelated'; } >"$f"
chmod 644 "$f"; ino="$(ls -i "$f" | awk '{print $1}')"; cp "$f" "$tmp/reg-before"
KEYS="platform=$R1 ops-laptop=$R2"
reg_run --check >"$tmp/out" 2>&1 && bad "a registry out of step passed the check"
grep -q 'DRIFT   ncs_login' "$tmp/out" && grep -q '1 to add, 2 to remove, 1 to adopt; 3 unmanaged' "$tmp/out" && cmp -s "$f" "$tmp/reg-before" && [ ! -s "$CALLS" ] \
  && ok "ncs_login registry: --check counts what to add, remove and adopt, and the unmanaged lines, and changes nothing" || { cat "$tmp/out"; bad "registry check"; }
for s in "$R1" "$R2" "$R3" "$MINE"; do grep -qF -- "$s" "$tmp/out" && bad "registry check printed a key body"; done
reg_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; cat "$f"; bad "registry apply"; }
grep -q 'done    ncs_login' "$tmp/out" && grep -q '1 added, 2 removed, 1 adopted; 3 unmanaged' "$tmp/out" || { cat "$tmp/out"; bad "registry apply output"; }
for s in "$R1" "$R2" "$R3" "$MINE"; do grep -qF -- "$s" "$tmp/out" && bad "registry apply printed a key body"; done
expected="$tmp/reg-expected"
{ echo '# my own notes'; echo "ssh-ed25519 $MINE mine@laptop"; echo "from=\"10.0.0.0/8\" ssh-ed25519 $R2 restricted"; echo "ssh-ed25519 $R1 nuit-cloud:platform"
  echo 'ssh-rsa AAAAB3Nza unrelated'; echo "ssh-ed25519 $R2 nuit-cloud:ops-laptop"; } >"$expected"
cmp -s "$f" "$expected" && ok "ncs_login registry: a listed key is added, revoked and stale marked lines are removed, a hand-added platform key is adopted in place, other lines kept as they were" \
  || { diff "$expected" "$f"; bad "registry file"; }
[ "$(ls -l "$f" | cut -c1-10)" = "-rw-------" ] && [ "$(ls -ld "$ROOT/home/nuitcloud/.ssh" | cut -c1-10)" = "drwx------" ] \
  && [ "$(ls -i "$f" | awk '{print $1}')" != "$ino" ] && [ -z "$(ls -A "$ROOT/home/nuitcloud/.ssh" | grep -v '^authorized_keys$')" ] \
  && grep -q "chown nuitcloud:nuitcloud $ROOT/home/nuitcloud/.ssh/.authorized_keys.nuit-cloud." "$CALLS" && grep -q "chown nuitcloud:nuitcloud $ROOT/home/nuitcloud/.ssh\$" "$CALLS" \
  && ok "ncs_login registry: the file is replaced by rename (a new file, no temporary left), mode 600 in a 700 directory, owned by the account before it lands" || { ls -la "$ROOT/home/nuitcloud/.ssh"; cat "$CALLS"; bad "registry atomic write"; }
: >"$CALLS"; reg_run >"$tmp/out" 2>&1 && reg_run --check >/dev/null 2>&1 && cmp -s "$f" "$expected" && grep -q 'ok      ncs_login' "$tmp/out" && [ ! -s "$CALLS" ] \
  && ok "ncs_login registry: in step, a second run and the check change nothing" || { cat "$tmp/out"; bad "registry idempotent"; }
# revoking: the key leaves the list, its marked line goes; the unmarked lines stay
KEYS="platform=$R1"; reg_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "registry revoke"; }
grep -q '0 added, 1 removed, 0 adopted' "$tmp/out" && ! grep -q "nuit-cloud:ops-laptop" "$f" && grep -q "restricted\$" "$f" && grep -q "mine@laptop\$" "$f" && grep -q "nuit-cloud:platform\$" "$f" \
  && ok "ncs_login registry: a revoked key's marked line is removed, the rest stays" || { cat "$tmp/out"; cat "$f"; bad "registry revoke result"; }
# a key that changes body under the same id is replaced, not kept beside the new one
KEYS="platform=$R3"; reg_run >"$tmp/out" 2>&1 && [ "$(grep -c 'nuit-cloud:platform$' "$f")" = 1 ] && grep -qF "$R3 nuit-cloud:platform" "$f" && ! grep -qF "$R1" "$f" \
  && ok "ncs_login registry: a key replaced under the same id leaves one marked line, the new one" || { cat "$tmp/out"; cat "$f"; bad "registry rekey"; }
# no file and no directory yet: made with the right modes
reg_net; rmdir "$ROOT/home/nuitcloud/.ssh"; KEYS="platform=$R1 ops-laptop=$R2"
reg_run >"$tmp/out" 2>&1 && [ "$(cat "$f")" = "$(printf 'ssh-ed25519 %s nuit-cloud:platform\nssh-ed25519 %s nuit-cloud:ops-laptop' "$R1" "$R2")" ] && [ "$(ls -l "$f" | cut -c1-10)" = "-rw-------" ] \
  && [ "$(ls -ld "$ROOT/home/nuitcloud/.ssh" | cut -c1-10)" = "drwx------" ] && grep -q '2 added, 0 removed, 0 adopted; 0 unmanaged' "$tmp/out" \
  && ok "ncs_login registry: a machine with no keys file gets one, 600 in a 700 directory" || { cat "$tmp/out"; ls -la "$ROOT/home/nuitcloud/.ssh"; bad "registry fresh"; }
# refusals: nothing is written
reg_refuse() { # reg_refuse <description> <keys>
  reg_net; printf 'ssh-ed25519 %s nuit-cloud:platform\nssh-ed25519 %s mine\n' "$R1" "$MINE" >"$f"; cp "$f" "$tmp/reg-before"
  KEYS="$2"; reg_run >"$tmp/out" 2>&1 && bad "registry: $1 was accepted"
  grep -q 'MANUAL  ncs_login' "$tmp/out" && cmp -s "$f" "$tmp/reg-before" && [ -z "$(ls -A "$ROOT/home/nuitcloud/.ssh" | grep -v '^authorized_keys$')" ] && ok "ncs_login registry: $1 is refused and nothing changes" || { cat "$tmp/out"; cat "$f"; bad "registry refusal: $1"; }
}
reg_refuse "an empty list" ""
reg_refuse "a list of spaces" "   "
reg_refuse "an upper-case id" "Platform=$R1"
reg_refuse "an id starting with -" "-platform=$R1"
reg_refuse "an id of 41 characters" "$(printf 'a%.0s' $(seq 41))=$R1"
reg_refuse "a duplicate id" "platform=$R1 platform=$R2"
reg_refuse "a duplicate key" "platform=$R1 other=$R1"
reg_refuse "a key that is not ssh-ed25519" "platform=AAAAB3NzaC1yc2EAAAADAQABAAABAQC1234567890123456789012345678901234567890123"
reg_refuse "a short key" "platform=AAAAC3NzaC1lZDI1NTE5AAAAI"
reg_refuse "shell in the list" "platform=$R1;reboot"
reg_refuse "a glob in the list" "platform=$R1 *"
reg_refuse "a second line" "platform=$R1
other=$R2"
reg_refuse "65 keys" "$(for i in $(seq 65); do printf 'k%s=%s ' "$i" "$R1"; done)"
# the hardening guard counts a marked line as the named key being present (it compares fingerprints, not comments)
reg_net; printf 'ssh-ed25519 %s nuit-cloud:platform\n' "$R1" >"$f"; chmod 600 "$f"
FPR1="$(ssh-keygen -lf "$tmp/reg1.pub" | awk '{print $2}')"
mkdir -p "$ROOT/etc/ssh/sshd_config.d"; printf 'Include /etc/ssh/sshd_config.d/*.conf\nPort 22\n' >"$ROOT/etc/ssh/sshd_config"; rm -f "$ROOT/etc/ssh/sshd_config.d/00-nuit-cloud.conf" "$STATE/sshd-bad" "$STATE/sshd-bad-after" "$STATE/another-file-wins" "$STATE/pubkey-off"
env NCS_SSH_REQUIRE_FINGERPRINTS="$FPR1" sh "$here/bootstrap.sh" nuit-cloud-ssh >"$tmp/out" 2>&1 || true
grep -q 'done    ncs_ssh_harden' "$tmp/out" && ok "ncs_ssh_harden: a marked registry line satisfies the named key" || { cat "$tmp/out"; bad "harden with a marked line"; }
# systemd expands dollar-brace, doubled dollars and percent specifiers in a transient unit's command line: the registry writer must carry none
sh -c '. "$1"; printf "%s" "$LOGIN_SYNC$LOGIN_SYNC_AWK"' sh "$here/features.sh" >"$tmp/writer" 2>/dev/null || true
[ -s "$tmp/writer" ] && ! grep -q '\${\|\$\$\|%' "$tmp/writer" && ok "ncs_login registry: the writer carries nothing systemd-run would expand" || bad "the registry writer would be expanded by systemd"
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
# SSH over IPv6 follows the setting (nuit-cloud/nuit-cloud#202): no IPv6 range named, no IPv6 rule; one named, exactly that one.
fw_mixed="$(sh -c 'p() { printf "%s\n" "$1"; }; NCS_FIREWALL_SSH_FROM="10.10.10.0/24 fd00:31::/64"; . "$1/features.sh"; fw_render' x "$here")"
printf '%s\n' "$fw_mixed" | grep -qx '    tcp dport 22 ip saddr { 10.10.10.0/24 } accept' || bad "a mixed SSH setting should give the IPv4 rule for its IPv4 range"
printf '%s\n' "$fw_mixed" | grep -qx '    tcp dport 22 ip6 saddr { fd00:31::/64 } accept' || bad "a mixed SSH setting should give the IPv6 rule for its IPv6 range only"
fw_v4only="$(sh -c 'p() { printf "%s\n" "$1"; }; NCS_FIREWALL_SSH_FROM="10.10.10.0/24"; . "$1/features.sh"; fw_render' x "$here")"
printf '%s\n' "$fw_v4only" | grep -q 'ip6 saddr' && bad "with no IPv6 range named, SSH over IPv6 must not be admitted"
fw_default="$(sh -c 'p() { printf "%s\n" "$1"; }; . "$1/features.sh"; fw_render' x "$here")"
printf '%s\n' "$fw_default" | grep -q 'fe80::\|fc00::' && bad "the default must not admit IPv6 link-local or ULA to SSH"
ok "ncs_firewall: SSH over IPv6 is admitted only from IPv6 ranges the setting names"
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
# pod rules (nuit-cloud/nuit-cloud#91): absent, the ruleset is exactly this; observe counts and logs pod traffic but drops none of it; enforce
# drops. In every mode the machine forwards only for its own containers (nuit-cloud/nuit-cloud#202): from or to a container bridge, nothing
# between its own interfaces (that is counted as host_route and dropped, never only observed).
cat >"$tmp/fw-today.nft" <<'TODAY'
# Written by the nuit-cloud agent (profile nuit-cloud-firewall). Edit the task, not this file.
table inet nuit_cloud
delete table inet nuit_cloud
table inet nuit_cloud {
  counter host_route {
  }
  chain input {
    type filter hook input priority 0; policy drop;
    iif lo accept
    iifname { "ocisaas0", "cni0", "docker0", "br-*", "pterodactyl0" } accept
    ct state established,related accept
    ct state invalid drop
    ip protocol icmp accept
    ip6 nexthdr icmpv6 accept
    udp dport 68 accept
    tcp dport 22 ip saddr { 10.10.10.0/24 } accept
    tcp dport { 80,443 } accept
  }
  chain forward {
    type filter hook forward priority 0; policy accept;
    ct state established,related accept
    iifname { "ocisaas0", "cni0", "docker0", "br-*", "pterodactyl0" } accept
    oifname { "ocisaas0", "cni0", "docker0", "br-*", "pterodactyl0" } accept
    limit rate 10/minute log prefix "nc-host_route "
    counter name host_route drop
  }
}
TODAY
export FW_TCP="80,443" FW_FROM="10.10.10.0/24"
fw_net; fw_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "fw apply without pod rules"; }
cmp -s "$RULES" "$tmp/fw-today.nft" && ok "ncs_firewall: with no pod rules the ruleset is exactly the input rules and a forward chain that passes only container traffic" || { diff "$tmp/fw-today.nft" "$RULES"; bad "fw no pods changed the ruleset"; }
: >"$CALLS"; fw_run --check >"$tmp/out" 2>&1 && ! grep -q 'nft -f' "$CALLS" && ok "ncs_firewall: without pod rules the forward chain checks clean once applied" || { cat "$tmp/out"; bad "fw no pods check"; }
pods_observe() {
  export NCS_FIREWALL_POD_MODE=observe NCS_FIREWALL_POD_ALLOW="10.10.10.161:5335,10.20.0.0/24,10.10.10.140:5432,10.10.10.161:5335"
  export NCS_FIREWALL_ADMIN_TARGETS="10.20.0.4,10.20.0.3" NCS_FIREWALL_ADMIN_PORTS="53443,5380" NCS_FIREWALL_ADMIN_FROM="10.10.10.140,10.10.10.0/24"
}
pods_off() { unset NCS_FIREWALL_POD_MODE NCS_FIREWALL_POD_ALLOW NCS_FIREWALL_ADMIN_TARGETS NCS_FIREWALL_ADMIN_PORTS NCS_FIREWALL_ADMIN_FROM; }
# the forward chain, alone
fw_forward() { awk '/^  chain forward/ {f=1} f {print} f && /^  }/ {exit}' "$RULES"; }
fw_input() { awk '/^  chain input/ {f=1} f {print} f && /^  }/ {exit}' "$RULES"; }
pods_observe; fw_net; fw_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "fw observe apply"; }
B='{ "ocisaas0", "cni0", "docker0", "br-*", "pterodactyl0" }'
grep -q 'done    ncs_firewall' "$tmp/out" && grep -q 'pod rules in observe mode' "$tmp/out" && grep -qx '# pod-mode: observe' "$RULES" \
  && grep -qx '  counter pod_input {' "$RULES" && grep -qx '  counter pod_private {' "$RULES" && grep -qx '  counter admin_other {' "$RULES" \
  && ! fw_input | grep -qxF "    iifname $B accept" \
  && grep -qxF "    iifname $B udp dport { 53, 67 } accept" "$RULES" && grep -qxF "    iifname $B tcp dport 53 accept" "$RULES" \
  && grep -qxF "    iifname $B limit rate 10/minute log prefix \"nc-pod_input \"" "$RULES" && grep -qxF "    iifname $B counter name pod_input accept" "$RULES" \
  && grep -qx '    type filter hook forward priority 0; policy accept;' "$RULES" \
  && fw_forward | grep -qxF '    ct status dnat ct original ip daddr { 10.20.0.3, 10.20.0.4 } ct original proto-dst { 5380, 53443 } ip saddr { 10.10.10.0/24, 10.10.10.140 } accept' \
  && fw_forward | grep -qxF '    ct status dnat ct original ip daddr { 10.20.0.3, 10.20.0.4 } ct original proto-dst { 5380, 53443 } counter name admin_other accept' \
  && fw_forward | grep -qxF "    iifname $B ip daddr 10.89.0.0/16 accept" \
  && fw_forward | grep -qxF "    iifname $B ip daddr 10.10.10.140 meta l4proto { tcp, udp } th dport 5432 accept" \
  && [ "$(fw_forward | grep -cF 'ip daddr 10.10.10.161 meta l4proto { tcp, udp } th dport 5335 accept')" = 1 ] \
  && fw_forward | grep -qxF "    iifname $B ip daddr 10.20.0.0/24 accept" \
  && fw_forward | grep -qF 'counter name pod_private accept' && fw_forward | grep -qF 'log prefix "nc-pod_private "' \
  && grep -qx '  counter host_route {' "$RULES" && [ "$(grep -Ec 'counter name [a-z_]+ drop' "$RULES")" = 1 ] && [ "$(fw_forward | grep -cw drop)" = 1 ] \
  && fw_forward | grep -qxF '    counter name host_route drop' \
  && ok "ncs_firewall: observe mode adds the pod rules to the forward chain, the counters and the allowed destinations (sorted, once each) and drops no pod traffic" || { cat "$tmp/out"; cat "$RULES"; bad "fw observe render"; }
# the end of the forward chain, in every mode: what comes from or goes to a container bridge passes, the rest (one of the machine's own
# interfaces to another, e.g. the tenant VLAN to the management VLAN) is logged, counted and dropped, after every pod rule
cat >"$tmp/fw-forward-tail" <<TAIL
    iifname $B accept
    oifname $B accept
    limit rate 10/minute log prefix "nc-host_route "
    counter name host_route drop
  }
TAIL
fw_tail() { fw_forward | tail -n 5 | cmp -s - "$tmp/fw-forward-tail"; }
fw_tail && fw_forward | awk '/pod_private accept/ {c=NR} /admin_other accept/ {a=NR} $0 ~ /^    iifname .* accept$/ && $0 !~ /daddr|oifname/ {i=NR} END { exit (a && c && i && a < c && c < i) ? 0 : 1 }' \
  && ok "ncs_firewall: in observe mode pod_private is still counted and accepted while forwarding between the machine's own interfaces is dropped, after the pod rules" || { fw_forward; bad "fw observe host route"; }
# the admin rules come before the pods' own, and the allowed destinations before the catch-all for private ranges
fw_forward | awk '/admin_other accept/ {a=NR} /10.89.0.0\/16 accept/ {p=NR} /th dport 5432 accept/ {l=NR} /pod_private accept/ {c=NR} END { exit (a && p && l && c && a < p && p < l && l < c) ? 0 : 1 }' \
  && ok "ncs_firewall: the forward chain is in order (admin, the pods' own range, what the task allows, then the private ranges)" || { fw_forward; bad "fw forward order"; }
# containers on the machine talking to each other (in on a bridge, out on a bridge) pass, after the admin rules and before the private-range catch-all
fw_forward | awk -v line="    iifname $B oifname $B accept" '$0 == line {x=NR} /admin_other accept/ {a=NR} /pod_private accept/ {c=NR} END { exit (x && a && c && a < x && x < c) ? 0 : 1 }' \
  && ok "ncs_firewall: containers on the machine reaching each other pass, after the admin rules and before the private-range catch-all" || { fw_forward; bad "fw container exemption"; }
: >"$CALLS"; fw_run --check >/dev/null 2>&1 && ok "ncs_firewall: observe rules check clean once applied" || { cat "$RULES"; bad "fw observe check"; }
NCS_FIREWALL_POD_ALLOW="10.10.10.161:5335,10.20.0.0/24,10.10.10.140:5432" fw_run --check >/dev/null 2>&1 && ok "ncs_firewall: the same allow list in another order with duplicates removed is the same ruleset" || bad "fw observe order-insensitive"
NCS_FIREWALL_POD_MODE=enforce fw_run --check >/dev/null 2>&1 && bad "a change of pod mode passed the check" || ok "ncs_firewall: a change of pod mode is drift"
pods_off; fw_run --check >/dev/null 2>&1 && bad "removing the pod rules passed the check" || ok "ncs_firewall: removing the pod rules is drift"
cp "$RULES" "$tmp/fw-observe.nft"
# a real nft, where this machine has one and may use it, checks the rendered rules (the stub above stands in for it otherwise)
real_nft=""; for c in /usr/sbin/nft /sbin/nft /usr/bin/nft; do [ -x "$c" ] && { real_nft="$c"; break; }; done
pods_observe; NCS_FIREWALL_POD_MODE=enforce; fw_net; fw_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "fw enforce apply"; }
grep -qx '# pod-mode: enforce' "$RULES" && grep -qxF "    iifname $B counter name pod_input drop" "$RULES" && fw_forward | grep -qF 'counter name pod_private drop' && fw_forward | grep -qF 'counter name admin_other drop' \
  && fw_forward | grep -qF 'ip saddr { 10.10.10.0/24, 10.10.10.140 } accept' && grep -qx '    type filter hook forward priority 0; policy accept;' "$RULES" \
  && [ "$(sed '/host_route drop/!s/ drop$/ accept/' "$RULES" | grep -v '^# pod-mode' | grep -v 'ct state invalid')" = "$(grep -v '^# pod-mode' "$tmp/fw-observe.nft" | grep -v 'ct state invalid')" ] && fw_tail \
  && ok "ncs_firewall: enforce mode drops what observe mode only counts and is otherwise the same ruleset, with the same host-route drop at the end" || { cat "$RULES"; bad "fw enforce render"; }
if [ -n "$real_nft" ] && "$real_nft" -c -f "$tmp/fw-today.nft" >/dev/null 2>&1; then
  "$real_nft" -c -f "$tmp/fw-observe.nft" && "$real_nft" -c -f "$RULES" && ok "ncs_firewall: $real_nft accepts the observe and enforce rules" || bad "nft rejected the pod rules"
else
  echo "ok - ncs_firewall: SKIPPED checking the pod rules with a real nft (none here, or it may not use netlink; checked by hand on Debian 13's nft 1.1.3)"
fi
# what the task may not say is refused before anything changes: the machine keeps its observe rules
for case in "NCS_FIREWALL_POD_MODE=permissive" "NCS_FIREWALL_POD_ALLOW=10.0.0.1:99999" "NCS_FIREWALL_POD_ALLOW=10.0.0.1:0" "NCS_FIREWALL_POD_ALLOW=10.0.0.256" \
  "NCS_FIREWALL_POD_ALLOW=10.0.0.0/33" "NCS_FIREWALL_POD_ALLOW=10.0.0.1;reboot" "NCS_FIREWALL_POD_ALLOW=*" "NCS_FIREWALL_POD_ALLOW=10.0.0.1,,10.0.0.2" \
  "NCS_FIREWALL_POD_ALLOW=10.0.0.1 accept" "NCS_FIREWALL_POD_ALLOW=$(seq 1 33 | sed 's/^/10.0.0./' | paste -sd, -)" \
  "NCS_FIREWALL_ADMIN_FROM=" "NCS_FIREWALL_ADMIN_TARGETS=10.20.0.0/24" "NCS_FIREWALL_ADMIN_PORTS=1,2,3,4,5,6,7,8,9" "NCS_FIREWALL_ADMIN_PORTS=5380/tcp"; do
  pods_observe; fw_net; cp "$tmp/fw-observe.nft" "$RULES" 2>/dev/null || { mkdir -p "$(dirname "$RULES")"; cp "$tmp/fw-observe.nft" "$RULES"; }
  env "$case" sh "$here/bootstrap.sh" nuit-cloud-firewall >"$tmp/out" 2>&1 && bad "the pod rules $case were accepted"
  grep -q 'MANUAL  ncs_firewall' "$tmp/out" && [ ! -s "$CALLS" ] && cmp -s "$RULES" "$tmp/fw-observe.nft" || { cat "$tmp/out"; cat "$CALLS"; bad "fw hostile $case"; }
done
ok "ncs_firewall: a bad pod mode, address, port, list or admin set (or one that is not all three) changes nothing"
pods_off; fw_net; NCS_FIREWALL_POD_ALLOW=10.0.0.1 fw_run >"$tmp/out" 2>&1 && bad "pod rules without a mode were accepted"
grep -q 'MANUAL  ncs_firewall' "$tmp/out" && [ ! -s "$CALLS" ] && ok "ncs_firewall: pod rules without a mode change nothing" || { cat "$tmp/out"; bad "fw pods without mode"; }
pods_off; NCS_FIREWALL_POD_MODE=observe; export NCS_FIREWALL_POD_MODE; fw_net; fw_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "fw observe minimal"; }
! grep -q 'admin_other accept' "$RULES" && grep -qx '  counter admin_other {' "$RULES" && fw_forward | grep -qF 'counter name pod_private accept' && fw_tail \
  && ok "ncs_firewall: observe mode with nothing allowed and no admin set still counts every private destination and drops host routing" || { cat "$RULES"; bad "fw observe minimal render"; }
: >"$CALLS"; fw_run --check >/dev/null 2>&1 && ! grep -q 'nft -f' "$CALLS" && ok "ncs_firewall: observe rules without admin targets check clean once applied" || { cat "$RULES"; bad "fw observe minimal check"; }
pods_off; unset FW_TCP FW_FROM
export ROOT="$OLDROOT4"
# 15. ncs_housekeeping: a weekly timer, written once and never over someone else's
HKROOT="$tmp/hkroot"; mkdir -p "$HKROOT"; OLDROOT5="$ROOT"; export ROOT="$HKROOT"; : >"$CALLS"
sh -c 'ROOT="'"$ROOT"'"; NC_USER=nuitcloud; JOURNAL_MAX=50M; p() { printf "%s%s\n" "$ROOT" "$1"; }; write_if_missing() { f="$(p "$1")"; if [ -e "$f" ]; then cat >/dev/null; return 0; fi; mkdir -p "$(dirname "$f")"; cat >"$f"; }; systemctl() { echo "systemctl $*" >>"'"$CALLS"'"; }; . "'"$here"'/features.sh"; f_ncs_housekeeping_ok && exit 9; f_ncs_housekeeping_do; f_ncs_housekeeping_ok || exit 8; echo custom >"$(p /etc/systemd/system/nuit-cloud-housekeeping.timer)"; f_ncs_housekeeping_do; exit 0'
rc=$?
[ "$rc" = 0 ] && grep -q 'journalctl --vacuum-size=50M' "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.service" && grep -q 'OnCalendar=weekly' "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.timer" || true
[ "$rc" = 0 ] && grep -q 'systemctl enable --now nuit-cloud-housekeeping.timer' "$CALLS" && grep -qx custom "$ROOT/etc/systemd/system/nuit-cloud-housekeeping.timer" && ok "ncs_housekeeping: the service and weekly timer are written, enabled, and a timer someone else wrote is not overwritten" || { echo "rc=$rc"; cat "$CALLS"; bad "housekeeping"; }
export ROOT="$OLDROOT5"
# 16. ncs_vlan on a minimal Debian: iptables is installed first instead of the feature giving up
# bootstrap.sh puts /usr/sbin and /sbin on PATH, so on a test host that has a real iptables (GitHub's Ubuntu runners) the feature
# finds it and a missing iptables cannot be simulated: the case is skipped there, and runs on macOS and a bare Debian/Ubuntu.
if [ -x /usr/sbin/iptables ] || [ -x /sbin/iptables ] || [ -x /usr/local/sbin/iptables ]; then
  echo "skip - ncs_vlan: iptables is installed when it is missing (this host has a real iptables)"
else
vlan_net; mv "$tmp/bin/iptables" "$tmp/iptables.away"
vlan_run >"$tmp/out" 2>&1 && bad "the VLAN was applied with no iptables"
mv "$tmp/iptables.away" "$tmp/bin/iptables"
grep -q 'apt-get install -y -qq iptables' "$CALLS" && ok "ncs_vlan: iptables is installed when it is missing (a new machine has none before the runtime goes on)" || { cat "$CALLS"; bad "vlan iptables install"; }
fi
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
# 22. the Ingress Service (nuit-cloud-ingress, nuit-cloud/nuit-cloud#184): the agent stages the content; ingress-apply.sh writes it next to
# files that are not ours, tests it with nginx in the container and puts everything back if nginx refuses. Distinctive fake keys are planted
# in the stage and must never appear in an output or a command.
cp -p "$tmp/bin/docker" "$tmp/docker.orig"
stub docker 'echo "docker $*" >>"$CALLS"
case "$1" in
  inspect) [ -e "$STATE/ing-running" ] && echo true || echo false; exit 0;;
  exec) shift; shift; case "$*" in
    "nginx -t") if [ -e "$STATE/ing-bad" ]; then echo "nginx: [emerg] unknown directive \"bogus\" in /etc/nginx/conf.d/nuit-cloud-abc12345-shop.conf:1" >&2; exit 1; fi
      echo "nginx: configuration file /etc/nginx/nginx.conf test is successful" >&2; exit 0;;
    "nginx -s reload") exit 0;;
  esac;;
esac; exit 1'
OLDROOT22="$ROOT"; export ROOT="$tmp/ingroot"; rm -rf "$ROOT" "$STATE"/ing-*
ICONF=/home/nuitcloud/nginx/conf.d; ICERTS=/home/nuitcloud/nginx/certs
mkdir -p "$ROOT$ICONF" "$ROOT$ICERTS/abc12345/shop.example.com" "$ROOT$ICERTS/zzz/old.example.com"
printf 'server { listen 8080; } # someone else\n' >"$ROOT$ICONF/other.conf"
printf '# stale route\n' >"$ROOT$ICONF/nuit-cloud-old.conf"
printf '# default v1\n' >"$ROOT$ICONF/00-nuit-cloud-default.conf"
printf '# shop v1\n' >"$ROOT$ICONF/nuit-cloud-abc12345-shop.conf"
printf 'old chain\n' >"$ROOT$ICERTS/abc12345/shop.example.com/fullchain.pem"; printf 'old key\n' >"$ROOT$ICERTS/abc12345/shop.example.com/privkey.pem"
chmod 640 "$ROOT$ICERTS/abc12345/shop.example.com/"*.pem
printf 'not ours\n' >"$ROOT$ICERTS/abc12345/shop.example.com/README"; printf 'other chain\n' >"$ROOT$ICERTS/zzz/old.example.com/fullchain.pem"
ING_KEY1="FIXTURE-INGRESS-KEY-ONE-6a1f0c93"; ING_KEY2="FIXTURE-INGRESS-KEY-TWO-d2e47b18"; ING_KEY3="FIXTURE-INGRESS-KEY-THREE-81c0aa5e"
IREV1="$(hex64 1)"; IREV2="$(hex64 2)"
# mkstage <dir> <revision> then lines "site <file> <content>" and "cert <dir> <chain> <key>" on stdin: what the agent leaves in its stage
mkstage() {
  rm -rf "$1"; mkdir -m 700 -p "$1/sites" "$1/certs"; printf '%s\n' "$2" >"$1/revision"; : >"$1/sites.list"; : >"$1/certs.list"; k=0
  while IFS=' ' read -r kind a b c; do
    case "$kind" in
      site) printf '%s\n' "$b${c:+ $c}" >"$1/sites/$a"; echo "$a" >>"$1/sites.list";;
      cert) k=$((k + 1)); mkdir -m 700 "$1/certs/$k"; printf -- '-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n' "$b" >"$1/certs/$k/fullchain.pem"
        printf -- '-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----\n' "$c" >"$1/certs/$k/privkey.pem"; echo "$k $a" >>"$1/certs.list";;
    esac
  done
  chmod 600 "$1"/sites/* "$1"/certs/*/* "$1"/*.list "$1/revision" 2>/dev/null || true
}
IST="$tmp/ingstage"
mkstage "$IST" "$IREV1" <<STAGE
site 00-nuit-cloud-default.conf # default v2
site nuit-cloud-abc12345-shop.conf # shop v2
site nuit-cloud-abc12345-new.conf # new route
cert $ICERTS/abc12345/shop.example.com chain-shop $ING_KEY1
cert $ICERTS/abc12345/*.example.com chain-wild $ING_KEY2
STAGE
export NCS_INGRESS_REVISION="$IREV1" NCS_INGRESS_CONF_DIR="$ICONF" NCS_INGRESS_CERT_ROOT="$ICERTS" NCS_INGRESS_CONTAINER=nuit-nginx NCS_INGRESS_STAGE="$IST"
ing_run() { sh "$here/bootstrap.sh" nuit-cloud-ingress "$@"; }
ingout="$tmp/ing-all"; : >"$ingout"
# snap: every path under ROOT with its mode, owner and content checksum (what "exactly as it was" means)
snap() { (cd "$ROOT" && find . -print | LC_ALL=C sort | while IFS= read -r x; do if [ -f "$x" ] && [ ! -L "$x" ]; then printf '%s %s %s\n' "$x" "$(ls -ln "$x" | awk '{print $1" "$3":"$4}')" "$(cksum <"$x")"; else printf '%s %s\n' "$x" "$(ls -ldn "$x" | awk '{print $1" "$3":"$4}')"; fi; done); }
mode() { ls -ln "$ROOT$1" | cut -c1-10; }
before="$(snap)"; : >"$CALLS"
if ing_run --check >"$tmp/out" 2>&1; then bad "ingress: a fresh machine passed the check"; fi; cat "$tmp/out" >>"$ingout"
grep -q 'DRIFT   ncs_ingress' "$tmp/out" && [ "$before" = "$(snap)" ] && ! grep -q 'docker exec' "$CALLS" && ok "ingress: --check on a fresh machine is drift and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "ingress check"; }

# the container is not running: a person's job, and nothing is changed
ing_run >"$tmp/out" 2>&1 && bad "ingress: applied with the container stopped"; cat "$tmp/out" >>"$ingout"
grep -q 'MANUAL  ncs_ingress' "$tmp/out" && grep -q 'nuit-nginx is not running' "$tmp/out" && grep -q 'Nothing was changed' "$tmp/out" && [ "$before" = "$(snap)" ] && ! grep -q 'docker exec' "$CALLS" \
  && ok "ingress: a container that is not running is MANUAL with a plain message, and nothing is changed" || { cat "$tmp/out"; bad "ingress not running"; }

touch "$STATE/ing-running"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out" "$CALLS"; bad "ingress apply failed"; }; cat "$tmp/out" >>"$ingout"
grep -q 'done    ncs_ingress' "$tmp/out" || { cat "$tmp/out"; bad "ingress apply result"; }
for f in 00-nuit-cloud-default.conf nuit-cloud-abc12345-shop.conf nuit-cloud-abc12345-new.conf; do
  cmp -s "$IST/sites/$f" "$ROOT$ICONF/$f" && [ "$(mode "$ICONF/$f")" = "-rw-r--r--" ] || bad "ingress: $f is not the staged file, mode 0644"
done
for pair in "1 shop.example.com" "2 *.example.com"; do
  k="${pair%% *}"; d="$ROOT$ICERTS/abc12345/${pair#* }"
  cmp -s "$IST/certs/$k/fullchain.pem" "$d/fullchain.pem" && cmp -s "$IST/certs/$k/privkey.pem" "$d/privkey.pem" && [ "$(ls -ln "$d/fullchain.pem" | cut -c1-10)" = "-rw-r--r--" ] && [ "$(ls -ln "$d/privkey.pem" | cut -c1-10)" = "-rw-------" ] \
    || bad "ingress: the certificate in $d is not the staged one with modes 0644/0600"
done
ok "ingress: the site files and certificates are exactly the staged bytes, sites and chains 0644, keys 0600 (a wildcard's directory too)"
[ "$(ls -ldn "$ROOT$ICERTS/abc12345/*.example.com" | cut -c1-10)" = "drwxr-xr-x" ] && [ "$(ls -ldn "$ROOT$ICONF/nuit-cloud-abc12345-new.conf" | awk '{print $3":"$4}')" = "$(ls -ldn "$ROOT$ICONF" | awk '{print $3":"$4}')" ] \
  && ok "ingress: a new certificate directory is made 0755, and files are owned like the directory they are in" || bad "ingress: directory mode or ownership"
[ ! -e "$ROOT$ICONF/nuit-cloud-old.conf" ] && grep -q 'someone else' "$ROOT$ICONF/other.conf" && grep -q 'not ours' "$ROOT$ICERTS/abc12345/shop.example.com/README" && grep -q 'other chain' "$ROOT$ICERTS/zzz/old.example.com/fullchain.pem" \
  && ok "ingress: the stale nuit-cloud-old.conf is removed; other.conf, a file beside a certificate and another certificate directory are left alone" || bad "ingress: removal"
[ -z "$(find "$ROOT" -name '*.nuit-new' -o -name '*.nuit-old')" ] && ok "ingress: no temporary file is left behind" || bad "ingress: temporary files left"
[ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && grep -qx "conf-dir=$ICONF" "$ROOT/var/lib/nuit-cloud/ingress/settings" && grep -qx "container=nuit-nginx" "$ROOT/var/lib/nuit-cloud/ingress/settings" \
  && ok "ingress: the revision and the settings are recorded" || bad "ingress: record"
grep -n 'docker exec nuit-nginx nginx -t' "$CALLS" >/dev/null && [ "$(grep -n 'nginx -t' "$CALLS" | head -n1 | cut -d: -f1)" -lt "$(grep -n 'nginx -s reload' "$CALLS" | head -n1 | cut -d: -f1)" ] \
  && ok "ingress: nginx -t runs in the named container before it is reloaded" || { cat "$CALLS"; bad "ingress: nginx order"; }
ing_run --check >"$tmp/out" 2>&1 && grep -q 'ok      ncs_ingress' "$tmp/out" && ok "ingress: --check is clean after the apply" || { cat "$tmp/out"; bad "ingress check after apply"; }
cat "$tmp/out" >>"$ingout"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && ! grep -q applying "$tmp/out" && ! grep -q 'docker exec' "$CALLS" && ok "ingress: a second apply of the same revision changes nothing" || { cat "$tmp/out"; bad "ingress idempotent"; }

# nginx refuses the next revision: every file is put back exactly (the removed one too, the new directory gone) and nothing is recorded
IST2="$tmp/ingstage2"
mkstage "$IST2" "$IREV2" <<STAGE
site 00-nuit-cloud-default.conf # default v3
site nuit-cloud-abc12345-shop.conf bogus;
cert $ICERTS/abc12345/shop.example.com chain-shop-3 $ING_KEY3
cert $ICERTS/def67890/www.example.org chain-www $ING_KEY3
STAGE
export NCS_INGRESS_REVISION="$IREV2" NCS_INGRESS_STAGE="$IST2"
touch "$STATE/ing-bad"; before="$(snap)"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "ingress: a configuration nginx refused was applied"; cat "$tmp/out" >>"$ingout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'nginx -t refused the configuration' "$tmp/out" && grep -q 'unknown directive "bogus"' "$tmp/out" && [ "$before" = "$(snap)" ] && [ -e "$ROOT$ICONF/nuit-cloud-abc12345-new.conf" ] && [ ! -e "$ROOT$ICERTS/def67890" ] \
  && ! grep -q 'nginx -s reload' "$CALLS" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] \
  && ok "ingress: when nginx -t fails every previous file is back exactly (a removed one too), nothing is reloaded or recorded, and nginx's own message is said" || { cat "$tmp/out"; echo "$before" >"$tmp/snap-before"; snap >"$tmp/snap-after"; diff "$tmp/snap-before" "$tmp/snap-after" || true; bad "ingress restore"; }
rm -f "$STATE/ing-bad"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "ingress apply of the second revision"; }; cat "$tmp/out" >>"$ingout"
[ ! -e "$ROOT$ICONF/nuit-cloud-abc12345-new.conf" ] && cmp -s "$IST2/certs/2/privkey.pem" "$ROOT$ICERTS/def67890/www.example.org/privkey.pem" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && grep -q 'nginx -s reload' "$CALLS" \
  && ok "ingress: once nginx accepts it the next revision is applied, reloaded and recorded" || { cat "$tmp/out"; bad "ingress second revision"; }

# what the stage or the task names is checked again: a site file off the pattern, a directory outside certRoot, a stage of another revision,
# a hostile directory or container: FAILED, and nothing on the machine changes
before="$(snap)"; export NCS_INGRESS_REVISION="$IREV1"
mkstage "$IST" "$IREV1" <<STAGE
site ../evil.conf x
STAGE
ing_run >"$tmp/out" 2>&1 && bad "ingress: a site file off the pattern was accepted"; cat "$tmp/out" >>"$ingout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$before" = "$(snap)" ] && ok "ingress: a staged site file name off the pattern is refused before anything is written" || { cat "$tmp/out"; bad "ingress hostile site"; }
for d in /etc/nginx/x "$ICERTS/../../../etc" "$ICERTS"; do
  mkstage "$IST" "$IREV1" <<STAGE
site nuit-cloud-a.conf x
cert $d chain $ING_KEY1
STAGE
  ing_run >"$tmp/out" 2>&1 && bad "ingress: the certificate directory $d was accepted"; cat "$tmp/out" >>"$ingout"
  grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$before" = "$(snap)" ] || { cat "$tmp/out"; bad "ingress hostile cert dir $d"; }
done
ok "ingress: a certificate directory outside certRoot, with .., or certRoot itself is refused before anything is written"
mkstage "$IST" "$IREV2" </dev/null
ing_run >"$tmp/out" 2>&1 && bad "ingress: a stage of another revision was applied"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'not fetched' "$tmp/out" && [ "$before" = "$(snap)" ] && ok "ingress: a stage that is not the signed revision is refused" || { cat "$tmp/out"; bad "ingress stage revision"; }
mkstage "$IST" "$IREV1" </dev/null
for bad_env in "NCS_INGRESS_CONF_DIR=/home/nuitcloud/../etc" "NCS_INGRESS_CONF_DIR=etc/nginx" "NCS_INGRESS_CONF_DIR=$ICERTS/x" "NCS_INGRESS_CONTAINER=nuit;reboot" "NCS_INGRESS_CERT_ROOT=/home/nuit cloud" "NCS_INGRESS_REVISION=$(hex64 A)"; do
  env "$bad_env" sh "$here/bootstrap.sh" nuit-cloud-ingress >"$tmp/out" 2>&1 && bad "ingress: $bad_env was accepted"
  grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$before" = "$(snap)" ] || { cat "$tmp/out"; bad "ingress hostile setting $bad_env"; }
done
ok "ingress: a hostile directory, container or revision in the settings changes nothing"

leaks=0
for s in "$ING_KEY1" "$ING_KEY2" "$ING_KEY3" chain-shop chain-wild; do
  grep -qF -- "$s" "$ingout" "$CALLS" && { echo "leaked: $s" >&2; leaks=1; }
  grep -rqF -- "$s" "$ROOT/var" "$STATE" 2>/dev/null && { echo "recorded: $s" >&2; leaks=1; }
done
[ "$leaks" = 0 ] && ok "ingress: no certificate or key appears in any output, command or record" || bad "ingress leaked key material"
rm -f "$STATE"/ing-*; unset NCS_INGRESS_REVISION NCS_INGRESS_CONF_DIR NCS_INGRESS_CERT_ROOT NCS_INGRESS_CONTAINER NCS_INGRESS_STAGE
cp -p "$tmp/docker.orig" "$tmp/bin/docker"
export ROOT="$OLDROOT22"

# 23. the Ingress Service on a machine with no Docker (container `host`): nginx is Debian's package, installed and kept by the profile. It is
# installed only when missing (without starting on Debian's default site), Debian's default-site link alone is taken out, one include file
# names the site directory, nginx is tested and reloaded on the machine itself, and docker is never asked anything.
for c in systemctl apt-get docker ip ss; do cp -p "$tmp/bin/$c" "$tmp/$c.orig23"; done
stub systemctl 'case "$1" in is-active|is-enabled) ;; *) echo "systemctl $*" >>"$CALLS";; esac
case "$1" in
  enable) touch "$STATE/h-enabled-$2";;
  start) [ -e "$STATE/h-nostart" ] && exit 1; touch "$STATE/h-active-$2";;
  stop) rm -f "$STATE/h-active-$2";;
  restart) touch "$STATE/h-restarted-$2"; [ -e "$STATE/h-norestart" ] && { echo "Job for nginx.service failed." >&2; exit 1; }; touch "$STATE/h-active-$2";;
  reload) [ -e "$STATE/h-active-$2" ] || { echo "nginx.service is not active, cannot reload." >&2; exit 1; };;
  is-active) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/h-active-$1" ]; exit $?;;
  is-enabled) shift; [ "$1" = --quiet ] && shift; [ -e "$STATE/h-enabled-$1" ]; exit $?;;
esac; exit 0'
stub apt-get 'echo "apt-get $* (DEBIAN_FRONTEND=${DEBIAN_FRONTEND:-})" >>"$CALLS"
[ "$1" = install ] || [ "$1" = -y ] || [ "$2" = install ] || exit 0
if [ -e "$STATE/h-apt-fail" ]; then echo "Reading package lists..."; echo "E: Unable to locate package nginx" >&2; exit 100; fi
for a in "$@"; do [ "$a" = nginx ] || continue
  touch "$STATE/pkg-nginx"; [ ! -e "$ROOT/usr/sbin/policy-rc.d" ] || touch "$STATE/policy-rc-seen"
  mkdir -p "$ROOT/etc/nginx/sites-available" "$ROOT/etc/nginx/sites-enabled" "$ROOT/etc/nginx/conf.d"
  printf "server { listen 80 default_server; }\n" >"$ROOT/etc/nginx/sites-available/default"; ln -sf /etc/nginx/sites-available/default "$ROOT/etc/nginx/sites-enabled/default"
done; exit 0'
stub nginx 'echo "nginx $*" >>"$CALLS"; if [ "$1" = -T ]; then cat "$STATE/h-dump" 2>/dev/null; echo "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok" >&2; exit 0; fi; [ "$1" = -t ] || exit 0
if [ -e "$STATE/h-bad" ]; then echo "nginx: [emerg] unknown directive \"bogus\" in /home/nuitcloud/nginx/conf.d/nuit-cloud-abc12345-shop.conf:1" >&2; exit 1; fi
echo "nginx: configuration file /etc/nginx/nginx.conf test is successful" >&2; exit 0'
stub docker 'echo "docker $*" >>"$CALLS"; exit 1'
# the machine's IPv4 addresses and nginx's listening sockets, as the test sets them
stub ip 'echo "ip $*" >>"$CALLS"; [ "$*" = "-o -4 addr show" ] && cat "$STATE/h-addrs" 2>/dev/null; exit 0'
# (ss -p answers nothing with h-nop; after systemctl restart nginx the sockets are h-listen-restart, when the test gives one)
stub ss 'echo "ss $*" >>"$CALLS"; case "$*" in *p*) [ -e "$STATE/h-nop" ] && exit 0;; esac
if [ -e "$STATE/h-restarted-nginx" ] && [ -e "$STATE/h-listen-restart" ]; then cat "$STATE/h-listen-restart"; else cat "$STATE/h-listen" 2>/dev/null; fi; exit 0'
OLDROOT23="$ROOT"; export ROOT="$tmp/hostroot"; rm -rf "$ROOT" "$STATE"/h-* "$STATE"/pkg-nginx "$STATE"/policy-rc-seen; mkdir -p "$ROOT/etc"
INC="$ROOT/etc/nginx/conf.d/nuit-cloud-ingress.conf"; DEFSITE="$ROOT/etc/nginx/sites-enabled/default"
want_inc="$(printf '%s\n' "# Managed by the nuit-cloud agent (the ncs_ingress feature): the Ingress Service's site files. Changes made here are replaced." "include $ICONF/*.conf;")"
IST3="$tmp/hoststage"
mkstage "$IST3" "$IREV1" <<STAGE
site 00-nuit-cloud-default.conf # default h1
site nuit-cloud-abc12345-shop.conf # shop h1
cert $ICERTS/abc12345/shop.example.com chain-shop $ING_KEY1
STAGE
export NCS_INGRESS_REVISION="$IREV1" NCS_INGRESS_CONF_DIR="$ICONF" NCS_INGRESS_CERT_ROOT="$ICERTS" NCS_INGRESS_CONTAINER=host NCS_INGRESS_STAGE="$IST3"
hout="$tmp/host-all"; : >"$hout"
line_of() { grep -n "$1" "$CALLS" | head -n 1 | cut -d: -f1; }

before="$(snap)"; : >"$CALLS"
if ing_run --check >"$tmp/out" 2>&1; then bad "host ingress: a fresh machine passed the check"; fi; cat "$tmp/out" >>"$hout"
grep -q 'DRIFT   ncs_ingress' "$tmp/out" && [ "$before" = "$(snap)" ] && [ ! -s "$CALLS" ] && ok "host ingress: --check on a fresh machine is drift and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress check"; }

# apt cannot install nginx: FAILED with apt's last line, nothing else done, and the install guard is gone again
touch "$STATE/h-apt-fail"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: went ahead without nginx"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'could not install nginx: E: Unable to locate package nginx' "$tmp/out" && [ ! -e "$ROOT/usr/sbin/policy-rc.d" ] && [ ! -e "$ROOT$ICONF" ] && ! grep -q '^systemctl\|^nginx' "$CALLS" \
  && ok "host ingress: an nginx apt cannot install is FAILED with apt's own last line, and nothing else is done" || { cat "$tmp/out" "$CALLS"; bad "host ingress apt failure"; }
rm -f "$STATE/h-apt-fail"

: >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out" "$CALLS"; bad "host ingress apply failed"; }; cat "$tmp/out" >>"$hout"
grep -q 'done    ncs_ingress' "$tmp/out" || { cat "$tmp/out"; bad "host ingress apply result"; }
grep -q 'apt-get update -qq' "$CALLS" && grep -q 'apt-get install -y -qq nginx (DEBIAN_FRONTEND=noninteractive)' "$CALLS" && [ "$(count 'apt-get install')" = 1 ] && [ -e "$STATE/policy-rc-seen" ] && [ ! -e "$ROOT/usr/sbin/policy-rc.d" ] \
  && ok "host ingress: a missing nginx is installed from Debian's package, non-interactively, without the package starting it" || { cat "$CALLS"; bad "host ingress install"; }
[ ! -e "$DEFSITE" ] && [ ! -L "$DEFSITE" ] && [ -f "$ROOT/etc/nginx/sites-available/default" ] && ok "host ingress: Debian's default-site link is taken out (the site itself is left in sites-available)" || bad "host ingress default site"
[ "$(cat "$INC")" = "$want_inc" ] && [ "$(ls -ln "$INC" | cut -c1-10)" = "-rw-r--r--" ] && ok "host ingress: the include file says it is the agent's and names the site directory, and nothing else" || { cat "$INC"; bad "host ingress include"; }
[ "$(ls -ldn "$ROOT$ICONF" | cut -c1-10)" = drwxr-xr-x ] && [ "$(ls -ldn "$ROOT$ICERTS" | cut -c1-10)" = drwxr-xr-x ] && ok "host ingress: the site directory and certificate root are made when missing" || bad "host ingress directories"
cmp -s "$IST3/sites/nuit-cloud-abc12345-shop.conf" "$ROOT$ICONF/nuit-cloud-abc12345-shop.conf" && cmp -s "$IST3/certs/1/privkey.pem" "$ROOT$ICERTS/abc12345/shop.example.com/privkey.pem" \
  && [ "$(ls -ln "$ROOT$ICERTS/abc12345/shop.example.com/privkey.pem" | cut -c1-10)" = "-rw-------" ] && ok "host ingress: the staged site files and certificates are written, the key 0600" || bad "host ingress files"
[ -e "$STATE/h-enabled-nginx" ] && [ -e "$STATE/h-active-nginx" ] && [ "$(line_of 'systemctl enable nginx')" -lt "$(line_of '^nginx -t')" ] && [ "$(line_of 'systemctl start nginx')" -lt "$(line_of '^nginx -t')" ] \
  && [ "$(line_of '^nginx -t')" -lt "$(line_of 'systemctl reload nginx')" ] && ok "host ingress: nginx is enabled and started, then nginx -t runs on the machine before systemctl reloads it" || { cat "$CALLS"; bad "host ingress nginx order"; }
! grep -q '^docker' "$CALLS" && ok "host ingress: docker is never asked anything" || { cat "$CALLS"; bad "host ingress used docker"; }
[ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && grep -qx 'container=host' "$ROOT/var/lib/nuit-cloud/ingress/settings" && ok "host ingress: the revision and container=host are recorded" || bad "host ingress record"
! grep -q '^ip \|^ss ' "$CALLS" && ok "host ingress: site files that name no address are applied as before, without asking for the machine's addresses or sockets" || { cat "$CALLS"; bad "host ingress asked ip or ss"; }
ing_run --check >"$tmp/out" 2>&1 && grep -q 'ok      ncs_ingress' "$tmp/out" && ok "host ingress: --check is clean after the apply" || { cat "$tmp/out"; bad "host ingress check after apply"; }
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && ! grep -q applying "$tmp/out" && [ ! -s "$CALLS" ] && ok "host ingress: a second apply of the same revision changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress idempotent"; }

# each part of the role's base is checked: an edited include file, Debian's link back (relative, as some versions write it), nginx stopped
for drift in include link stopped; do
  case "$drift" in
    include) echo '# edited' >>"$INC";;
    link) ln -s ../sites-available/default "$DEFSITE";;
    stopped) rm -f "$STATE/h-active-nginx";;
  esac
  ing_run --check >"$tmp/out" 2>&1 && bad "host ingress: the check missed $drift"
  : >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress repair $drift"; }; cat "$tmp/out" >>"$hout"
  [ "$(cat "$INC")" = "$want_inc" ] && [ ! -L "$DEFSITE" ] && [ -e "$STATE/h-active-nginx" ] && [ "$(count 'apt-get')" = 0 ] && ! grep -q '^docker' "$CALLS" || { cat "$tmp/out" "$CALLS"; bad "host ingress repair $drift"; }
done
ok "host ingress: an edited include file, Debian's default site back or nginx stopped is drift, and an apply puts it right without installing anything"

# a default site that is not Debian's link is left alone, and said so
ln -s /etc/nginx/sites-available/mine "$DEFSITE"; printf '# mine\n' >"$ROOT/etc/nginx/sites-available/mine"
export NCS_INGRESS_REVISION="$IREV2"; mkstage "$IST3" "$IREV2" <<STAGE
site 00-nuit-cloud-default.conf # default h2
site nuit-cloud-abc12345-shop.conf # shop h2
STAGE
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress with a foreign default link"; }; cat "$tmp/out" >>"$hout"
[ "$(readlink "$DEFSITE")" = /etc/nginx/sites-available/mine ] && grep -q "sites-enabled/default is not Debian's link to its default site, so it was left alone" "$tmp/out" || { cat "$tmp/out"; bad "host ingress foreign link"; }
rm -f "$DEFSITE"; printf 'server { listen 80; }\n' >"$DEFSITE"
export NCS_INGRESS_REVISION="$IREV1"; mkstage "$IST3" "$IREV1" <<STAGE
site 00-nuit-cloud-default.conf # default h3
STAGE
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress with a default file"; }; cat "$tmp/out" >>"$hout"
grep -q 'listen 80;' "$DEFSITE" && [ ! -L "$DEFSITE" ] && grep -q "is not Debian's link to its default site, so it was left alone" "$tmp/out" \
  && ok "host ingress: a sites-enabled/default that is not Debian's link (a file, a link elsewhere) is left alone, with a note" || { cat "$tmp/out"; bad "host ingress default file"; }
rm -f "$DEFSITE"

# nginx -t refuses: the site files and certificates are put back exactly, nothing is reloaded or recorded; the role's base stays
mkstage "$IST3" "$IREV2" <<STAGE
site 00-nuit-cloud-default.conf # default h4
site nuit-cloud-abc12345-shop.conf bogus;
cert $ICERTS/def67890/www.example.org chain-www $ING_KEY3
STAGE
export NCS_INGRESS_REVISION="$IREV2"
echo '# edited' >>"$INC"; ln -s /etc/nginx/sites-available/default "$DEFSITE"
touch "$STATE/h-bad"; sites_before="$(cd "$ROOT/home" && find . -print | LC_ALL=C sort | while IFS= read -r x; do printf '%s %s\n' "$x" "$( [ -f "$x" ] && cksum <"$x")"; done)"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: a configuration nginx refused was applied"; cat "$tmp/out" >>"$hout"
sites_after="$(cd "$ROOT/home" && find . -print | LC_ALL=C sort | while IFS= read -r x; do printf '%s %s\n' "$x" "$( [ -f "$x" ] && cksum <"$x")"; done)"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'nginx -t refused the configuration' "$tmp/out" && grep -q 'unknown directive "bogus"' "$tmp/out" && [ "$sites_before" = "$sites_after" ] \
  && ! grep -q 'systemctl reload\|nginx -s' "$CALLS" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && [ "$(cat "$INC")" = "$want_inc" ] && [ ! -L "$DEFSITE" ] && ! grep -q '^docker' "$CALLS" \
  && ok "host ingress: when nginx -t refuses, the site files and certificates are put back and nothing is reloaded or recorded, while the include file and the default site stay put right" \
  || { cat "$tmp/out" "$CALLS"; printf '%s\n' "$sites_before" >"$tmp/h-before"; printf '%s\n' "$sites_after" >"$tmp/h-after"; diff "$tmp/h-before" "$tmp/h-after" || true; bad "host ingress restore"; }
rm -f "$STATE/h-bad"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress apply after a refusal"; }; cat "$tmp/out" >>"$hout"
[ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && [ "$(count 'systemctl reload nginx')" = 1 ] && ok "host ingress: once nginx accepts it, the revision is applied and nginx reloaded" || { cat "$CALLS"; bad "host ingress after refusal"; }

# an nginx that will not start on the files it had is started once nginx -t accepts the new ones; one that will not start at all is FAILED
rm -f "$STATE/h-active-nginx"; export NCS_INGRESS_REVISION="$IREV1"; mkstage "$IST3" "$IREV1" <<STAGE
site 00-nuit-cloud-default.conf # default h5
STAGE
touch "$STATE/h-nostart"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: an nginx that would not start passed"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'nginx accepted the configuration but did not start' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] \
  && ok "host ingress: an nginx that will not start is FAILED plainly, and the revision is not recorded" || { cat "$tmp/out" "$CALLS"; bad "host ingress no start"; }
rm -f "$STATE/h-nostart"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress start"; }; cat "$tmp/out" >>"$hout"
[ -e "$STATE/h-active-nginx" ] && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && ok "host ingress: a stopped nginx is started again" || { cat "$CALLS"; bad "host ingress started"; }

# the site files listen on the Ingress Service's own address (nuit-cloud/nuit-cloud#205): it must be on the machine before anything is
# written (else MANUAL), and after the reload nginx must listen there (else FAILED, not recorded); a reload that "succeeds" is not enough
printf '%s\n' '1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever' \
  '2: eth0    inet 10.31.0.12/24 brd 10.31.0.255 scope global eth0\       valid_lft forever preferred_lft forever' >"$STATE/h-addrs"
printf '%s\n' 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' 'LISTEN 0 511 0.0.0.0:443 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
export NCS_INGRESS_REVISION="$IREV2"; mkstage "$IST3" "$IREV2" <<STAGE
site 00-nuit-cloud-default.conf server { listen 10.20.0.34:80 default_server; listen 10.20.0.34:443 ssl default_server; }
site nuit-cloud-abc12345-shop.conf server { listen 10.20.0.34:443 ssl; }
cert $ICERTS/def67890/www.example.org chain-www $ING_KEY3
STAGE
echo '# edited' >>"$INC"; before="$(snap)"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: applied with its address not on the machine"; cat "$tmp/out" >>"$hout"
grep -q 'MANUAL  ncs_ingress' "$tmp/out" && grep -q 'the Ingress Service address 10.20.0.34 is not on this machine yet (the tenant VLAN apply puts it there)' "$tmp/out" \
  && [ "$before" = "$(snap)" ] && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && ! grep -q '^nginx\|^systemctl\|^apt-get\|^ss ' "$CALLS" \
  && ok "host ingress: an address the site files listen on that is not on the machine is MANUAL, before anything is written, installed or reloaded" \
  || { cat "$tmp/out" "$CALLS"; echo "$before" >"$tmp/snap-before"; snap >"$tmp/snap-after"; diff "$tmp/snap-before" "$tmp/snap-after" || true; bad "host ingress address absent"; }

printf '%s\n' '3: eth0.20    inet 10.20.0.34/24 brd 10.20.0.255 scope global eth0.20\       valid_lft forever preferred_lft forever' >>"$STATE/h-addrs"
printf '%s\n' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded while nginx does not listen on its address"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q 'nginx did not take the new configuration: it does not listen on 10.20.0.34:\(80\|443\); journalctl -u nginx says why' "$tmp/out" \
  && [ "$(line_of 'systemctl reload nginx')" -lt "$(line_of '^ss -Htln')" ] && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] \
  && ok "host ingress: a reload after which nginx does not listen on the address the files name is FAILED, and the revision is not recorded" || { cat "$tmp/out" "$CALLS"; bad "host ingress listen check"; }

# another configuration on the machine still has a listen without an address (ncs-mini-1, nuit-cloud/nuit-cloud#205): nginx listens on its
# address and on every address too, so the Ingress Service answers on the management address. FAILED, not recorded, and the culprits named
# from nginx -T as "<file>: <line>"; nothing else of the configuration is printed, and a wildcard on a port the files do not name is not one
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' 'LISTEN 0 511 [::]:443 [::]:*' \
  'LISTEN 0 511 0.0.0.0:8080 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
cat >"$STATE/h-dump" <<'DUMP'
# configuration file /etc/nginx/nginx.conf:
user www-data;
http {
    # listen 80; in a comment is not a listen
    ssl_certificate_key /etc/nginx/secret-marker-key.pem;
    include /etc/nginx/conf.d/*.conf;
    include /etc/nginx/sites-enabled/*;
}

# configuration file /etc/nginx/sites-enabled/old-site:
server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name secret-marker.example;
}
server { listen 8080; listen *:80; }

# configuration file /etc/nginx/conf.d/legacy.conf:
server {
    listen
        443 ssl http2;
    listen 0.0.0.0:443 ssl;
    listen [::1]:443 ssl;
}

# configuration file /home/nuitcloud/nginx/conf.d/00-nuit-cloud-default.conf:
server { listen 10.20.0.34:80 default_server; listen 10.20.0.34:443 ssl default_server; }
DUMP
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded while nginx also listens on every address"; cat "$tmp/out" >>"$hout"
for want in '  nginx also listens on every address on port 80: another configuration has a listen without an address' \
  '  nginx also listens on every address on port 443: another configuration has a listen without an address' \
  '    LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' '    LISTEN 0 511 [::]:443 [::]:*' \
  '    /etc/nginx/sites-enabled/old-site: listen 80 default_server;' '    /etc/nginx/sites-enabled/old-site: listen [::]:80 default_server;' \
  '    /etc/nginx/sites-enabled/old-site: listen *:80;' '    /etc/nginx/conf.d/legacy.conf: listen 443 ssl http2;' '    /etc/nginx/conf.d/legacy.conf: listen 0.0.0.0:443 ssl;' \
  '    /etc/nginx/conf.d/legacy.conf: listen [::1]:443 ssl;'; do
  grep -qxF -- "$want" "$tmp/out" || { cat "$tmp/out"; bad "host ingress wildcard: missing line: $want"; }
done
grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && grep -qx 'nginx -T' "$CALLS" && [ "$(line_of 'systemctl reload nginx')" -lt "$(line_of '^nginx -T')" ] \
  && ! grep -q 'secret-marker\|8080\|10\.20\.0\.34:\|0\.0\.0\.0:22\|user www-data\|in a comment\|syntax is ok' "$tmp/out" \
  && ok "host ingress: nginx also listening on every address on a port the files name is FAILED, not recorded, and the listen lines on that port not on the files' addresses are named by file, nothing else" \
  || { cat "$tmp/out" "$CALLS"; bad "host ingress wildcard"; }

# the culprits are capped at twenty lines, and an nginx -T that names none says what else it can be
{ echo '# configuration file /etc/nginx/conf.d/many.conf:'; i=0; while [ "$i" -lt 30 ]; do echo "server { listen 80; server_name s$i.example; }"; i=$((i + 1)); done; } >"$STATE/h-dump"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded with thirty wildcard listens"; cat "$tmp/out" >>"$hout"
[ "$(grep -cxF '    /etc/nginx/conf.d/many.conf: listen 80;' "$tmp/out")" = 20 ] && grep -qxF '    ... and 10 more' "$tmp/out" && ! grep -q 'server_name' "$tmp/out" \
  && ok "host ingress: the listen lines named are capped at twenty, with how many more" || { cat "$tmp/out"; bad "host ingress wildcard cap"; }
printf '# configuration file /etc/nginx/nginx.conf:\nhttp { server { server_name x; } }\n' >"$STATE/h-dump"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded with a wildcard and no listen line"; cat "$tmp/out" >>"$hout"
grep -q 'nginx -T names no such listen line' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] \
  && ok "host ingress: a wildcard nginx -T names no listen line for is still FAILED, and says a server block without one listens on port 80" || { cat "$tmp/out"; bad "host ingress wildcard none named"; }
rm -f "$STATE/h-dump"

printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out" "$CALLS"; bad "host ingress apply on its address"; }; cat "$tmp/out" >>"$hout"
grep -q 'done    ncs_ingress' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && cmp -s "$IST3/sites/nuit-cloud-abc12345-shop.conf" "$ROOT$ICONF/nuit-cloud-abc12345-shop.conf" \
  && [ "$(cat "$INC")" = "$want_inc" ] && [ "$(line_of 'systemctl reload nginx')" -lt "$(line_of '^ss -Htln')" ] \
  && ok "host ingress: with the address on the machine and nginx listening on it, the revision is applied and recorded" || { cat "$tmp/out" "$CALLS"; bad "host ingress address present"; }

# the recorded revision alone is not "in step" (nuit-cloud/nuit-cloud#205, ncs-mini-1): the check also looks, once, at where nginx listens
# on the ports the installed site files name. In step, the check is clean and an apply of the same revision changes nothing
: >"$CALLS"; ing_run --check >"$tmp/out" 2>&1 && grep -q 'ok      ncs_ingress' "$tmp/out" && [ "$(count '^ss -Htln')" = 1 ] && ! grep -q '^nginx\|^systemctl [a-z]' "$CALLS" \
  && ok "host ingress: same revision with nginx listening only where the site files say: the check is clean and asks ss once" || { cat "$tmp/out" "$CALLS"; bad "host ingress check in step"; }
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && ! grep -q applying "$tmp/out" && ! grep -q 'reload\|^nginx' "$CALLS" \
  && ok "host ingress: same revision with nginx listening only where the site files say: an apply changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress apply in step"; }

# same revision, but nginx also listens on every address on port 80: the check is drift with a one-line reason and changes nothing; the
# apply writes the same revision again, reloads, and fails naming the culprit (rewriting the files does not take another file's listen away)
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
printf '%s\n' '# configuration file /etc/nginx/sites-enabled/old-site:' 'server { listen 80 default_server; server_name secret-marker.example; }' >"$STATE/h-dump"
rm -f "$ROOT$ICONF/nuit-cloud-abc12345-shop.conf"; before="$(snap)"; : >"$CALLS"
ing_run --check >"$tmp/out" 2>&1 && bad "host ingress: the check passed while nginx listens on every address"; cat "$tmp/out" >>"$hout"
grep -q 'DRIFT   ncs_ingress' "$tmp/out" && grep -qxF '  nginx also listens on every address on port 80' "$tmp/out" && [ "$before" = "$(snap)" ] && ! grep -q '^nginx\|^systemctl [a-z]' "$CALLS" \
  && ok "host ingress: same revision with nginx on every address: the check is drift, says so in one line, and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress check wildcard"; }
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: same revision applied while nginx listens on every address"; cat "$tmp/out" >>"$hout"
grep -q 'applying ncs_ingress' "$tmp/out" && grep -q 'FAILED  ncs_ingress' "$tmp/out" \
  && grep -qxF '  nginx also listens on every address on port 80: another configuration has a listen without an address' "$tmp/out" \
  && grep -qxF '    LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' "$tmp/out" && grep -qxF '    /etc/nginx/sites-enabled/old-site: listen 80 default_server;' "$tmp/out" \
  && cmp -s "$IST3/sites/nuit-cloud-abc12345-shop.conf" "$ROOT$ICONF/nuit-cloud-abc12345-shop.conf" && grep -q '^nginx -t' "$CALLS" && grep -q 'systemctl reload nginx' "$CALLS" \
  && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && ! grep -q 'secret-marker' "$tmp/out" \
  && ok "host ingress: same revision with nginx on every address: the apply runs again (files written, nginx tested and reloaded) and fails naming the culprit" \
  || { cat "$tmp/out" "$CALLS"; bad "host ingress apply wildcard"; }

# same revision, nginx listens on another specific address on a port the files name (10.31.0.13:80 alone, the management address): the same
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 511 10.31.0.13:80 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
printf '%s\n' '# configuration file /etc/nginx/sites-enabled/mgmt:' 'server { listen 10.31.0.13:80; listen 10.20.0.34:80; listen 10.31.0.13:8080; }' >"$STATE/h-dump"
before="$(snap)"; : >"$CALLS"
ing_run --check >"$tmp/out" 2>&1 && bad "host ingress: the check passed while nginx listens on an address the files do not name"; cat "$tmp/out" >>"$hout"
grep -q 'DRIFT   ncs_ingress' "$tmp/out" && grep -qxF '  nginx listens on 10.31.0.13:80, which the site files do not name' "$tmp/out" && [ "$before" = "$(snap)" ] \
  && ok "host ingress: same revision with nginx on another address: the check is drift, says so in one line, and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress check other address"; }
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: same revision applied while nginx listens on another address"; cat "$tmp/out" >>"$hout"
grep -q 'applying ncs_ingress' "$tmp/out" && grep -q 'FAILED  ncs_ingress' "$tmp/out" \
  && grep -qxF '  nginx listens on 10.31.0.13:80, which the site files do not name: another configuration listens there' "$tmp/out" \
  && grep -qxF '    LISTEN 0 511 10.31.0.13:80 0.0.0.0:*' "$tmp/out" && grep -qxF '    /etc/nginx/sites-enabled/mgmt: listen 10.31.0.13:80;' "$tmp/out" \
  && ! grep -q 'listen 10.20.0.34:80;\|8080' "$tmp/out" && grep -q 'systemctl reload nginx' "$CALLS" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] \
  && ok "host ingress: same revision with nginx on another address: the apply runs again and fails naming that listen line, and only it" \
  || { cat "$tmp/out" "$CALLS"; bad "host ingress apply other address"; }

# same revision, nginx no longer listens on an address the files name: the check says which
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
ing_run --check >"$tmp/out" 2>&1 && bad "host ingress: the check passed while nginx does not listen on its address"
grep -q 'DRIFT   ncs_ingress' "$tmp/out" && grep -qxF '  nginx does not listen on 10.20.0.34:443' "$tmp/out" \
  && ok "host ingress: same revision with nginx not listening on an address the files name: the check is drift and says which" || { cat "$tmp/out"; bad "host ingress check missing"; }
rm -f "$STATE/h-dump"

# and once only the files' addresses are listened on again, the same revision is in step
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"
ing_run --check >"$tmp/out" 2>&1 && grep -q 'ok      ncs_ingress' "$tmp/out" && ok "host ingress: with the stray listen gone, the same revision is in step again" || { cat "$tmp/out"; bad "host ingress in step again"; }

# nginx kept its old configuration's sockets (ncs-mini-1, nuit-cloud/nuit-cloud#205): the files on disk all listen on 10.20.0.34, nginx -t
# passes, systemctl reload "succeeds", but the running master still holds 0.0.0.0:80/443 and [::]:80/443 and could not bind the address-
# specific sockets; nginx -T names no wildcard listen line. Only a restart rebinds: the apply restarts nginx once and checks again.
NGX='users:(("nginx",pid=812,fd=6),("nginx",pid=811,fd=6))'
printf '%s\n' "LISTEN 0 511 0.0.0.0:80 0.0.0.0:* $NGX" "LISTEN 0 511 0.0.0.0:443 0.0.0.0:* $NGX" "LISTEN 0 511 [::]:80 [::]:* $NGX" "LISTEN 0 511 [::]:443 [::]:* $NGX" \
  'LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=600,fd=3))' >"$STATE/h-listen"
printf '%s\n' "LISTEN 0 511 10.20.0.34:80 0.0.0.0:* $NGX" "LISTEN 0 511 10.20.0.34:443 0.0.0.0:* $NGX" 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=600,fd=3))' >"$STATE/h-listen-restart"
cat >"$STATE/h-dump" <<'DUMP'
# configuration file /etc/nginx/nginx.conf:
http { include /etc/nginx/conf.d/*.conf; }

# configuration file /home/nuitcloud/nginx/conf.d/00-nuit-cloud-default.conf:
server { listen 10.20.0.34:80 default_server; listen 10.20.0.34:443 ssl default_server; }
DUMP
rm -f "$ROOT/var/lib/nuit-cloud/ingress/revision" "$STATE"/h-restarted-*; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out" "$CALLS"; bad "host ingress: nginx holding its old sockets was not restarted into the new ones"; }; cat "$tmp/out" >>"$hout"
grep -q 'done    ncs_ingress' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && [ "$(count 'systemctl restart nginx')" = 1 ] \
  && grep -qxF '  nginx kept its old listening sockets after the reload (a reload cannot move a port from every address to one address); restarting nginx' "$tmp/out" \
  && grep -q 'nginx reloaded, then restarted (it had kept its old listening sockets)' "$tmp/out" && grep -q '^ss -Htlnp' "$CALLS" \
  && [ "$(line_of 'systemctl reload nginx')" -lt "$(line_of '^nginx -T')" ] && [ "$(line_of '^nginx -T')" -lt "$(line_of 'systemctl restart nginx')" ] \
  && ! grep -q 'also listens on every address' "$tmp/out" \
  && ok "host ingress: nginx still on its old wildcard sockets after the reload, with no culprit in its configuration, is restarted once, then listens where the files say: applied and recorded, with a note" \
  || { cat "$tmp/out" "$CALLS"; bad "host ingress stale sockets restart"; }

# the restart does not help: FAILED with the full message, restarted once only, not recorded
rm -f "$STATE/h-listen-restart" "$STATE"/h-restarted-* "$ROOT/var/lib/nuit-cloud/ingress/revision"; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded while nginx kept wildcard sockets after a restart"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$(count 'systemctl restart nginx')" = 1 ] && [ ! -e "$ROOT/var/lib/nuit-cloud/ingress/revision" ] \
  && grep -qxF '  nginx also listens on every address on port 80: another configuration has a listen without an address' "$tmp/out" \
  && grep -qxF "    LISTEN 0 511 0.0.0.0:80 0.0.0.0:* $NGX" "$tmp/out" && grep -q 'nginx -T names no such listen line' "$tmp/out" \
  && grep -qxF '  nginx was restarted and still does not listen only where the site files say' "$tmp/out" \
  && ok "host ingress: when nginx still holds the wildcard sockets after the restart the apply is FAILED with the full message, nginx was restarted once only, and nothing is recorded" \
  || { cat "$tmp/out" "$CALLS"; bad "host ingress stale sockets restart fails"; }

# a restart that fails is said plainly, and nothing is recorded
touch "$STATE/h-norestart"; rm -f "$STATE"/h-restarted-*; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded after a failed restart"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -qxF '  nginx did not restart: Job for nginx.service failed. (journalctl -u nginx says why)' "$tmp/out" && [ "$(count 'systemctl restart nginx')" = 1 ] \
  && [ ! -e "$ROOT/var/lib/nuit-cloud/ingress/revision" ] && ok "host ingress: a restart that fails is FAILED plainly, and the revision is not recorded" || { cat "$tmp/out" "$CALLS"; bad "host ingress restart fails"; }
rm -f "$STATE/h-norestart"; touch "$STATE/h-active-nginx"

# the configuration names the culprit: a restart would bind the wildcard again, so nginx is not restarted and the culprit is named
printf '%s\n' "LISTEN 0 511 10.20.0.34:80 0.0.0.0:* $NGX" "LISTEN 0 511 10.20.0.34:443 0.0.0.0:* $NGX" "LISTEN 0 511 0.0.0.0:80 0.0.0.0:* $NGX" >"$STATE/h-listen"
printf '%s\n' "LISTEN 0 511 10.20.0.34:443 0.0.0.0:* $NGX" >"$STATE/h-listen-restart"
printf '%s\n' '# configuration file /etc/nginx/sites-enabled/old-site:' 'server { listen 80 default_server; }' >"$STATE/h-dump"
rm -f "$STATE"/h-restarted-*; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded with a culprit in the configuration"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$(count 'systemctl restart')" = 0 ] && ! grep -q 'restarting nginx' "$tmp/out" \
  && grep -qxF '    /etc/nginx/sites-enabled/old-site: listen 80 default_server;' "$tmp/out" && grep -qxF "    LISTEN 0 511 0.0.0.0:80 0.0.0.0:* $NGX" "$tmp/out" \
  && ok "host ingress: a wildcard the configuration on disk asks for is FAILED naming the listen line, and nginx is not restarted" || { cat "$tmp/out" "$CALLS"; bad "host ingress culprit no restart"; }

# a wildcard on the port another program holds: that program is named, nginx is not restarted (it could not take the port either)
printf '%s\n' "LISTEN 0 511 10.20.0.34:443 0.0.0.0:* $NGX" 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("apache2",pid=700,fd=4),("apache2",pid=701,fd=4))' >"$STATE/h-listen"
cat >"$STATE/h-dump" <<'DUMP'
# configuration file /home/nuitcloud/nginx/conf.d/00-nuit-cloud-default.conf:
server { listen 10.20.0.34:80 default_server; listen 10.20.0.34:443 ssl default_server; }
DUMP
rm -f "$STATE"/h-restarted-*; printf '%s\n' "$IREV2" >"$ROOT/var/lib/nuit-cloud/ingress/revision"; : >"$CALLS"
ing_run --check >"$tmp/out" 2>&1 && bad "host ingress: the check passed while apache2 holds port 80"
grep -qxF '  apache2 listens on every address on port 80' "$tmp/out" && ok "host ingress: the check names another program on every address of a port" || { cat "$tmp/out"; bad "host ingress check foreign"; }
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded while apache2 holds port 80"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && [ "$(count 'systemctl restart')" = 0 ] && ! grep -q 'restarting nginx\|nginx also listens' "$tmp/out" \
  && grep -qxF '  apache2 listens on every address on port 80: another program holds the port, so nginx cannot listen there' "$tmp/out" \
  && grep -qxF '    LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:(("apache2",pid=700,fd=4),("apache2",pid=701,fd=4))' "$tmp/out" \
  && ok "host ingress: a wildcard another program holds is named as that program's, FAILED, and nginx is not restarted" || { cat "$tmp/out" "$CALLS"; bad "host ingress foreign wildcard"; }

# ss -p says nothing (no owners known): the sockets come from ss -Htln, spoken of as nginx's, and nginx is not restarted on a guess
touch "$STATE/h-nop"
printf '%s\n' 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' 'LISTEN 0 511 0.0.0.0:443 0.0.0.0:*' >"$STATE/h-listen"
rm -f "$STATE"/h-restarted-*; : >"$CALLS"
ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded with sockets of no known owner"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -q '^ss -Htlnp' "$CALLS" && grep -qx 'ss -Htln' "$CALLS" && [ "$(count 'systemctl restart')" = 0 ] \
  && grep -qxF '  nginx also listens on every address on port 80: another configuration has a listen without an address' "$tmp/out" && grep -qxF '    LISTEN 0 511 0.0.0.0:80 0.0.0.0:*' "$tmp/out" \
  && ok "host ingress: when ss -p names no process the sockets come from ss -Htln, and nginx is not restarted for sockets of no known owner" || { cat "$tmp/out" "$CALLS"; bad "host ingress ss fallback"; }
rm -f "$STATE/h-nop" "$STATE/h-dump" "$STATE/h-listen-restart" "$STATE"/h-restarted-*

# a hostile process name in ss's answer is not run, and is said only as plain characters
printf '%s\n' "LISTEN 0 511 10.20.0.34:80 0.0.0.0:* $NGX" "LISTEN 0 511 10.20.0.34:443 0.0.0.0:* $NGX" 'LISTEN 0 511 0.0.0.0:443 0.0.0.0:* users:(("$(touch /tmp/x);rm",pid=9,fd=4))' >"$STATE/h-listen"
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 && bad "host ingress: recorded with a hostile process on port 443"; cat "$tmp/out" >>"$hout"
grep -q 'FAILED  ncs_ingress' "$tmp/out" && grep -qxF '  __touch__tmp_x__rm listens on every address on port 443: another program holds the port, so nginx cannot listen there' "$tmp/out" \
  && [ "$(count 'systemctl restart')" = 0 ] && ok "host ingress: a process name with shell characters is said with them replaced, and changes nothing" || { cat "$tmp/out" "$CALLS"; bad "host ingress hostile process name"; }
printf '%s\n' 'LISTEN 0 511 10.20.0.34:80 0.0.0.0:*' 'LISTEN 0 511 10.20.0.34:443 0.0.0.0:*' 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:*' >"$STATE/h-listen"

# only a dotted IPv4 address with a port is an address: a commented one, IPv6, a port alone, an impossible or hostile value are not asked for
export NCS_INGRESS_REVISION="$IREV1"; mkstage "$IST3" "$IREV1" <<STAGE
site 00-nuit-cloud-default.conf server { listen 10.20.0.34:80 default_server; } # listen 10.9.9.9:80;
site nuit-cloud-abc12345-shop.conf server { listen 80; listen [::]:443 ssl; listen 999.1.1.1:80; listen 10.9.9.9:99999; listen 10.9.9.9:80\$(touch\${IFS}$STATE/h-pwned); }
STAGE
: >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out" "$CALLS"; bad "host ingress with values that are not addresses"; }; cat "$tmp/out" >>"$hout"
grep -q 'done    ncs_ingress' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV1" ] && [ ! -e "$STATE/h-pwned" ] && ! grep -q '10\.9\.9\.9\|999' "$tmp/out" \
  && ok "host ingress: a commented address, IPv6, a bare port, and an impossible or hostile value are not taken as addresses, and nothing in them is run" || { cat "$tmp/out" "$CALLS"; bad "host ingress strict parse"; }

# an address-free revision again: neither the addresses nor the sockets are asked for
mkstage "$IST3" "$IREV2" <<STAGE
site 00-nuit-cloud-default.conf server { listen 80 default_server; listen [::]:80 default_server; }
STAGE
export NCS_INGRESS_REVISION="$IREV2"; : >"$CALLS"; ing_run >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "host ingress without an address"; }; cat "$tmp/out" >>"$hout"
grep -q 'done    ncs_ingress' "$tmp/out" && [ "$(cat "$ROOT/var/lib/nuit-cloud/ingress/revision")" = "$IREV2" ] && ! grep -q '^ip \|^ss ' "$CALLS" \
  && ok "host ingress: listen lines without an address (today's form) are applied as before" || { cat "$tmp/out" "$CALLS"; bad "host ingress no address"; }
: >"$CALLS"; ing_run --check >"$tmp/out" 2>&1 && grep -q 'ok      ncs_ingress' "$tmp/out" && ! grep -q '^ip \|^ss \|^nginx' "$CALLS" \
  && ok "host ingress: with listen lines without an address, the check is the recorded revision as before, and asks no sockets" || { cat "$tmp/out" "$CALLS"; bad "host ingress no address check"; }

# only the exact word is the machine's nginx: `Host` is a container name like any other (and not running here, so a person's job)
: >"$CALLS"; env NCS_INGRESS_CONTAINER=Host sh "$here/bootstrap.sh" nuit-cloud-ingress >"$tmp/out" 2>&1 && bad "host ingress: Host was taken for host"
grep -q 'MANUAL  ncs_ingress' "$tmp/out" && grep -q 'docker inspect' "$CALLS" && ! grep -q 'apt-get\|^nginx\|^systemctl' "$CALLS" && ok "host ingress: only the exact word host means the machine's nginx" || { cat "$tmp/out" "$CALLS"; bad "host ingress word"; }

leaks=0
for s in "$ING_KEY1" "$ING_KEY3" chain-shop chain-www; do
  grep -qF -- "$s" "$hout" "$CALLS" && { echo "leaked: $s" >&2; leaks=1; }
  grep -rqF -- "$s" "$ROOT/var" "$ROOT/etc" "$STATE" 2>/dev/null && { echo "recorded: $s" >&2; leaks=1; }
done
[ "$leaks" = 0 ] && ok "host ingress: no certificate or key appears in any output, command or record" || bad "host ingress leaked key material"
rm -f "$STATE"/h-* "$STATE"/pkg-nginx "$STATE"/policy-rc-seen; unset NCS_INGRESS_REVISION NCS_INGRESS_CONF_DIR NCS_INGRESS_CERT_ROOT NCS_INGRESS_CONTAINER NCS_INGRESS_STAGE
for c in systemctl apt-get docker ip ss; do cp -p "$tmp/$c.orig23" "$tmp/bin/$c"; done; rm -f "$tmp/bin/nginx"
export ROOT="$OLDROOT23"
echo "all passed"
