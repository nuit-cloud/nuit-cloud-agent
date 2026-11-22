# Features for bootstrap.sh. Each is a pair: f_<name>_ok (exit 0 if already satisfied, changing nothing) and
# f_<name>_do (make it so; exit 3 = "a person has to do this", with a message). Profiles may set the defaults below.
LOG_MAX_SIZE="${LOG_MAX_SIZE:-10m}"
LOG_MAX_FILE="${LOG_MAX_FILE:-3}"
NGINX_DIR="${NGINX_DIR:-/home/$NC_USER/nginx}"
JOURNAL_MAX="${JOURNAL_MAX:-50M}"

# The user the platform logs in as over SSH. Creating it (and installing the platform's key) is deliberately by hand.
f_user_ok() { id "$NC_USER" >/dev/null 2>&1; }
f_user_do() { echo "  create $NC_USER and install the platform's SSH key (ssh-copy-id) first, then run this again" >&2; return 3; }

# A feature that is about to download something first checks the machine can resolve the host it will use, so a machine with dead resolvers
# is told so plainly (MANUAL, before anything is changed) instead of failing deep inside apt or curl. Call it only on the path that downloads.
net_resolves() { # net_resolves [host]
  _h="${1:-deb.debian.org}"
  if command -v timeout >/dev/null 2>&1; then timeout 8 getent hosts "$_h" >/dev/null 2>&1; else getent hosts "$_h" >/dev/null 2>&1; fi && return 0
  _ns="$(awk '$1=="nameserver" {printf "%s%s", sep, $2; sep=", "}' "$(p /etc/resolv.conf)" 2>/dev/null)"
  echo "  this machine cannot resolve names (its resolvers: ${_ns:-none}): set them first (the platform's Name resolution panel, or the nuit-cloud-resolvers profile)" >&2
  return 3
}

f_docker_ok() { command -v docker >/dev/null 2>&1 && systemctl is-enabled --quiet docker 2>/dev/null; }
f_docker_do() {
  if ! command -v docker >/dev/null 2>&1; then
    # Debian's own package from the machine's apt sources (the platform's package source once it is in use, nuit-cloud/nuit-cloud#129),
    # never a script fetched from the internet and piped into a shell.
    net_resolves || return 3
    apt-get update -qq && apt-get install -y -qq docker.io || return 1
  fi
  systemctl enable --now docker
}

# The platform drives `docker` as the user: the docker group and nothing else (no sudo).
f_docker_group_ok() { id -nG "$NC_USER" 2>/dev/null | tr ' ' '\n' | grep -qx docker; }
f_docker_group_do() { usermod -aG docker "$NC_USER"; }

# Cap every container's log; one chatty service filling the disk is the likeliest outage. Docker applies it only to
# containers created afterwards, so nuit-nginx (stateless: bind mounts) is removed here and recreated by the nginx feature.
f_docker_logs_ok() { grep -q '"max-size"' "$(p /etc/docker/daemon.json)" 2>/dev/null; }
f_docker_logs_do() {
  if [ -e "$(p /etc/docker/daemon.json)" ]; then echo "  /etc/docker/daemon.json exists without a log cap; left alone — add log-opts max-size/max-file yourself" >&2; return 3; fi
  write_if_missing /etc/docker/daemon.json <<JSON
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "$LOG_MAX_SIZE", "max-file": "$LOG_MAX_FILE" }
}
JSON
  systemctl restart docker
  docker rm -f nuit-nginx >/dev/null 2>&1 || true
}

f_updates_ok() { dpkg -s unattended-upgrades >/dev/null 2>&1 && [ -e "$(p /etc/apt/apt.conf.d/20auto-upgrades)" ]; }
f_updates_do() {
  dpkg -s unattended-upgrades >/dev/null 2>&1 || { net_resolves || return 3; apt-get update -qq && apt-get install -y -qq unattended-upgrades; }
  printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' | write_if_missing /etc/apt/apt.conf.d/20auto-upgrades
}

# Certificate checks and token expiry depend on the clock. A clock service that is *running* is not enough: one that cannot reach a time server leaves the
# clock wherever it was (a fresh install's was hours out, and the agent refused every task as expired), so where timedatectl can say, the check is that
# the clock is synchronised, and applying waits a little for it to be.
f_timesync_running() { systemctl is-active --quiet systemd-timesyncd || systemctl is-active --quiet chrony || systemctl is-active --quiet ntpsec; }
f_timesync_synced() { [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = yes ]; }
f_timesync_ok() { f_timesync_running && { ! command -v timedatectl >/dev/null 2>&1 || f_timesync_synced; }; }
f_timesync_do() {
  if ! systemctl is-active --quiet chrony && ! systemctl is-active --quiet ntpsec; then
    dpkg -s systemd-timesyncd >/dev/null 2>&1 || { net_resolves || return 3; apt-get install -y -qq systemd-timesyncd; }
    systemctl enable systemd-timesyncd >/dev/null 2>&1 || true
    systemctl restart systemd-timesyncd >/dev/null 2>&1 || true
  fi
  timedatectl set-ntp true 2>/dev/null || true
  command -v timedatectl >/dev/null 2>&1 || return 0
  _ts=0
  while [ "$_ts" -lt "${TIMESYNC_WAIT_SECONDS:-30}" ]; do f_timesync_synced && return 0; _ts=$((_ts + 1)); sleep 1; done
  echo "  the clock service is running but the clock is not synchronising (can this machine reach a time server on UDP port 123?)" >&2
  return 1
}

# Unused images cleared weekly by a systemd timer (a minimal Debian has no cron). Running containers' images are kept.
f_prune_ok() { [ -e "$(p /etc/systemd/system/nuit-docker-prune.timer)" ]; }
f_prune_do() {
  write_if_missing /etc/systemd/system/nuit-docker-prune.service <<'UNIT'
[Unit]
Description=Remove unused Docker images older than a week

[Service]
Type=oneshot
ExecStart=/usr/bin/docker image prune --all --force --filter until=168h
UNIT
  write_if_missing /etc/systemd/system/nuit-docker-prune.timer <<'UNIT'
[Unit]
Description=Weekly removal of unused Docker images

[Timer]
OnCalendar=weekly
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now nuit-docker-prune.timer
}

# nginx in Docker on 80/443, config and certificates managed by the platform. certs is mounted at the SAME path inside
# the container because the platform writes host paths into the nginx config.
f_nginx_ok() { docker inspect nuit-nginx >/dev/null 2>&1; }
f_nginx_do() {
  install -d -o "$NC_USER" -g "$NC_USER" "$NGINX_DIR" "$NGINX_DIR/conf.d" "$NGINX_DIR/certs"
  docker run -d --name nuit-nginx -p 80:80 -p 443:443 \
    -v "$NGINX_DIR/conf.d:/etc/nginx/conf.d" -v "$NGINX_DIR/certs:$NGINX_DIR/certs" \
    --restart unless-stopped nginx:alpine
}

# ---- Raspberry Pi ----------------------------------------------------------------------------------------------

# Boot files moved to /boot/firmware in Bookworm.
boot_dir() { if [ -e "$(p /boot/firmware/cmdline.txt)" ]; then p /boot/firmware; else p /boot; fi; }

# 64-bit OS: many images (Pi-hole, Unbound, Technitium, the dyndns probe) are arm64-first, and a 32-bit userland is
# no help to the Pi 3's 1 GB. The OS cannot be switched in place, so this is for a person.
f_pi_arch_ok() { [ "$(uname -m)" = aarch64 ]; }
f_pi_arch_do() { echo "  this is a $(uname -m) system; reinstall Raspberry Pi OS Lite (64-bit) — it cannot be switched in place" >&2; return 3; }

# Docker's memory limits (a service's limits.memoryMb) need the memory cgroup, off by default on Raspberry Pi OS.
f_pi_cgroup_ok() { grep -q 'cgroup_enable=memory' "$(boot_dir)/cmdline.txt" 2>/dev/null && grep -q 'cgroup_memory=1' "$(boot_dir)/cmdline.txt"; }
f_pi_cgroup_do() {
  f="$(boot_dir)/cmdline.txt"; [ -e "$f" ] || { echo "  no cmdline.txt found" >&2; return 1; }
  rewrite "$f" sh -c 'printf "%s cgroup_enable=memory cgroup_memory=1\n" "$(tr -d "\n" <"$1")"' _ "$f"
  REBOOT=1
}

# Headless: give the GPU the minimum so the 1 GB goes to containers.
f_pi_gpumem_ok() { grep -q '^gpu_mem=' "$(boot_dir)/config.txt" 2>/dev/null; }
f_pi_gpumem_do() {
  f="$(boot_dir)/config.txt"; [ -e "$f" ] || { echo "  no config.txt found" >&2; return 1; }
  rewrite "$f" sh -c 'cat "$1"; printf "\n# nuit-cloud: headless\ngpu_mem=16\n"' _ "$f"
  REBOOT=1
}

# Swap in compressed RAM, not on the SD card (slow, and swapping wears the card out). Replaces the SD swap file.
f_pi_zram_ok() { dpkg -s zram-tools >/dev/null 2>&1 && ! systemctl is-enabled --quiet dphys-swapfile 2>/dev/null; }
f_pi_zram_do() {
  dpkg -s zram-tools >/dev/null 2>&1 || { net_resolves || return 3; apt-get update -qq && apt-get install -y -qq zram-tools; }
  printf 'ALGO=zstd\nPERCENT=50\n' | write_if_missing /etc/default/zramswap
  systemctl disable --now dphys-swapfile 2>/dev/null || true
  systemctl enable --now zramswap
}

# Fewer SD-card writes and a bounded journal.
f_pi_journal_ok() { [ -e "$(p /etc/systemd/journald.conf.d/nuit-cloud.conf)" ]; }
f_pi_journal_do() {
  printf '[Journal]\nSystemMaxUse=%s\nSyncIntervalSec=5m\n' "$JOURNAL_MAX" | write_if_missing /etc/systemd/journald.conf.d/nuit-cloud.conf
  systemctl restart systemd-journald
}

# A hung unattended Pi reboots itself (the SoC has a hardware watchdog).
f_pi_watchdog_ok() { [ -e "$(p /etc/systemd/system.conf.d/nuit-watchdog.conf)" ]; }
f_pi_watchdog_do() {
  printf '[Manager]\nRuntimeWatchdogSec=15\nRebootWatchdogSec=2min\n' | write_if_missing /etc/systemd/system.conf.d/nuit-watchdog.conf
  systemctl daemon-reexec
}

summary() {
  echo "Clock synchronised: $(timedatectl show -p NTPSynchronized --value 2>/dev/null || echo unknown)"
  echo "Still to do by hand: open 80 and 443 to the internet if this serves routes (firewall / router / cloud security group)."
  if command -v vcgencmd >/dev/null 2>&1; then echo "Power/thermal flags (0x0 is healthy): $(vcgencmd get_throttled 2>/dev/null)"; fi
}

# ─── Nuit Cloud Server (a machine the platform controls; see CONTEXT.md and ADR 0001) ──────────────────────────────────
# These are conservative on purpose: anything that could lock a person out of the machine (SSH settings) or needs a person
# at the hardware (a cable, the BIOS) is reported MANUAL and never changed behind their back.

# What the agent and the runtime need on a minimal Debian: a way to fetch things over TLS, and the network tools.
f_ncs_packages_ok() { dpkg -s ca-certificates >/dev/null 2>&1 && dpkg -s curl >/dev/null 2>&1 && dpkg -s iproute2 >/dev/null 2>&1 && dpkg -s iputils-arping >/dev/null 2>&1; }
f_ncs_packages_do() { net_resolves || return 3; apt-get update -qq && apt-get install -y -qq ca-certificates curl iproute2 iputils-arping; }

# Wired by default: at least one physical, non-wireless interface that is up and has an address. A cable is a person's job.
f_ncs_wired_ok() {
  for d in "$(p /sys/class/net)"/*; do
    [ -e "$d" ] || continue
    n="$(basename "$d")"
    case "$n" in lo) continue ;; esac
    [ -e "$d/wireless" ] || [ -e "$d/phy80211" ] && continue          # wireless
    [ -L "$d" ] && case "$(readlink "$d")" in *virtual*) continue ;; esac   # bridges, veths, tunnels
    [ "$(cat "$d/operstate" 2>/dev/null)" = up ] && return 0
  done
  return 1
}
f_ncs_wired_do() { echo "  no wired connection is up: connect a cable (or record a decision to allow wireless in the portal), then run this again" >&2; return 3; }

# Root over SSH with a password, or password logins at all, are left to a person: changing sshd before key login is proven
# can lock everyone out. Satisfied once neither is allowed.
sshd_effective() { # sshd_effective <keyword>: the value sshd would use, or empty
  if command -v sshd >/dev/null 2>&1; then sshd -T 2>/dev/null | awk -v k="$1" 'tolower($1)==k {print tolower($2); exit}'; fi
}
f_ncs_ssh_ok() {
  root="$(sshd_effective permitrootlogin)"; pass="$(sshd_effective passwordauthentication)"
  [ -n "$root" ] && [ "$root" != yes ] && [ "$pass" = no ]
}
f_ncs_ssh_do() {
  echo "  set PermitRootLogin no (or prohibit-password) and PasswordAuthentication no in /etc/ssh/sshd_config.d/, AFTER you have shown that key login as $NC_USER works" >&2
  return 3
}

# A bounded journal on every server (the Pi profile's is the same file).
f_journal_ok() { [ -e "$(p /etc/systemd/journald.conf.d/nuit-cloud.conf)" ]; }
f_journal_do() {
  printf '[Journal]\nSystemMaxUse=%s\n' "$JOURNAL_MAX" | write_if_missing /etc/systemd/journald.conf.d/nuit-cloud.conf
  systemctl restart systemd-journald
}

# The Nuit Container Service runs each container as a microVM: it needs /dev/kvm. If the CPU supports it and the module is
# simply not loaded, load it; if virtualisation is off in the firmware, only a person can turn it on.
f_kvm_ok() { [ -e "$(p /dev/kvm)" ]; }
f_kvm_do() {
  modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
  [ -e "$(p /dev/kvm)" ] && return 0
  echo "  no /dev/kvm: turn on Intel VT-x / AMD-V in the BIOS/UEFI (or enable nested virtualisation on the host), then run this again" >&2
  return 3
}

# cgroup v2 (the unified hierarchy): the runtime enforces each tenant's limits through it. The default on current Debian.
f_cgroup2_ok() { [ -e "$(p /sys/fs/cgroup/cgroup.controllers)" ]; }
f_cgroup2_do() { echo "  boot with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1 on the kernel command line), then reboot" >&2; return 3; }

# The Nuit Container Service runtime (a dedicated containerd service beside any Docker, the shim, the CNI bridge, the signed
# guest artifacts), installed from the oci-saas bundle. The agent hands this feature what it needs when the platform asks for a
# profile-apply, already downloaded and checked against the SHA-256 in the signed task:
#   NCS_RUNTIME_BUNDLE            the bundle tarball (oci-saas-bundle.tar.gz)
#   NCS_RUNTIME_BUNDLE_SHA256     its SHA-256 (recorded on the machine, so a different pinned bundle shows as drift)
#   NCS_RUNTIME_ROOT_FINGERPRINT  the fingerprint of the root key the bundle's keys must chain to. artifacts.pub comes from the
#                                 bundle, so checking the guest artifacts against it alone would be circular: this pins the anchor
#   NCS_CONTAINERD_TARBALL        the pinned official containerd 2.x release (Debian's own package is 1.7)
#   NCS_CRICTL_TARBALL            the pinned crictl release (Debian 13 has no cri-tools package)
# Run by hand without them, the feature says what is missing. The first install gives the node the bundle's initial policy, which
# admits namespace nuit-cloud-platform only; the platform's hourly policy-sync replaces it once the node is a registered pool host.
OCISAAS_ETC="${OCISAAS_ETC:-/etc/oci-saas}"
# Where the pinned containerd 2.x goes when the machine has an older one it must not replace.
OCISAAS_CONTAINERD_DIR="${OCISAAS_CONTAINERD_DIR:-/usr/local/lib/oci-saas/bin}"
# The bundle's smoke test (a pod in a microVM answers HTTP) is kept here so the agent's runtime-smoke task can run it later: root-owned, copied from the verified bundle.
OCISAAS_SMOKE="${OCISAAS_SMOKE:-/usr/local/lib/nuit-cloud/oci-saas-smoke.sh}"
# What "installed" means, in two parts the platform tells apart: the runtime is *running* here (its binary, its service, its socket, its keys),
# and the platform has a *record* of it (the smoke script it keeps, and the SHA-256 of the bundle it came from). A runtime installed by hand
# before the profile existed is running with no record; the first note below says exactly that, and the nuit-container-service-record profile
# writes the record, after comparing what is installed with the pinned bundle, without restarting anything.
# The platform looks for this phrase in the notes of a drifting oci_saas_runtime (lib/src/commissioning.ts in nuit-cloud/nuit-cloud-servers).
OCISAAS_UNRECORDED_NOTE='installed and running, but not recorded'
ocisaas_running() {
  [ -x "$(p /usr/local/bin/crictl-ocisaas)" ] && systemctl is-active --quiet containerd-ocisaas \
    && [ -e "$(p /run/oci-saas/containerd.sock)" ] \
    && [ -s "$(p "$OCISAAS_ETC/artifacts.pub")" ] && [ -s "$(p "$OCISAAS_ETC/policy.pub")" ]
}
ocisaas_recorded() {
  [ -x "$(p "$OCISAAS_SMOKE")" ] || return 1
  # when the platform names a bundle, the one recorded must be it (an upgrade then shows as drift)
  [ -z "${NCS_RUNTIME_BUNDLE_SHA256:-}" ] || [ "$(cat "$(p "$OCISAAS_ETC/installed-bundle")" 2>/dev/null)" = "$NCS_RUNTIME_BUNDLE_SHA256" ]
}
f_oci_saas_runtime_ok() {
  if ocisaas_running; then
    ocisaas_recorded && return 0
    missing=
    [ -x "$(p "$OCISAAS_SMOKE")" ] || missing="the smoke-test script ($OCISAAS_SMOKE)"
    if [ ! -e "$(p "$OCISAAS_ETC/installed-bundle")" ]; then
      missing="${missing:+$missing, }the record of which bundle it came from ($OCISAAS_ETC/installed-bundle)"
      echo "  $OCISAAS_UNRECORDED_NOTE: missing $missing. Nothing needs installing: the nuit-container-service-record profile compares what is here with the pinned bundle and writes the record without restarting anything" >&2
    elif [ -n "$missing" ]; then
      echo "  $OCISAAS_UNRECORDED_NOTE: missing $missing. The nuit-container-service-record profile puts it back without restarting anything" >&2
    else
      echo "  a different bundle is recorded here than the one the platform pins: applying this profile upgrades the runtime (and restarts it)" >&2
    fi
    return 1
  fi
  miss=
  [ -x "$(p /usr/local/bin/crictl-ocisaas)" ] || miss="$miss crictl-ocisaas"
  systemctl is-active --quiet containerd-ocisaas || miss="$miss containerd-ocisaas(service not active)"
  [ -e "$(p /run/oci-saas/containerd.sock)" ] || miss="$miss the-runtime-socket"
  { [ -s "$(p "$OCISAAS_ETC/artifacts.pub")" ] && [ -s "$(p "$OCISAAS_ETC/policy.pub")" ]; } || miss="$miss the-key-files"
  echo "  not installed, or not running here; missing:$miss" >&2
  return 1
}

# The fingerprint of the root key in a keys file (`root <hex>`): the SHA-256 of the key's bytes. Nothing at all (and no output) when it cannot be
# worked out: a missing tool must never look like a different key, so the empty input's hash is refused too. perl is part of Debian's base system
# (xxd is not: it comes with vim-common, and a machine installed by hand may lack it); xxd is the fallback.
root_fingerprint_of() { # root_fingerprint_of <artifacts.pub>
  hex="$(awk '$1=="root" {print $2; exit}' "$1")"
  [ -n "$hex" ] || return 1
  if command -v perl >/dev/null 2>&1; then got="$(printf '%s' "$hex" | perl -ne 'print pack("H*", $_)' | sha256sum | awk '{print $1}')"
  elif command -v xxd >/dev/null 2>&1; then got="$(printf '%s' "$hex" | xxd -r -p | sha256sum | awk '{print $1}')"
  else return 1; fi
  [ -n "$got" ] && [ "$got" != e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 ] && printf '%s\n' "$got"
}

f_oci_saas_runtime_do() {
  if [ -z "${NCS_RUNTIME_BUNDLE:-}" ] || [ -z "${NCS_CONTAINERD_TARBALL:-}" ] || [ -z "${NCS_CRICTL_TARBALL:-}" ] || [ -z "${NCS_RUNTIME_ROOT_FINGERPRINT:-}" ]; then
    echo "  the runtime comes from the oci-saas bundle: ask for this profile from the platform, which gives the agent the bundle, containerd and the root fingerprint to check it against" >&2; return 3
  fi
  [ -f "$NCS_RUNTIME_BUNDLE" ] && [ -f "$NCS_CONTAINERD_TARBALL" ] || { echo "  the bundle or the containerd tarball is not where the agent said it put it" >&2; return 1; }
  [ -e "$(p /dev/kvm)" ] || { echo "  /dev/kvm is missing: the kvm feature comes first" >&2; return 3; }
  sv="$(systemctl --version 2>/dev/null | awk 'NR==1 {print $2}')"
  [ "${sv:-0}" -ge 254 ] 2>/dev/null || { echo "  the runtime needs systemd 254 or newer (this is ${sv:-unknown})" >&2; return 3; }

  # The runtime needs containerd 2.x. With none on the machine the pinned release goes to /usr/bin; with 2.x there it is used. An older one
  # there (Docker's, which other services depend on) is left alone: the pinned 2.x goes in a directory of its own and the runtime's unit is
  # pointed at it by a systemd drop-in (which survives the bundle rewriting the unit). The bundle's installer only needs a containerd 2.x first
  # on PATH and an executable /usr/bin/containerd, so nobody has to decide anything and Docker keeps the containerd it has.
  have="$("$(p /usr/bin/containerd)" --version 2>/dev/null || true)"
  dedicated=
  if [ -n "$have" ] && ! printf '%s' "$have" | grep -qE ' v?2\.'; then dedicated=1; fi

  _need=
  for _pk in erofs-utils e2fsprogs iptables containernetworking-plugins xxd python3-minimal; do dpkg -s "$_pk" >/dev/null 2>&1 || _need=1; done
  if [ -n "$_need" ]; then
    net_resolves || return 3
    apt-get update -qq && apt-get install -y -qq erofs-utils e2fsprogs iptables containernetworking-plugins xxd python3-minimal || return 1
  fi

  work="$(mktemp -d)"
  tar -xzf "$NCS_RUNTIME_BUNDLE" -C "$work" || { rm -rf "$work"; echo "  the bundle is not a readable tarball" >&2; return 1; }
  dir="$(find "$work" -mindepth 1 -maxdepth 1 -type d | head -1)"
  [ -n "$dir" ] && [ -x "$dir/packaging/dedicated/install.sh" ] && [ -f "$dir/artifacts.pub" ] && [ -f "$dir/policy.pub" ] \
    || { rm -rf "$work"; echo "  the bundle does not have the layout of an oci-saas bundle" >&2; return 1; }
  got="$(root_fingerprint_of "$dir/artifacts.pub")" || got=
  if [ -z "$got" ]; then
    rm -rf "$work"; echo "  cannot work out the bundle's root key fingerprint here (it needs perl or xxd, and a root key in artifacts.pub): nothing was installed" >&2; return 1
  fi
  if [ "$got" != "$NCS_RUNTIME_ROOT_FINGERPRINT" ]; then
    rm -rf "$work"; echo "  the bundle's keys chain to root $got, not the root the platform pinned: nothing was installed" >&2; return 1
  fi

  bundle_path="$PATH"
  if [ -n "$dedicated" ]; then
    ded="$(p "$OCISAAS_CONTAINERD_DIR")"
    mkdir -p "$ded" "$(p /etc/systemd/system/containerd-ocisaas.service.d)"
    tar -xzf "$NCS_CONTAINERD_TARBALL" -C "$work" bin/containerd || { rm -rf "$work"; echo "  could not unpack containerd" >&2; return 1; }
    install -m 0755 "$work/bin/containerd" "$ded/containerd"
    printf '[Service]\nExecStart=\nExecStart=%s/containerd --config %s/containerd.toml\n' "$OCISAAS_CONTAINERD_DIR" "$OCISAAS_ETC" >"$(p /etc/systemd/system/containerd-ocisaas.service.d/10-dedicated-containerd.conf)"
    echo "  /usr/bin/containerd is $have, which other services use: it is left alone, and the runtime runs the pinned containerd 2.x from $OCISAAS_CONTAINERD_DIR" >&2
    bundle_path="$ded:$PATH"
  elif [ -z "$have" ]; then
    tar -xzf "$NCS_CONTAINERD_TARBALL" -C "$work" bin/containerd || { rm -rf "$work"; echo "  could not unpack containerd" >&2; return 1; }
    install -m 0755 "$work/bin/containerd" "$(p /usr/bin/containerd)"
  fi

  if [ ! -x "$(p /usr/local/bin/crictl)" ] && ! command -v crictl >/dev/null 2>&1; then
    work2="$(mktemp -d)"
    tar -xzf "$NCS_CRICTL_TARBALL" -C "$work2" crictl || { rm -rf "$work" "$work2"; echo "  could not unpack crictl" >&2; return 1; }
    install -m 0755 "$work2/crictl" "$(p /usr/local/bin/crictl)"; rm -rf "$work2"
  fi

  # first install: the bundle's own policy; every later run keeps the policy the platform has published and restarts
  mode="--policy-dir $dir/policy"; restart_after=
  if [ -e "$(p "$OCISAAS_ETC/installed-bundle")" ]; then
    if grep -q -- '--keep-policy' "$dir/packaging/dedicated/install.sh"; then
      mode="--keep-policy --restart"
    else
      # an older bundle's install.sh has no --keep-policy: hand it a copy of the policy files the node has now
      mkdir -p "$work/keep-policy" && cp "$(p /var/lib/oci-saas/policy)"/policy.json* "$work/keep-policy/" \
        || { rm -rf "$work"; echo "  could not read the installed policy to keep it; nothing was changed" >&2; return 1; }
      mode="--policy-dir $work/keep-policy"; restart_after=1
    fi
  fi
  # shellcheck disable=SC2086
  PATH="$bundle_path" "$dir/packaging/dedicated/install.sh" --bin "$dir/bin" --guest "$dir/guest" --artifacts-pub "$dir/artifacts.pub" \
    --policy-pub "$dir/policy.pub" $mode --cni-bin /usr/lib/cni --add-user "$NC_USER" \
    || { rm -rf "$work"; echo "  the bundle's install.sh failed (its output is above)" >&2; return 1; }
  [ -z "$restart_after" ] || systemctl restart containerd-ocisaas   # running pods keep running
  mkdir -p "$(p "$OCISAAS_ETC")" "$(dirname "$(p "$OCISAAS_SMOKE")")"
  install -m 0755 "$dir/packaging/dedicated/smoke.sh" "$(p "$OCISAAS_SMOKE")"
  printf '%s\n' "${NCS_RUNTIME_BUNDLE_SHA256:-unknown}" >"$(p "$OCISAAS_ETC/installed-bundle")"
  rm -rf "$work"
}

# What is installed, compared with the pinned bundle: the first file that differs, or nothing when the install is that bundle. Only what the
# bundle's own install.sh puts down from the bundle itself is compared (the programs, the guest artifacts and the two key files), never the
# policy, which the platform publishes after the install.
ocisaas_mismatch() { # ocisaas_mismatch <unpacked bundle dir>
  for f in "$1"/bin/*; do cmp -s "$f" "$(p "/usr/local/bin/$(basename "$f")")" || { basename "$f"; return; }; done
  for f in "$1"/guest/*; do cmp -s "$f" "$(p "$OCISAAS_ETC/artifacts/$(basename "$f")")" || { echo "artifacts/$(basename "$f")"; return; }; done
  cmp -s "$1/artifacts.pub" "$(p "$OCISAAS_ETC/artifacts.pub")" || { echo artifacts.pub; return; }
  cmp -s "$1/policy.pub" "$(p "$OCISAAS_ETC/policy.pub")" || echo policy.pub
}
f_oci_saas_record_ok() { ocisaas_running && ocisaas_recorded; }
f_oci_saas_record_do() {
  if [ -z "${NCS_RUNTIME_BUNDLE:-}" ] || [ -z "${NCS_RUNTIME_ROOT_FINGERPRINT:-}" ] || [ -z "${NCS_RUNTIME_BUNDLE_SHA256:-}" ]; then
    echo "  recording needs the pinned bundle: ask for this profile from the platform, which gives the agent the bundle and the root fingerprint to check it against" >&2; return 3
  fi
  ocisaas_running || { echo "  the runtime is not installed or not running here: there is nothing to record. Install it with the nuit-container-service profile" >&2; return 3; }
  [ -f "$NCS_RUNTIME_BUNDLE" ] || { echo "  the bundle is not where the agent said it put it" >&2; return 1; }
  work="$(mktemp -d)"
  tar -xzf "$NCS_RUNTIME_BUNDLE" -C "$work" || { rm -rf "$work"; echo "  the bundle is not a readable tarball" >&2; return 1; }
  dir="$(find "$work" -mindepth 1 -maxdepth 1 -type d | head -1)"
  [ -n "$dir" ] && [ -x "$dir/packaging/dedicated/smoke.sh" ] && [ -f "$dir/artifacts.pub" ] && [ -f "$dir/policy.pub" ] \
    || { rm -rf "$work"; echo "  the bundle does not have the layout of an oci-saas bundle" >&2; return 1; }
  got="$(root_fingerprint_of "$dir/artifacts.pub")" || got=
  if [ -z "$got" ]; then
    rm -rf "$work"; echo "  cannot work out the bundle's root key fingerprint here (it needs perl or xxd, and a root key in artifacts.pub): nothing was recorded" >&2; return 1
  fi
  if [ "$got" != "$NCS_RUNTIME_ROOT_FINGERPRINT" ]; then
    rm -rf "$work"; echo "  the bundle's keys chain to root $got, not the root the platform pinned: nothing was recorded" >&2; return 1
  fi
  diff="$(ocisaas_mismatch "$dir")"
  if [ -n "$diff" ]; then
    rm -rf "$work"; echo "  what is installed here is not the pinned bundle ($diff differs): nothing was recorded. Applying the nuit-container-service profile installs the pinned bundle, which restarts the runtime" >&2; return 3
  fi
  mkdir -p "$(p "$OCISAAS_ETC")" "$(dirname "$(p "$OCISAAS_SMOKE")")"
  install -m 0755 "$dir/packaging/dedicated/smoke.sh" "$(p "$OCISAAS_SMOKE")"
  printf '%s\n' "$NCS_RUNTIME_BUNDLE_SHA256" >"$(p "$OCISAAS_ETC/installed-bundle")"
  rm -rf "$work"
}

# Tenants' volumes are disk images under the runtime's directory (default /var/lib/oci-saas) and need room to grow. If the
# filesystem that directory would be on has less than MIN_VOLUMES_GIB free and another real filesystem has far more, the
# directory is put on that one with a bind mount (the runtime sees an ordinary directory). It never touches a directory
# that already has content, and the fstab line is `nofail`, so a mistake cannot stop the machine booting.
VOLUMES_DIR="${VOLUMES_DIR:-/var/lib/oci-saas}"
MIN_VOLUMES_GIB="${MIN_VOLUMES_GIB:-20}"

# Free space in KiB on the filesystem that holds <path>, or on its nearest existing parent.
free_kib_of() {
  d="$1"; while [ ! -e "$d" ] && [ "$d" != / ]; do d="$(dirname "$d")"; done
  df -Pk "$d" 2>/dev/null | awk 'NR==2 {print $4}'
}
# The real filesystems worth putting volumes on, one per line: "<free KiB> <mount point>", roomiest first. Not the boot
# partitions, not virtual or removable filesystems, and not the filesystem the volumes directory is already on.
roomy_mounts() {
  here="$(free_kib_of "$(p "$VOLUMES_DIR")")"
  awk '$3 ~ /^(ext[234]|xfs|btrfs|f2fs)$/ {print $2}' "$(p /proc/mounts)" 2>/dev/null | while read -r m; do
    case "$m" in /boot|/boot/*|/run|/run/*|/snap/*|/var/lib/docker/*) continue ;; esac
    free="$(df -Pk "$(p "$m")" 2>/dev/null | awk 'NR==2 {print $4}')"
    [ -n "$free" ] && [ "$free" != "$here" ] && echo "$free $m"
  done | sort -rn
}
f_ncs_volumes_ok() {
  mountpoint -q "$(p "$VOLUMES_DIR")" 2>/dev/null && return 0              # already its own mount (a bind mount or a volume)
  free="$(free_kib_of "$(p "$VOLUMES_DIR")")"
  [ -n "$free" ] && [ "$free" -ge $((MIN_VOLUMES_GIB * 1024 * 1024)) ]       # or it already has room
}
f_ncs_volumes_do() {
  dest="$(p "$VOLUMES_DIR")"
  if [ -d "$dest" ] && [ -n "$(ls -A "$dest" 2>/dev/null)" ]; then
    echo "  $VOLUMES_DIR already has content and not enough room: move it by hand, then bind-mount the new place onto it" >&2; return 3
  fi
  here="$(free_kib_of "$dest")"
  pick="$(roomy_mounts | awk -v here="${here:-0}" -v min=$((MIN_VOLUMES_GIB * 1024 * 1024)) '$1 >= min && $1 >= here * 4 {print; exit}')"
  if [ -z "$pick" ]; then echo "  no filesystem has $MIN_VOLUMES_GIB GiB and four times the room of the one $VOLUMES_DIR is on: grow one, or add a disk" >&2; return 3; fi
  mnt="${pick#* }"; src="${mnt%/}/oci-saas"
  echo "  putting $VOLUMES_DIR on $mnt ($((${pick%% *} / 1024 / 1024)) GiB free) as $src" >&2
  mkdir -p "$(p "$src")" "$dest"
  chmod 755 "$(p "$src")" "$dest"
  line="$src $VOLUMES_DIR none bind,nofail,x-systemd.requires-mounts-for=$mnt 0 0"
  grep -qsF " $VOLUMES_DIR " "$(p /etc/fstab)" 2>/dev/null || printf '%s\n' "$line" >>"$(p /etc/fstab)"
  systemctl daemon-reload 2>/dev/null || true
  mount "$VOLUMES_DIR"
}

# ─── Where tenants' container logs are written ─────────────────────────────────────────────────────────────────────────────────
# containerd (root) creates each log file 0640 with the group of the directory it lands in, and the platform reads logs as its login
# (`crictl logs` opens the file as that user). Without a shared root that is group-owned by the runtime's group and setgid, the logs land under the
# login's home where root's files are unreadable to it, and the portal's Logs view fails with "permission denied". The connector uses this
# directory when it exists and is writable by the login (container-management's DEFAULT_LOG_ROOT). A pod made before the directory existed keeps its
# old log place: it logs here once it is deployed again.
OCISAAS_LOG_ROOT="${OCISAAS_LOG_ROOT:-/var/lib/oci-saas/logs}"
OCISAAS_GROUP="${OCISAAS_GROUP:-oci-saas}"
f_ncs_log_root_ok() {
  d="$(p "$OCISAAS_LOG_ROOT")"
  [ -d "$d" ] && [ -g "$d" ] || return 1
  [ -n "$ROOT" ] || [ "$(ls -ld "$d" | awk '{print $4}')" = "$OCISAAS_GROUP" ]   # (the group is not checked under a test ROOT: it cannot be changed there)
}
f_ncs_log_root_do() {
  # the runtime's install makes the group and this directory's parent: before it there is nothing to give the logs to
  if [ ! -d "$(p "$OCISAAS_ETC")" ] || { [ -z "$ROOT" ] && ! getent group "$OCISAAS_GROUP" >/dev/null 2>&1; }; then
    echo "  the runtime is not installed yet (it makes the group $OCISAAS_GROUP): this runs after it" >&2; return 3
  fi
  d="$(p "$OCISAAS_LOG_ROOT")"
  mkdir -p "$d"
  chgrp "$OCISAAS_GROUP" "$d"
  chmod 2775 "$d"
}

# ─── The management address the platform allocated (ticket #67) ──────────────────────────────────────────────────────────────
# Turns the machine's DHCP address into the static one the platform allocated for it. This can cut a machine off from the platform,
# so it is built to be undone without anyone's help:
#   * a systemd timer that restores the old configuration is armed BEFORE anything changes, and cancelled only once the machine has
#     reached the platform from the new address; if the agent, the network or the machine dies in between, the timer still fires;
#   * the change is additive while it is tried: the new address is added beside the old one, and the old one is removed only after
#     that works; the interface is never taken down (VLANs and containers on it stay up);
#   * only ifupdown (Debian's default) is handled; NetworkManager, systemd-networkd or an address in another subnet are reported
#     MANUAL and left alone.
# The agent passes NCS_MANAGEMENT_ADDRESS (from the signed task) and NCS_PLATFORM_URL (what to reach to prove it works).
# Another subnet is handled only when it is a tagged VLAN the platform names (nuit-cloud/nuit-cloud#197): the agent then also passes
# NCS_MANAGEMENT_VLAN, NCS_MANAGEMENT_PREFIX and NCS_MANAGEMENT_GATEWAY, and the address moves onto the VLAN's interface (<parent>.<vlan>,
# or vlan<vlan> where that name would not fit: vlan_ifname) together with the machine's default route (addr_vlan_ok and addr_vlan_do).
ADDR_DIR="${ADDR_DIR:-/var/lib/nuit-cloud-agent/address}"
ADDR_ROLLBACK_SECONDS="${ADDR_ROLLBACK_SECONDS:-180}"
ADDR_STATIC_FILE="/etc/network/interfaces.d/nuit-cloud-address"

# addr_net_of <address> <prefix> -> the number of the subnet the address is in (the same for two addresses in one subnet)
addr_net_of() { awk -v ip="$1" -v pl="$2" 'BEGIN {split(ip, o, "."); v = ((o[1] * 256 + o[2]) * 256 + o[3]) * 256 + o[4]; b = 2 ^ (32 - pl); print int(v / b)}'; }
addr_is_ipv4() { printf '%s' "$1" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' && ! printf '%s' "$1" | grep -Eq '(^|\.)(25[6-9]|2[6-9][0-9]|[3-9][0-9][0-9])(\.|$)'; }

# ─── VLAN interface names (nuit-cloud/nuit-cloud#197) ────────────────────────────────────────────────────────────────────────────
# A VLAN interface is <parent>.<id> wherever that fits the kernel's 15 characters (IFNAMSIZ); a parent whose name is already long (a
# Raspberry Pi's enx<mac> is 15) gets vlan<id>, tied to its parent by ifupdown's vlan-raw-device. Whatever looks for "the interface of
# VLAN <id>" asks the kernel (/proc/net/vlan/config), so either name is found, and so are interfaces made by hand.

# vlan_kernel: one "<name> <vlan id> <parent>" line for each VLAN interface the kernel has (nothing when the 8021q module is not loaded,
# which means there are none).
vlan_kernel() {
  _vkf="$(p /proc/net/vlan/config)"
  [ -r "$_vkf" ] || return 0
  awk -F'|' '{for (k = 1; k <= 3; k++) gsub(/[[:space:]]/, "", $k)} NF >= 3 && $1 != "" && $2 ~ /^[0-9]+$/ && $3 != "" {print $1, $2, $3}' "$_vkf"
}

# vlan_ifname <parent> <vlan id> -> the name the agent gives a new VLAN interface: <parent>.<id> when the parent is a plain name and the
# whole fits in 15 characters, vlan<id> otherwise. The dotted name is kept wherever it fits, so machines set up before keep theirs.
vlan_ifname() {
  _vn="$1.$2"
  if printf '%s\n' "$1" | grep -Eq '^[a-zA-Z0-9_-]+$' && [ "${#_vn}" -le 15 ]; then printf '%s\n' "$_vn"; else printf 'vlan%s\n' "$2"; fi
}

# vlan_dev <parent> <vlan id> -> the interface carrying VLAN <id> on <parent>: the one the kernel has, whatever its name, or else the
# name the agent would give it.
vlan_dev() {
  _vd="$(vlan_kernel | awk -v l="$1" -v i="$2" '$3 == l && $2 == i {print $1; exit}')"
  if [ -n "$_vd" ]; then printf '%s\n' "$_vd"; else vlan_ifname "$1" "$2"; fi
}

# vlan_name_free <name> <parent> <vlan id>: no interface has <name> yet, or it is VLAN <id> on <parent> already. An existing interface
# the kernel knows as another VLAN, or a vlan<id> the kernel does not know as a VLAN at all, is someone else's; an existing dotted
# <parent>.<id> the kernel says nothing about is taken as what its name says (as before).
vlan_name_free() {
  ip link show dev "$1" >/dev/null 2>&1 || return 0
  _vk="$(vlan_kernel | awk -v n="$1" '$1 == n {print $2, $3; exit}')"
  if [ -n "$_vk" ]; then [ "$_vk" = "$3 $2" ]; return; fi
  [ "$1" = "$2.$3" ]
}

# The physical interface under <interface>: for a VLAN interface, its parent (from the kernel; else from the vlan-raw-device line of
# its ifupdown stanza; else <if> from a name <if>.<n>); for any other, the interface itself. Once the management address is on a VLAN
# the default route is on that VLAN's interface, and whatever hangs a VLAN of its own off the machine's link (the tenant VLAN) must hang
# it off the parent, beside it.
addr_lower() {
  _lk="$(vlan_kernel | awk -v n="$1" '$1 == n {print $3; exit}')"
  if [ -n "$_lk" ]; then printf '%s\n' "$_lk"; return 0; fi
  _lk="$(for _lf in "$(p /etc/network/interfaces)" "$(p /etc/network/interfaces.d)"/*; do
    [ -f "$_lf" ] && awk -v i="$1" '$1 == "iface" {s = ($2 == i); next} /^(auto|allow-[a-z]+|mapping|source)/ {s = 0} s && $1 == "vlan-raw-device" {print $2; exit}' "$_lf"
  done | head -n 1)"
  if printf '%s\n' "$_lk" | grep -Eq '^[a-zA-Z0-9_-]{1,15}$'; then printf '%s\n' "$_lk"; return 0; fi
  case "$1" in
    ?*.*)
      _n="${1##*.}"; _l="${1%.*}"
      if [ -n "$_l" ] && printf '%s' "$_n" | grep -Eq '^[0-9]{1,4}$'; then printf '%s\n' "$_l"; return 0; fi
      ;;
  esac
  printf '%s\n' "$1"
}

# The interface and gateway of the default route, the machine's own address on it, and the physical interface under it (set A_IF,
# A_GW, A_CUR, A_PREFIX, A_PARENT).
addr_discover() {
  A_IF="$(ip -4 route show default 2>/dev/null | awk '/^default/ {for (i = 1; i < NF; i++) if ($i == "dev") {print $(i + 1); exit}}')"
  A_GW="$(ip -4 route show default 2>/dev/null | awk '/^default/ {for (i = 1; i < NF; i++) if ($i == "via") {print $(i + 1); exit}}')"
  A_CUR=""; A_PREFIX=""; A_PARENT=""
  [ -n "$A_IF" ] || return 1
  A_PARENT="$(addr_lower "$A_IF")"
  cur="$(ip -4 -o addr show dev "$A_IF" scope global 2>/dev/null | awk '{print $4; exit}')"
  A_CUR="${cur%/*}"; A_PREFIX="${cur#*/}"
}

# The ifupdown files that define <interface> (the main file and interfaces.d/*).
addr_files_for() {
  for f in "$(p /etc/network/interfaces)" "$(p /etc/network/interfaces.d)"/*; do
    [ -f "$f" ] && grep -Eq "^[[:space:]]*iface[[:space:]]+$1[[:space:]]" "$f" && printf '%s\n' "$f"
  done
  return 0
}

f_ncs_address_ok() {
  [ -n "${NCS_MANAGEMENT_ADDRESS:-}" ] && addr_discover || return 1
  if [ -n "${NCS_MANAGEMENT_VLAN:-}" ]; then addr_vlan_ok; return; fi
  ip -4 -o addr show dev "$A_IF" 2>/dev/null | awk '{print $4}' | grep -qx "$NCS_MANAGEMENT_ADDRESS/$A_PREFIX" || return 1
  grep -Eq "^[[:space:]]*address[[:space:]]+$NCS_MANAGEMENT_ADDRESS/" "$(p "$ADDR_STATIC_FILE")" 2>/dev/null || return 1
  for f in $(addr_files_for "$A_IF"); do
    [ "$f" = "$(p "$ADDR_STATIC_FILE")" ] && continue
    ! grep -Eq "^[[:space:]]*iface[[:space:]]+$A_IF[[:space:]]+inet[[:space:]]+dhcp" "$f" || return 1
  done
  return 0
}

# Comment out the stanza that defines <interface> (inet) in <file>, keeping every other line. A stanza ends at the next line
# that begins a new one (auto, allow-*, iface, mapping, source, no-*) or at a line with no indentation that is not a comment.
addr_neutralise() { # addr_neutralise <file> <interface>
  awk -v ifc="$2" '
    /^[[:space:]]*iface[[:space:]]+/ { in_stanza = ($2 == ifc && $3 == "inet") }
    /^(auto|allow-[a-z]+|mapping|source|source-directory|no-auto-down|no-scripts)([[:space:]]|$)/ { in_stanza = 0 }
    { if (in_stanza && $0 !~ /^[[:space:]]*#/ && $0 !~ /^[[:space:]]*$/) print "# nuit-cloud: " $0; else print }
  ' "$1"
}

# Everything else on the machine that depends on the network: every IPv4 address (VLAN interfaces, tenant addresses), every route
# and every policy rule. Taken before the change, so the change can be proved not to have lost any of it, and so the rollback
# can put back what was lost.
addr_snapshot() { # addr_snapshot <dir>
  ip -4 -o addr show 2>/dev/null | awk '{print $2, $4}' | sort >"$1/before.addrs"
  ip -4 route show 2>/dev/null | sort >"$1/before.routes"
  ip -4 rule show 2>/dev/null | sed 's/^[0-9]*:[[:space:]]*//' | sort >"$1/before.rules"
}
# What the snapshot had that is gone now, ignoring what the change was meant to replace (the old address and routes that name it).
# Writes one line per loss ("addr <if> <cidr>", "route <route>", "rule <rule>") to <dir>/lost and returns 1 if there is any.
addr_lost() { # addr_lost <dir> <old address or empty> [<old gateway> <old interface>: the old default route, meant to go too]
  ld="$1"; lold="$2"; lgw="${3:-}"; lif="${4:-}"
  ip -4 -o addr show 2>/dev/null | awk '{print $2, $4}' | sort >"$ld/after.addrs"
  ip -4 route show 2>/dev/null | sort >"$ld/after.routes"
  ip -4 rule show 2>/dev/null | sed 's/^[0-9]*:[[:space:]]*//' | sort >"$ld/after.rules"
  : >"$ld/lost"
  for kind in addrs routes rules; do
    comm -23 "$ld/before.$kind" "$ld/after.$kind" | while IFS= read -r line; do
      if [ -n "$lold" ]; then case "$line" in *" $lold/"*|*"src $lold"*|*" $lold "*) continue ;; esac; fi
      if [ -n "$lgw" ]; then case "$line" in "default via $lgw dev $lif"|"default via $lgw dev $lif "*) continue ;; esac; fi
      printf '%s %s\n' "${kind%s}" "$line" >>"$ld/lost"
    done
  done
  # what the change meant to take away (<dir>/meant: fixed strings, one per line; the tenant VLAN's renumbering, #197) is not a loss
  if [ -s "$ld/meant" ]; then grep -vF -f "$ld/meant" "$ld/lost" >"$ld/lost.kept" || true; mv "$ld/lost.kept" "$ld/lost"; fi
  [ ! -s "$ld/lost" ]
}
# Put back what is missing: an address by "ip addr add", a route by "ip route replace". Rules are reported, not restored.
addr_heal() { # addr_heal <dir>
  while IFS= read -r l; do
    case "$l" in
      "addr "*) set -- ${l#addr }; ip addr add "$2" dev "$1" 2>/dev/null || true ;;
      "route "*) ip route replace ${l#route } 2>/dev/null || true ;;
    esac
  done <"$1/lost"
}

f_ncs_address_do() {
  target="${NCS_MANAGEMENT_ADDRESS:-}"
  [ -n "$target" ] || { echo "  no management address was given: allocate one in the portal, then apply this profile from there" >&2; return 3; }
  addr_is_ipv4 "$target" || { echo "  $target is not an IPv4 address" >&2; return 3; }
  command -v ifup >/dev/null 2>&1 && [ -f "$(p /etc/network/interfaces)" ] || { echo "  this machine does not use ifupdown (no /etc/network/interfaces): set $target by hand" >&2; return 3; }
  for m in NetworkManager systemd-networkd; do
    ! systemctl is-active --quiet "$m" 2>/dev/null || { echo "  $m manages the network here; this feature only handles ifupdown: set $target by hand" >&2; return 3; }
  done
  command -v systemd-run >/dev/null 2>&1 || { echo "  systemd-run is needed for the safety timer; set $target by hand" >&2; return 3; }
  addr_discover && [ -n "$A_GW" ] && [ -n "$A_CUR" ] || { echo "  could not find the default route and the address on it" >&2; return 3; }
  addr_is_ipv4 "$A_CUR" && addr_is_ipv4 "$A_GW" || { echo "  the default route is not plain IPv4" >&2; return 3; }
  # another subnet only when the platform names it as a tagged VLAN, with its gateway
  if [ -n "${NCS_MANAGEMENT_VLAN:-}" ]; then addr_vlan_do; return; fi
  # otherwise the same subnet only: another subnet changes the gateway and routes, which a person should see
  [ "$(addr_net_of "$target" "$A_PREFIX")" = "$(addr_net_of "$A_CUR" "$A_PREFIX")" ] || { echo "  $target is not in the machine's current subnet ($A_CUR/$A_PREFIX): change subnets by hand" >&2; return 3; }
  [ "$target" != "$A_GW" ] || { echo "  $target is the gateway's own address" >&2; return 3; }
  files="$(addr_files_for "$A_IF")"
  [ -n "$files" ] || { echo "  $A_IF is not defined in /etc/network/interfaces: it is configured some other way; set $target by hand" >&2; return 3; }
  printf '%s\n' "$files" | grep -q "$ADDR_STATIC_FILE\$" && files="$(printf '%s\n' "$files" | grep -v "$ADDR_STATIC_FILE\$")"
  printf '%s\n' "$A_IF" | grep -Eq '^[a-zA-Z0-9._-]{1,15}$' || { echo "  odd interface name $A_IF" >&2; return 3; }

  added=0
  if [ "$A_CUR" != "$target" ]; then
    # somebody else may already answer for it
    command -v arping >/dev/null 2>&1 || { apt-get install -y -qq iputils-arping >/dev/null 2>&1 || true; }
    if command -v arping >/dev/null 2>&1; then
      arping -D -q -c 2 -w 3 -I "$A_IF" "$target" >/dev/null 2>&1 || { echo "  another device already answers for $target on the network: nothing was changed" >&2; return 1; }
    fi
    added=1
  fi

  # backups, then the timer that undoes everything
  d="$(p "$ADDR_DIR")"; rm -rf "$d/backup"; mkdir -p "$d/backup"; chmod 700 "$d"
  addr_snapshot "$d"
  : >"$d/backup/files"
  for f in $files; do cp -p "$f" "$d/backup/$(printf '%s' "$f" | tr '/' '_')" && printf '%s\n' "$f" >>"$d/backup/files"; done
  dhcp_was=0; for f in $files; do grep -Eq "^[[:space:]]*iface[[:space:]]+$A_IF[[:space:]]+inet[[:space:]]+dhcp" "$f" && dhcp_was=1; done
  cat >"$d/rollback.sh" <<ROLLBACK
#!/bin/sh
# Put the machine's network back as it was before the management address was applied. Run by the safety timer, or at once if the
# change could not be proved to work. Written by the nuit-cloud agent's ncs_address feature.
PATH="\$PATH:/usr/local/sbin:/usr/sbin:/sbin"
echo "nuit-cloud: undoing the management address change"
while IFS= read -r f; do cp -p "$d/backup/\$(printf '%s' "\$f" | tr '/' '_')" "\$f"; done <"$d/backup/files"
rm -f "$(p "$ADDR_STATIC_FILE")"
ip addr add $A_CUR/$A_PREFIX dev $A_IF 2>/dev/null
[ "$added" = 1 ] && ip addr del $target/$A_PREFIX dev $A_IF 2>/dev/null
ip route replace default via $A_GW dev $A_IF 2>/dev/null
[ "$dhcp_was" = 1 ] && ifup --force $A_IF 2>/dev/null
# and whatever else the machine had on its network: VLAN interfaces, tenant addresses, routes (put back if they are missing)
if [ -f "$d/before.addrs" ]; then while read -r ifc cidr; do ip addr add "\$cidr" dev "\$ifc" 2>/dev/null; done <"$d/before.addrs"; fi
if [ -f "$d/before.routes" ]; then while IFS= read -r line; do ip route replace \$line 2>/dev/null; done <"$d/before.routes"; fi
rm -f "$d/armed"
exit 0
ROLLBACK
  chmod 700 "$d/rollback.sh"
  systemd-run --quiet --unit=nuit-cloud-address-rollback --collect --on-active="${ADDR_ROLLBACK_SECONDS}s" /bin/sh "$d/rollback.sh" || { echo "  could not arm the safety timer; nothing was changed" >&2; return 1; }
  : >"$d/armed"

  # 1. live, additive: the new address beside the old one
  if [ "$added" = 1 ]; then
    sysctl -q -w "net.ipv4.conf.$A_IF.promote_secondaries=1" >/dev/null 2>&1 || true
    ip addr add "$target/$A_PREFIX" dev "$A_IF" || { sh "$d/rollback.sh"; echo "  could not add $target to $A_IF" >&2; return 1; }
  fi
  # 2. persistent: the DHCP stanza out, the static one in
  for f in $files; do
    addr_neutralise "$f" "$A_IF" >"$f.nuit-new" && mv "$f.nuit-new" "$f" || { sh "$d/rollback.sh"; echo "  could not rewrite $f" >&2; return 1; }
  done
  mkdir -p "$(dirname "$(p "$ADDR_STATIC_FILE")")"
  cat >"$(p "$ADDR_STATIC_FILE")" <<STATIC
# Written by the nuit-cloud agent: the management address the platform allocated for this machine. The previous configuration is
# commented out where it was, and kept in $ADDR_DIR/backup.
iface $A_IF inet static
    address $target/$A_PREFIX
    gateway $A_GW
STATIC
  # 3. the DHCP client stops, so it does not take the address back or rewrite the routes (the address stays on the interface)
  for pf in "/run/dhclient.$A_IF.pid" "/run/dhcpcd/$A_IF.pid" "/run/udhcpc.$A_IF.pid"; do
    [ -f "$(p "$pf")" ] && kill "$(cat "$(p "$pf")")" 2>/dev/null || true
  done
  ip -4 -o addr show dev "$A_IF" 2>/dev/null | awk '{print $4}' | grep -qx "$target/$A_PREFIX" || ip addr add "$target/$A_PREFIX" dev "$A_IF" 2>/dev/null || true
  ip route replace default via "$A_GW" dev "$A_IF" 2>/dev/null || true
  # 4. the old address goes, only now
  if [ "$added" = 1 ]; then
    ip addr del "$A_CUR/$A_PREFIX" dev "$A_IF" 2>/dev/null || true
    # removing the primary address takes its secondaries with it unless the kernel promotes them (the sysctl above, which a
    # restricted system may refuse): so the new address and the route are asserted again, which is harmless when they survived
    ip -4 -o addr show dev "$A_IF" 2>/dev/null | awk '{print $4}' | grep -qx "$target/$A_PREFIX" || ip addr add "$target/$A_PREFIX" dev "$A_IF" 2>/dev/null || true
    ip route replace default via "$A_GW" dev "$A_IF" 2>/dev/null || true
  fi

  # 5. nothing else may have been lost (VLAN interfaces, tenant addresses, routes): what is missing is put back, and if it still
  #    is, everything is undone
  old_for_check=""; [ "$added" = 1 ] && old_for_check="$A_CUR"
  if ! addr_lost "$d" "$old_for_check"; then
    addr_heal "$d"
    if ! addr_lost "$d" "$old_for_check"; then
      lost_list="$(tr '\n' ';' <"$d/lost")"
      sh "$d/rollback.sh"
      echo "  the change would have lost other network configuration ($lost_list), so the old configuration was put back" >&2
      return 1
    fi
  fi

  # 6. proof: the platform must be reachable from the new address
  proved=0
  if [ -n "${NCS_PLATFORM_URL:-}" ]; then
    for _ in 1 2 3 4 5 6; do
      if curl -fsS --max-time 15 --interface "$target" -o /dev/null "$NCS_PLATFORM_URL/api/agent/download/VERSION" 2>/dev/null; then proved=1; break; fi
      sleep 5
    done
  else
    ping -c 2 -W 3 -I "$target" "$A_GW" >/dev/null 2>&1 && proved=1
  fi
  if [ "$proved" != 1 ]; then
    sh "$d/rollback.sh"
    echo "  the platform could not be reached from $target, so the old configuration was put back" >&2
    return 1
  fi
  systemctl stop nuit-cloud-address-rollback.timer >/dev/null 2>&1 || true
  rm -f "$d/armed"
  echo "  $A_IF now uses $target/$A_PREFIX as a static address (was $A_CUR; the old configuration is in $ADDR_DIR/backup)"
}

# ─── The management address on a tagged VLAN (nuit-cloud/nuit-cloud#197) ─────────────────────────────────────────────────────────
# The platform names a VLAN (NCS_MANAGEMENT_VLAN), the address's prefix and the VLAN's gateway: the address moves onto the VLAN's
# interface (<parent>.<vlan>, or vlan<vlan> when the dotted name would be longer than 15 characters: vlan_ifname), and the machine's
# default route with it (the owner's decision), off the untagged address it had. <parent> is the physical interface under today's
# default route. The parent stays up with no address of its own ("inet manual"): the tenant VLAN and its tenant addresses hang off it.
# The same discipline as above, with a state directory and a safety timer of its own (nuit-cloud-address-vlan-rollback; the tenant
# VLAN's timer, table 105 and mark 0x5 are not touched):
#   * the timer that puts everything back is armed before anything changes;
#   * the VLAN interface and the new address come first, beside the old address; the default route then moves to the new gateway;
#   * proof: the platform answers from the new address and the new gateway answers on the VLAN, or everything is undone at once;
#   * only then does the old address go, and nothing else on the network may have been lost.
ADDR_VLAN_DIR="${ADDR_VLAN_DIR:-$ADDR_DIR/management-vlan}"

# addr_vlan_inputs_ok: the VLAN, prefix and gateway the agent passed are what the platform may send (checked again here: they reach
# files and commands).
addr_vlan_inputs_ok() {
  printf '%s' "${NCS_MANAGEMENT_VLAN:-}" | grep -Eq '^[1-9][0-9]{0,3}$' && [ "$NCS_MANAGEMENT_VLAN" -le 4094 ] || return 1
  printf '%s' "${NCS_MANAGEMENT_PREFIX:-}" | grep -Eq '^[1-3][0-9]$' && [ "$NCS_MANAGEMENT_PREFIX" -ge 16 ] && [ "$NCS_MANAGEMENT_PREFIX" -le 30 ] || return 1
  addr_is_ipv4 "${NCS_MANAGEMENT_ADDRESS:-}" && addr_is_ipv4 "${NCS_MANAGEMENT_GATEWAY:-}" || return 1
  [ "$NCS_MANAGEMENT_GATEWAY" != "$NCS_MANAGEMENT_ADDRESS" ] || return 1
  [ "$(addr_net_of "$NCS_MANAGEMENT_GATEWAY" "$NCS_MANAGEMENT_PREFIX")" = "$(addr_net_of "$NCS_MANAGEMENT_ADDRESS" "$NCS_MANAGEMENT_PREFIX")" ]
}

# addr_stanza <file> <interface> <method>: the file defines `iface <interface> inet <method>` (not commented out)
addr_stanza() { [ -f "$1" ] && awk -v i="$2" -v m="$3" '$1 == "iface" && $2 == i && $3 == "inet" && $4 == m {f = 1} END {exit !f}' "$1"; }

# Moved already: the VLAN interface carries the address, the default route goes by the VLAN's gateway on it, the static file holds the
# stanza, and no other file still asks DHCP for the parent.
addr_vlan_ok() {
  addr_vlan_inputs_ok || return 1
  mvif="$(vlan_dev "$A_PARENT" "$NCS_MANAGEMENT_VLAN")"
  [ "$A_IF" = "$mvif" ] && [ "$A_GW" = "$NCS_MANAGEMENT_GATEWAY" ] || return 1
  ip -4 -o addr show dev "$mvif" 2>/dev/null | awk '{print $4}' | grep -qx "$NCS_MANAGEMENT_ADDRESS/$NCS_MANAGEMENT_PREFIX" || return 1
  sf="$(p "$ADDR_STATIC_FILE")"
  addr_stanza "$sf" "$mvif" static || return 1
  grep -Eq "^[[:space:]]*address[[:space:]]+$NCS_MANAGEMENT_ADDRESS/$NCS_MANAGEMENT_PREFIX([[:space:]]|$)" "$sf" || return 1
  grep -Eq "^[[:space:]]*gateway[[:space:]]+$NCS_MANAGEMENT_GATEWAY([[:space:]]|$)" "$sf" || return 1
  for f in $(addr_files_for "$A_PARENT"); do
    [ "$f" = "$sf" ] && continue
    ! addr_stanza "$f" "$A_PARENT" dhcp || return 1
  done
  return 0
}

addr_vlan_do() {
  # (f_ncs_address_do has checked the address, ifupdown, NetworkManager/systemd-networkd and systemd-run, and run addr_discover)
  addr_vlan_inputs_ok || { echo "  the management VLAN, prefix or gateway is not valid (a VLAN from 1 to 4094, a prefix from 16 to 30, a gateway in the address's subnet that is not the address)" >&2; return 3; }
  vid="$NCS_MANAGEMENT_VLAN"; mpfx="$NCS_MANAGEMENT_PREFIX"; mgw="$NCS_MANAGEMENT_GATEWAY"
  [ -n "${NCS_PLATFORM_URL:-}" ] || { echo "  the platform's address was not passed in, so the move cannot be proved safe" >&2; return 3; }
  printf '%s\n' "$A_IF" | grep -Eq '^[a-zA-Z0-9._-]{1,15}$' || { echo "  odd interface name $A_IF" >&2; return 3; }
  par="$A_PARENT"
  printf '%s\n' "$par" | grep -Eq '^[a-zA-Z0-9_-]{1,15}$' || { echo "  the interface name $par cannot carry a VLAN sub-interface here: set $target by hand" >&2; return 3; }
  mvif="$(vlan_dev "$par" "$vid")"
  printf '%s\n' "$mvif" | grep -Eq '^[a-zA-Z0-9._-]{1,15}$' || { echo "  odd VLAN interface name $mvif: set $target by hand" >&2; return 3; }
  vlan_name_free "$mvif" "$par" "$vid" || { echo "  $mvif exists already and is not VLAN $vid on $par: set $target by hand" >&2; return 3; }
  # from the untagged network, or again onto the same VLAN (a move that did not finish); from one VLAN onto another is a person's job
  [ "$A_IF" = "$par" ] || [ "$A_IF" = "$mvif" ] || { echo "  the default route is on the VLAN interface $A_IF already: moving from one VLAN to another is left to a person" >&2; return 3; }
  [ ! -e "$(p "/etc/network/interfaces.d/nuit-cloud-vlan$vid")" ] || { echo "  VLAN $vid is this machine's tenant VLAN: the management address cannot go on it" >&2; return 3; }
  if [ "$A_IF" = "$par" ] && [ "$(addr_net_of "$A_CUR" "$mpfx")" = "$(addr_net_of "$target" "$mpfx")" ]; then
    echo "  $target/$mpfx overlaps the machine's current network ($A_CUR on $par): that is not a move to another network" >&2; return 3
  fi
  files="$(addr_files_for "$par" | grep -v "$ADDR_STATIC_FILE\$" || true)"
  # (defined only in the agent's own file is fine: a static address applied before, or a move onto this VLAN that did not finish)
  [ -n "$files" ] || grep -Eq "^[[:space:]]*iface[[:space:]]+$par[[:space:]]+inet[[:space:]]" "$(p "$ADDR_STATIC_FILE")" 2>/dev/null || { echo "  $par is not defined in /etc/network/interfaces: it is configured some other way; set $target by hand" >&2; return 3; }
  # what is on the VLAN interface already (from a move that did not finish) may only be this address
  existed=0
  if ip link show dev "$mvif" >/dev/null 2>&1; then
    existed=1
    other="$(ip -4 -o addr show dev "$mvif" 2>/dev/null | awk '{print $4}' | grep -vx "$target/$mpfx" || true)"
    [ -z "$other" ] || { echo "  $mvif exists already and carries $(echo $other): it is configured some other way; set $target by hand" >&2; return 3; }
  fi
  # the address must not be anywhere else on this machine
  if ip -4 -o addr show 2>/dev/null | awk -v v="$mvif" '$2 != v {print $4}' | grep -q "^$target/"; then echo "  $target is already on another interface of this machine: nothing was changed" >&2; return 1; fi
  target_added=1; ip -4 -o addr show dev "$mvif" 2>/dev/null | awk '{print $4}' | grep -qx "$target/$mpfx" && target_added=0
  old_goes=1; [ "$A_IF" = "$mvif" ] && [ "$A_CUR" = "$target" ] && old_goes=0
  created=1; [ "$existed" = 1 ] && created=0
  dhcp_was=0; for f in $files; do addr_stanza "$f" "$par" dhcp && dhcp_was=1; done
  sf="$(p "$ADDR_STATIC_FILE")"
  need_auto=1
  for f in "$(p /etc/network/interfaces)" "$(p /etc/network/interfaces.d)"/*; do
    [ -f "$f" ] && [ "$f" != "$sf" ] || continue
    awk -v i="$par" '$1 == "auto" || $1 == "allow-hotplug" {for (k = 2; k <= NF; k++) if ($k == i) f = 1} END {exit !f}' "$f" && need_auto=0
  done

  # backups, then the timer that undoes everything
  d="$(p "$ADDR_VLAN_DIR")"; rm -rf "$d"; mkdir -p "$d/backup"; chmod 700 "$(p "$ADDR_DIR")" "$d"
  addr_snapshot "$d"
  : >"$d/backup/files"
  for f in $files; do cp -p "$f" "$d/backup/$(printf '%s' "$f" | tr '/' '_')" && printf '%s\n' "$f" >>"$d/backup/files"; done
  static_was=0; [ -f "$sf" ] && { cp -p "$sf" "$d/backup/static"; static_was=1; }
  cat >"$d/rollback.sh" <<ROLLBACK
#!/bin/sh
# Put the machine's network back as it was before the management address moved onto VLAN $vid. Run by the safety timer, or at once if
# the move could not be proved to work. Written by the nuit-cloud agent's ncs_address feature.
PATH="\$PATH:/usr/local/sbin:/usr/sbin:/sbin"
echo "nuit-cloud: undoing the move of the management address onto VLAN $vid"
while IFS= read -r f; do cp -p "$d/backup/\$(printf '%s' "\$f" | tr '/' '_')" "\$f"; done <"$d/backup/files"
if [ "$static_was" = 1 ]; then cp -p "$d/backup/static" "$sf"; else rm -f "$sf"; fi
if [ "$created" = 1 ]; then ip link del dev $mvif 2>/dev/null; elif [ "$target_added" = 1 ]; then ip addr del $target/$mpfx dev $mvif 2>/dev/null; fi
ip addr add $A_CUR/$A_PREFIX dev $A_IF 2>/dev/null
ip route replace default via $A_GW dev $A_IF 2>/dev/null
[ "$dhcp_was" = 1 ] && ifup --force $par 2>/dev/null
# and whatever else the machine had on its network: the tenant VLAN, tenant addresses, routes (put back if they are missing)
if [ -f "$d/before.addrs" ]; then while read -r ifc cidr; do ip addr add "\$cidr" dev "\$ifc" 2>/dev/null; done <"$d/before.addrs"; fi
if [ -f "$d/before.routes" ]; then while IFS= read -r line; do ip route replace \$line 2>/dev/null; done <"$d/before.routes"; fi
rm -f "$d/armed"
exit 0
ROLLBACK
  chmod 700 "$d/rollback.sh"
  systemd-run --quiet --unit=nuit-cloud-address-vlan-rollback --collect --on-active="${ADDR_ROLLBACK_SECONDS}s" /bin/sh "$d/rollback.sh" || { echo "  could not arm the safety timer; nothing was changed" >&2; return 1; }
  : >"$d/armed"
  addr_vlan_undo() { sh "$d/rollback.sh" >/dev/null 2>&1; systemctl stop nuit-cloud-address-vlan-rollback.timer >/dev/null 2>&1 || true; echo "  $1, so the old configuration was put back" >&2; return 1; }

  # 1. live, additive: the VLAN interface and the new address on it, beside the old address
  modprobe 8021q 2>/dev/null || true
  if [ "$created" = 1 ]; then ip link add link "$par" name "$mvif" type vlan id "$vid" || { addr_vlan_undo "could not create $mvif"; return 1; }; fi
  ip link set dev "$mvif" up || { addr_vlan_undo "could not bring $mvif up"; return 1; }
  if [ "$target_added" = 1 ]; then
    # somebody else may already answer for it on that network
    command -v arping >/dev/null 2>&1 || { apt-get install -y -qq iputils-arping >/dev/null 2>&1 || true; }
    if command -v arping >/dev/null 2>&1; then
      arping -D -q -c 2 -w 3 -I "$mvif" "$target" >/dev/null 2>&1 || { addr_vlan_undo "another device already answers for $target on VLAN $vid"; return 1; }
    fi
    ip addr add "$target/$mpfx" dev "$mvif" || { addr_vlan_undo "could not add $target to $mvif"; return 1; }
  fi
  # 2. persistent: the parent's own stanza out, the parent kept up with no address, the VLAN interface with the address and gateway
  for f in $files; do
    addr_neutralise "$f" "$par" >"$f.nuit-new" && mv "$f.nuit-new" "$f" || { addr_vlan_undo "could not rewrite $f"; return 1; }
  done
  mkdir -p "$(dirname "$sf")"
  {
    echo "# Written by the nuit-cloud agent: the management address the platform allocated for this machine, on VLAN $vid. The"
    echo "# previous configuration is commented out where it was, and kept in $ADDR_VLAN_DIR/backup. $par stays up with no address:"
    echo "# the tenant VLAN hangs off it too."
    [ "$need_auto" = 0 ] || echo "auto $par"
    echo "iface $par inet manual"
    echo "    up ip link set dev $par up"
    echo ""
    echo "auto $mvif"
    echo "iface $mvif inet static"
    echo "    address $target/$mpfx"
    echo "    gateway $mgw"
    echo "    vlan-raw-device $par"
    echo "    pre-up modprobe 8021q 2>/dev/null || true"
    echo "    pre-up ip link show $mvif >/dev/null 2>&1 || ip link add link $par name $mvif type vlan id $vid"
    echo "    post-down ip link del $mvif 2>/dev/null || true"
  } >"$sf"
  [ -e "$(p /etc/modules-load.d/8021q.conf)" ] || { mkdir -p "$(p /etc/modules-load.d)"; echo 8021q >"$(p /etc/modules-load.d/8021q.conf)"; }   # load the VLAN module at boot
  # 3. the default route moves to the VLAN's gateway, and the parent's DHCP client stops (it would take the route back on renewal)
  for pf in "/run/dhclient.$par.pid" "/run/dhcpcd/$par.pid" "/run/udhcpc.$par.pid"; do
    [ -f "$(p "$pf")" ] && kill "$(cat "$(p "$pf")")" 2>/dev/null || true
  done
  ip route replace default via "$mgw" dev "$mvif" || { addr_vlan_undo "could not move the default route to $mgw on $mvif"; return 1; }

  # 4. proof: the VLAN's gateway answers on the VLAN, and the platform answers from the new address
  ping -c 2 -W 3 -I "$mvif" "$mgw" >/dev/null 2>&1 || { addr_vlan_undo "the gateway $mgw did not answer on $mvif"; return 1; }
  proved=0
  for _ in 1 2 3 4 5 6; do
    if curl -fsS --max-time 15 --interface "$target" -o /dev/null "$NCS_PLATFORM_URL/api/agent/download/VERSION" 2>/dev/null; then proved=1; break; fi
    sleep 5
  done
  [ "$proved" = 1 ] || { addr_vlan_undo "the platform could not be reached from $target on $mvif"; return 1; }

  # 5. the old address goes, only now; then nothing else may have been lost (the tenant VLAN, tenant addresses, routes): what is
  #    missing is put back, and if it still is, everything is undone
  old_for_check=""; old_gw=""; old_if=""
  if [ "$old_goes" = 1 ]; then
    sysctl -q -w "net.ipv4.conf.$A_IF.promote_secondaries=1" >/dev/null 2>&1 || true
    ip addr del "$A_CUR/$A_PREFIX" dev "$A_IF" 2>/dev/null || true
    old_for_check="$A_CUR"; old_gw="$A_GW"; old_if="$A_IF"
  fi
  if ! addr_lost "$d" "$old_for_check" "$old_gw" "$old_if"; then
    addr_heal "$d"
    if ! addr_lost "$d" "$old_for_check" "$old_gw" "$old_if"; then
      lost_list="$(tr '\n' ';' <"$d/lost")"
      addr_vlan_undo "the move would have lost other network configuration ($lost_list)"
      return 1
    fi
  fi
  systemctl stop nuit-cloud-address-vlan-rollback.timer >/dev/null 2>&1 || true
  rm -f "$d/armed"
  echo "  $mvif now carries $target/$mpfx and the default route by $mgw (was $A_CUR on $A_IF; the old configuration is in $ADDR_VLAN_DIR/backup)"
}

# ─── The tenant VLAN slice (ticket #67; ports nuit-cloud/nuit-cloud-servers scripts/server-vlan5.sh) ───────────────────────────────────────────────────────
# Adds a VLAN sub-interface (<parent>.<id>, or vlan<id> where that would not fit) on the machine's default-route interface carrying
# the slice the platform allocated: the node's own address and the tenant block as secondary addresses, plus the rules that send
# replies for connections that arrived on the VLAN back out by the VLAN gateway (without them a reply from a tenant address leaves by
# the wrong interface and the router drops it).
# The agent passes NCS_VLAN_ID, NCS_VLAN_ADDRESS, NCS_VLAN_PREFIX, NCS_VLAN_GATEWAY and NCS_VLAN_TENANT_ADDRESSES (may be empty).
# Tenant addresses are leases that follow their services between machines (nuit-cloud/nuit-cloud#189): the tenant-address task adds
# and removes them, one `post-up ip addr add` line each in the same file. This feature owns the interface and the node's own address; it
# makes sure every tenant address it is given is there, and never removes a tenant line it finds (those belong to the task).
# The VLAN is assumed to be carried to the port already, so a gateway that does not answer is a note, not a failure. What can hurt
# is the change taking the machine's own connectivity with it, so as for the management address: a timer that undoes it is armed
# first; a snapshot proves no address or route was lost; and the platform must still be reachable afterwards, or everything is undone.
# A new machine gets a new interface (ifup). A VLAN in service (the interface is there with addresses) is never taken down: what is
# missing is added as it runs, and since #197 its own address and its prefix can change as it runs too (10.20.0.34/24 to
# 10.20.255.34/16, the tenant addresses keeping their addresses with the new prefix): the new own address first, so it is the network's
# primary and the source of its connected route; each tenant address with the new prefix beside its old one, then the old one removed
# (never missing); the old own address only after the platform was reached, unless it is given as a tenant address (the Ingress Service
# keeps 10.20.0.34). Its rollback puts back the addresses and prefixes it had and removes only what the run added; it never runs ifdown.
VLAN_TABLE="${VLAN_TABLE:-105}"
VLAN_MARK="${VLAN_MARK:-0x5}"

# Pod routing (nuit-cloud/nuit-cloud#197). Nuit Container Service pods sit on the bridge ocisaas0 and are masqueraded as whatever
# interface their traffic leaves by. Once the management address and the default route moved onto the management VLAN, pod traffic
# followed the main table out of the management network. Policy rules route it by where it comes from instead:
#   1030  fwmark 0x31 lookup main: replies from pods to connections that arrived on the management interface (the default-route
#         interface: the management VLAN's after a move, else the parent) go back the way they came, by the main table. The platform
#         reaches pool services at the machine's management address and a host port, from outside the management subnet. The mark is
#         set on the connection when it arrives there (CONNMARK) and put back on the packets that come back from ocisaas0.
#   1040  iif ocisaas0 lookup main suppress_prefixlength 0: pods use the main table's specific routes (their own bridge, the tenant
#         VLAN's connected route), never its default.
#   1045  iif ocisaas0 lookup 105: everything else from pods goes by the tenant VLAN gateway. A connection that arrived on the tenant
#         VLAN carries 0x5 and goes there too (1045, and 1050 for traffic that is not from a pod), as before.
# A rule on an interface that does not exist yet is accepted by the kernel (shown as "[detached]") and takes effect when the bridge
# appears, so the order the VLAN and the runtime are installed in does not matter, and on a machine without the runtime the rules
# match nothing. Rules are found by what they select, whatever their priority.
VLAN_POD_IF=ocisaas0
VLAN_MGMT_MARK=0x31
# the items, in the order they are added: m2 m3 (the mangle rules), m1 p1 p2 (the policy rules)
VLAN_ROUTE_ITEMS="m2 m3 m1 p1 p2"
vlan_rule_sel() { # vlan_rule_sel <m1|p1|p2> -> the policy rule's selector
  case "$1" in
    m1) echo "fwmark $VLAN_MGMT_MARK lookup main" ;;
    p1) echo "iif $VLAN_POD_IF lookup main suppress_prefixlength 0" ;;
    p2) echo "iif $VLAN_POD_IF lookup $VLAN_TABLE" ;;
  esac
}
vlan_rule_prio() { case "$1" in m1) echo 1030 ;; p1) echo 1040 ;; p2) echo 1045 ;; esac; }
vlan_rule_re() { # vlan_rule_re <m1|p1|p2> -> a grep -Ew pattern for the rule in "ip -4 rule show" (attached or not)
  case "$1" in
    m1) echo "fwmark $VLAN_MGMT_MARK lookup main" ;;
    p1) echo "iif $VLAN_POD_IF ([[]detached[]] )?lookup main suppress_prefixlength 0" ;;
    p2) echo "iif $VLAN_POD_IF ([[]detached[]] )?lookup $VLAN_TABLE" ;;
  esac
}
vlan_ipt_spec() { # vlan_ipt_spec <m2|m3> <management interface> -> the mangle PREROUTING rule
  case "$1" in
    m2) echo "-i $2 -m conntrack --ctstate NEW -j CONNMARK --set-mark $VLAN_MGMT_MARK" ;;
    m3) echo "-i $VLAN_POD_IF -m connmark --mark $VLAN_MGMT_MARK -j CONNMARK --restore-mark" ;;
  esac
}
# shellcheck disable=SC2046 # selectors and specs are split into the command's words on purpose
vlan_item_has() { # vlan_item_has <item> <management interface>: in place now
  case "$1" in
    m2|m3) iptables -t mangle -C PREROUTING $(vlan_ipt_spec "$1" "$2") 2>/dev/null ;;
    *) ip -4 rule show 2>/dev/null | grep -Ewq "$(vlan_rule_re "$1")" ;;
  esac
}
vlan_item_add() { # vlan_item_add <item> <management interface>
  case "$1" in
    m2|m3) iptables -t mangle -A PREROUTING $(vlan_ipt_spec "$1" "$2") ;;
    *) ip rule add $(vlan_rule_sel "$1") priority "$(vlan_rule_prio "$1")" ;;
  esac
}
vlan_item_undo() { # vlan_item_undo <item> <management interface> -> the command that takes it away again
  case "$1" in
    m2|m3) echo "iptables -t mangle -D PREROUTING $(vlan_ipt_spec "$1" "$2")" ;;
    *) echo "ip rule del $(vlan_rule_sel "$1") priority $(vlan_rule_prio "$1")" ;;
  esac
}
vlan_item_persisted() { # vlan_item_persisted <item> <management interface> <file>: the file puts it back at boot
  case "$1" in
    m2|m3) _ipp="iptables -t mangle -A PREROUTING $(vlan_ipt_spec "$1" "$2")" ;;
    *) _ipp="ip rule add $(vlan_rule_sel "$1")" ;;
  esac
  grep -v '^[[:space:]]*#' "$3" 2>/dev/null | grep -Fq -- "$_ipp"
}
# Add each item that is missing. The ones added are listed in <file>, if given, for the rollback.
vlan_route_ensure() { # vlan_route_ensure <management interface> [file]
  for _ri in $VLAN_ROUTE_ITEMS; do
    vlan_item_has "$_ri" "$1" && continue
    vlan_item_add "$_ri" "$1" || return 1
    [ -z "${2:-}" ] || echo "$_ri" >>"$2"
  done
}
# The management interface as the routing items name it: the default route's device, when it is a name that may go into a file.
vlan_mgmt_if() { printf '%s\n' "${A_IF:-}" | grep -Eq '^[a-zA-Z0-9._-]{1,15}$' && printf '%s\n' "$A_IF"; }

vlan_file() { printf '/etc/network/interfaces.d/nuit-cloud-vlan%s' "$NCS_VLAN_ID"; }

# vlan_tenant_lines <vlan interface> [existing file] -> the tenant lines: every `ip addr add <a>/` line already in the file (not comments),
# as it is, then one line for each given address the file does not have yet. One line per address (the first kept), in that order. A
# line kept whose address is inside the VLAN's network gets the VLAN's prefix (the network may have grown, /24 to /16: #197); the rest
# of the line stays as it was.
vlan_tenant_lines() {
  _tv="$1"; _tf="${2:-}"; _seen=" "
  if [ -n "$_tf" ] && [ -f "$_tf" ]; then
    while IFS= read -r _tl || [ -n "$_tl" ]; do
      case "$_tl" in *"ip addr add "*) ;; *) continue;; esac
      printf '%s\n' "$_tl" | grep -q '^[[:space:]]*#' && continue
      _ta="$(printf '%s\n' "$_tl" | sed -n 's/.*ip addr add \([0-9][0-9.]*\)\/.*/\1/p')"
      [ -n "$_ta" ] || continue
      case "$_seen" in *" $_ta "*) continue;; esac
      _seen="$_seen$_ta "
      if addr_is_ipv4 "$_ta" && vlan_in_net "$_ta"; then
        _tre="$(printf '%s' "$_ta" | sed 's/\./\\./g')"
        printf '%s\n' "$_tl" | sed "s#ip addr add $_tre/[0-9]*#ip addr add $_ta/$NCS_VLAN_PREFIX#"
      else
        printf '%s\n' "$_tl"
      fi
    done <"$_tf"
  fi
  for _ta in ${NCS_VLAN_TENANT_ADDRESSES:-}; do
    case "$_seen" in *" $_ta "*) continue;; esac
    _seen="$_seen$_ta "
    echo "    post-up ip addr add ${_ta}/${NCS_VLAN_PREFIX} dev ${_tv} 2>/dev/null || true"
  done
}

# vlan_render <parent> [existing file] [VLAN interface] -> the stanza on stdout, keeping the existing file's tenant lines. The VLAN
# interface is <parent>.<id> unless given, or unless that would not fit (vlan<id>, which then names its parent with vlan-raw-device).
vlan_render() { # vlan_render <parent> [existing file] [VLAN interface] [management interface, else the parent]
  _if="$1"; _vif="${3:-$(vlan_ifname "$_if" "$NCS_VLAN_ID")}"; _mif="${4:-$_if}"
  _vsys="$(printf '%s' "$_vif" | tr . /)"   # sysctl writes a dot in an interface name as a slash
  cat <<EOF
# Managed by the nuit-cloud agent (ncs_vlan): the tenant VLAN slice the platform allocated for this machine.
auto ${_vif}
iface ${_vif} inet static
    address ${NCS_VLAN_ADDRESS}/${NCS_VLAN_PREFIX}
EOF
  [ "$_vif" = "$_if.$NCS_VLAN_ID" ] || echo "    vlan-raw-device ${_if}"
  cat <<EOF
    pre-up modprobe 8021q 2>/dev/null || true
    pre-up ip link show ${_vif} >/dev/null 2>&1 || ip link add link ${_if} name ${_vif} type vlan id ${NCS_VLAN_ID}
    pre-up sysctl -qw net.ipv4.conf.${_vsys}.promote_secondaries=1 2>/dev/null || true
EOF
  vlan_tenant_lines "$_vif" "${2:-}"
  cat <<EOF
    post-up sysctl -qw net.ipv4.conf.${_vsys}.rp_filter=2
    post-up iptables -t mangle -C PREROUTING -i ${_vif} -m conntrack --ctstate NEW -j CONNMARK --set-mark ${VLAN_MARK} 2>/dev/null || iptables -t mangle -A PREROUTING -i ${_vif} -m conntrack --ctstate NEW -j CONNMARK --set-mark ${VLAN_MARK}
    post-up iptables -t mangle -C PREROUTING ! -i ${_vif} -m connmark --mark ${VLAN_MARK} -j CONNMARK --restore-mark 2>/dev/null || iptables -t mangle -A PREROUTING ! -i ${_vif} -m connmark --mark ${VLAN_MARK} -j CONNMARK --restore-mark
    post-up ip route replace default via ${NCS_VLAN_GATEWAY} dev ${_vif} table ${VLAN_TABLE}
    post-up ip rule show | grep -q "fwmark ${VLAN_MARK} lookup ${VLAN_TABLE}" || ip rule add fwmark ${VLAN_MARK} lookup ${VLAN_TABLE} priority 1050
    post-up iptables -t mangle -C PREROUTING $(vlan_ipt_spec m2 "$_mif") 2>/dev/null || iptables -t mangle -A PREROUTING $(vlan_ipt_spec m2 "$_mif")
    post-up iptables -t mangle -C PREROUTING $(vlan_ipt_spec m3 "$_mif") 2>/dev/null || iptables -t mangle -A PREROUTING $(vlan_ipt_spec m3 "$_mif")
    post-up ip -4 rule show | grep -Ewq '$(vlan_rule_re m1)' || ip rule add $(vlan_rule_sel m1) priority $(vlan_rule_prio m1)
    post-up ip -4 rule show | grep -Ewq '$(vlan_rule_re p1)' || ip rule add $(vlan_rule_sel p1) priority $(vlan_rule_prio p1)
    post-up ip -4 rule show | grep -Ewq '$(vlan_rule_re p2)' || ip rule add $(vlan_rule_sel p2) priority $(vlan_rule_prio p2)
    pre-down ip rule del $(vlan_rule_sel p2) 2>/dev/null || true
    pre-down ip rule del $(vlan_rule_sel p1) 2>/dev/null || true
    pre-down ip rule del $(vlan_rule_sel m1) 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING $(vlan_ipt_spec m3 "$_mif") 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING $(vlan_ipt_spec m2 "$_mif") 2>/dev/null || true
    pre-down ip rule del fwmark ${VLAN_MARK} lookup ${VLAN_TABLE} 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING ! -i ${_vif} -m connmark --mark ${VLAN_MARK} -j CONNMARK --restore-mark 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING -i ${_vif} -m conntrack --ctstate NEW -j CONNMARK --set-mark ${VLAN_MARK} 2>/dev/null || true
    pre-down ip route flush table ${VLAN_TABLE} 2>/dev/null || true
    post-down ip link del ${_vif} 2>/dev/null || true
EOF
}

# vlan_in_net <address>: inside the VLAN's network (NCS_VLAN_ADDRESS/NCS_VLAN_PREFIX)
vlan_in_net() { [ "$(addr_net_of "$1" "$NCS_VLAN_PREFIX")" = "$(addr_net_of "$NCS_VLAN_ADDRESS" "$NCS_VLAN_PREFIX")" ]; }
# vlan_net_cidr <address> <prefix> -> the network the address is in, written a.b.c.d/<prefix> (as the kernel names its connected route)
vlan_net_cidr() {
  awk -v ip="$1" -v pl="$2" 'BEGIN {split(ip, o, "."); v = ((o[1] * 256 + o[2]) * 256 + o[3]) * 256 + o[4]; b = 2 ^ (32 - pl); v = int(v / b) * b
    printf "%d.%d.%d.%d/%d\n", int(v / 16777216) % 256, int(v / 65536) % 256, int(v / 256) % 256, v % 256, pl}'
}
# vlan_live <interface> -> its IPv4 addresses as <address>/<prefix>, in the kernel's order (each network's primary before its secondaries)
vlan_live() { ip -4 -o addr show dev "$1" 2>/dev/null | awk '$4 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\/[0-9]+$/ {print $4}'; }
# vlan_stanza_address <file> <interface> -> the address line of the interface's stanza (<address>/<prefix>), if there is one
vlan_stanza_address() {
  [ -f "$1" ] || return 0
  awk -v i="$2" '$1 == "iface" {s = ($2 == i && $3 == "inet"); next} /^(auto|allow-[a-z]+|mapping|source)/ {s = 0} s && $1 == "address" {print $2; exit}' "$1"
}
# vlan_file_tenants <file> -> the addresses the file's tenant lines add (comments left out)
vlan_file_tenants() {
  [ -f "$1" ] || return 0
  grep -v '^[[:space:]]*#' "$1" | sed -n 's/.*ip addr add \([0-9][0-9.]*\)\/.*/\1/p' | while read -r _fa; do addr_is_ipv4 "$_fa" && echo "$_fa"; done
}
# vlan_re <address> -> the address as an extended regular expression (its dots literal)
vlan_re() { printf '%s' "$1" | sed 's/\./\\./g'; }
# vlan_route_src <interface> -> the source of the connected route of the VLAN's network on the interface (empty when there is none)
vlan_route_src() {
  ip -4 route show 2>/dev/null | awk -v n="$(vlan_net_cidr "$NCS_VLAN_ADDRESS" "$NCS_VLAN_PREFIX")" -v v="$1" \
    '$1 == n && $2 == "dev" && $3 == v {for (i = 4; i < NF; i++) if ($i == "src") {print $(i + 1); exit}}'
}
vlan_reverse() { awk '{l[NR] = $0} END {for (i = NR; i > 0; i--) print l[i]}'; }
# vlan_table_ok <interface>: the tenant VLAN's own table sends by its gateway on the interface
vlan_table_ok() {
  ip -4 route show table "$VLAN_TABLE" 2>/dev/null | awk -v g="$NCS_VLAN_GATEWAY" -v v="$1" '$1 == "default" && $2 == "via" && $3 == g && $4 == "dev" && $5 == v {f = 1} END {exit !f}'
}
# vlan_drop_old_routes <interface> <file of <address>/<prefix> lines>: a connected route of a network those addresses were in, with their
# prefix, that lingers once the interface has no address left in it (the kernel normally takes it away itself)
vlan_drop_old_routes() {
  while IFS=/ read -r _oa _oq; do
    [ -n "$_oa" ] && [ "$_oq" != "$NCS_VLAN_PREFIX" ] || continue
    _on="$(vlan_net_cidr "$_oa" "$_oq")"
    _still=0
    for _c in $(vlan_live "$1"); do [ "${_c#*/}" = "$_oq" ] && [ "$(vlan_net_cidr "${_c%/*}" "$_oq")" = "$_on" ] && _still=1; done
    [ "$_still" = 0 ] || continue
    if ip -4 route show 2>/dev/null | awk -v n="$_on" -v v="$1" '$1 == n && $2 == "dev" && $3 == v {f = 1} END {exit !f}'; then
      ip route del "$_on" dev "$1" 2>/dev/null || true
    fi
  done <"$2"
}
# vlan_prove: the platform answers (or, with no platform address given, the management gateway)
vlan_prove() {
  if [ -n "${NCS_PLATFORM_URL:-}" ]; then
    for _ in 1 2 3 4 5 6; do
      curl -fsS --max-time 15 -o /dev/null "$NCS_PLATFORM_URL/api/agent/download/VERSION" 2>/dev/null && return 0
      sleep 5
    done
    return 1
  fi
  ping -c 2 -W 3 "$A_GW" >/dev/null 2>&1
}
# vlan_intact <dir>: nothing the machine had is gone apart from what the change meant to replace (<dir>/meant); what is missing is put
# back once and looked at again
vlan_intact() {
  addr_lost "$1" "" && return 0
  addr_heal "$1"
  addr_lost "$1" ""
}

vlan_inputs_ok() {
  printf '%s' "${NCS_VLAN_ID:-}" | grep -Eq '^[0-9]{1,4}$' || return 1
  printf '%s' "${NCS_VLAN_PREFIX:-}" | grep -Eq '^[0-9]{1,2}$' && [ "$NCS_VLAN_PREFIX" -ge 16 ] && [ "$NCS_VLAN_PREFIX" -le 30 ] || return 1
  addr_is_ipv4 "${NCS_VLAN_ADDRESS:-}" && addr_is_ipv4 "${NCS_VLAN_GATEWAY:-}" || return 1
  # one network: the gateway and every tenant address are in the node's own address's network, and none of them is another
  [ "$NCS_VLAN_GATEWAY" != "$NCS_VLAN_ADDRESS" ] && vlan_in_net "$NCS_VLAN_GATEWAY" || return 1
  for a in ${NCS_VLAN_TENANT_ADDRESSES:-}; do
    addr_is_ipv4 "$a" && vlan_in_net "$a" && [ "$a" != "$NCS_VLAN_ADDRESS" ] && [ "$a" != "$NCS_VLAN_GATEWAY" ] || return 1
  done
  return 0
}

f_ncs_vlan_ok() {
  vlan_inputs_ok && addr_discover || return 1
  _file="$(p "$(vlan_file)")"
  _vif="$(vlan_dev "$A_PARENT" "$NCS_VLAN_ID")"
  # what this feature writes, or an equivalent configuration made by hand (nuit-cloud/nuit-cloud-servers scripts/server-vlan5.sh): the
  # sub-interface with the node's own address, and every given tenant address named in it. Other tenant lines are the tenant-address
  # task's and are allowed. A working VLAN made by hand is adopted as it is, never rewritten, once it also carries the pod rules.
  grep -Eq "^[[:space:]]*iface[[:space:]]+$_vif[[:space:]]+inet[[:space:]]+static" "$_file" 2>/dev/null || return 1
  grep -Eq "^[[:space:]]*address[[:space:]]+$(vlan_re "$NCS_VLAN_ADDRESS")/$NCS_VLAN_PREFIX([[:space:]]|$)" "$_file" || return 1
  # every tenant line inside the VLAN's network (the given ones, which must be there, and the task's) has the VLAN's prefix (#197)
  for a in ${NCS_VLAN_TENANT_ADDRESSES:-}; do grep -v '^[[:space:]]*#' "$_file" | grep -Fq "ip addr add $a/" || return 1; done
  for a in $(vlan_file_tenants "$_file"); do
    vlan_in_net "$a" || continue
    grep -v '^[[:space:]]*#' "$_file" | grep -Eq "ip addr add $(vlan_re "$a")/$NCS_VLAN_PREFIX([[:space:]]|$)" || return 1
  done
  _have="$(vlan_live "$_vif")"
  for a in "$NCS_VLAN_ADDRESS" ${NCS_VLAN_TENANT_ADDRESSES:-}; do printf '%s\n' "$_have" | grep -qxF "$a/$NCS_VLAN_PREFIX" || return 1; done
  # nothing in the VLAN's network with another prefix (an address with the prefix the network had before, or the old own address),
  # and the network's connected route is from the node's own address, not a tenant's
  for c in $_have; do [ "${c#*/}" = "$NCS_VLAN_PREFIX" ] || ! vlan_in_net "${c%/*}" || return 1; done
  [ "$(vlan_route_src "$_vif")" = "$NCS_VLAN_ADDRESS" ] || return 1
  # pod routing (nuit-cloud/nuit-cloud#197): every item in place now, for the management interface the machine has now, and in the
  # file so it comes back at boot (a management move after the VLAN was written is drift here)
  _mif="$(vlan_mgmt_if)" && [ "$_mif" != "$_vif" ] || return 1
  command -v iptables >/dev/null 2>&1 || return 1
  for _ri in $VLAN_ROUTE_ITEMS; do
    vlan_item_has "$_ri" "$_mif" && vlan_item_persisted "$_ri" "$_mif" "$_file" || return 1
  done
  return 0
}

f_ncs_vlan_do() {
  vlan_inputs_ok || { echo "  the VLAN slice was not given, or is not valid: allocate one in the portal, then apply this profile from there" >&2; return 3; }
  command -v ifup >/dev/null 2>&1 && [ -f "$(p /etc/network/interfaces)" ] || { echo "  this machine does not use ifupdown: set the VLAN up by hand (nuit-cloud/nuit-cloud-servers scripts/server-vlan5.sh)" >&2; return 3; }
  for m in NetworkManager systemd-networkd; do
    ! systemctl is-active --quiet "$m" 2>/dev/null || { echo "  $m manages the network here; this feature only handles ifupdown" >&2; return 3; }
  done
  # A minimal Debian has nftables and no iptables, and this runs before the runtime (which installs it) on a new machine.
  command -v iptables >/dev/null 2>&1 || apt-get install -y -qq iptables >/dev/null 2>&1 || true
  command -v systemd-run >/dev/null 2>&1 && command -v iptables >/dev/null 2>&1 || { echo "  systemd-run (the safety timer) and iptables (reply routing) are needed" >&2; return 3; }
  addr_discover && [ -n "$A_GW" ] || { echo "  could not find the default route and its interface" >&2; return 3; }
  # the VLAN hangs off the physical interface: when the management address is itself on a VLAN (nuit-cloud/nuit-cloud#197), off that
  # VLAN's parent, beside it. Its interface is <parent>.<id>, or vlan<id> when that would be longer than 15 characters (vlan_ifname).
  printf '%s\n' "$A_PARENT" | grep -Eq '^[a-zA-Z0-9_-]{1,15}$' || { echo "  the interface name $A_PARENT cannot carry a VLAN sub-interface here" >&2; return 3; }
  vif="$(vlan_dev "$A_PARENT" "$NCS_VLAN_ID")"
  printf '%s\n' "$vif" | grep -Eq '^[a-zA-Z0-9._-]{1,15}$' || { echo "  odd VLAN interface name $vif: set the VLAN up by hand" >&2; return 3; }
  [ "$A_IF" != "$vif" ] || { echo "  the machine's default route is on $A_IF, which is the tenant VLAN's own interface: set this up by hand" >&2; return 3; }
  mif="$(vlan_mgmt_if)" || { echo "  odd interface name for the default route: set the VLAN up by hand" >&2; return 3; }
  vlan_name_free "$vif" "$A_PARENT" "$NCS_VLAN_ID" || { echo "  $vif exists already and is not VLAN $NCS_VLAN_ID on $A_PARENT: set the VLAN up by hand" >&2; return 3; }
  # A VLAN interface that is there with addresses on it is in service (#197): it is never taken down and brought up again, which would
  # drop its tenant addresses and the services on them. It is brought to what is wanted as it runs (the live path below).
  link_was=0; ip link show dev "$vif" >/dev/null 2>&1 && link_was=1
  live=0; [ "$link_was" = 1 ] && [ -n "$(vlan_live "$vif")" ] && live=1
  # ifup of a VLAN sub-interface first brings up its parent interface unless ifupdown already has it recorded as up; on a machine whose
  # parent was configured another way that would run its address configuration again. Found on a real kernel: only proceed if ifupdown
  # itself holds the parent up. (The live path runs no ifup.)
  if [ "$live" = 0 ]; then
    command -v ifquery >/dev/null 2>&1 && ifquery --state "$A_PARENT" >/dev/null 2>&1 || {
      echo "  $A_PARENT is not held up by ifupdown here (ifquery --state says it is not), and ifup of its VLAN would try to bring it up itself and could disturb its address: set the VLAN up by hand (nuit-cloud/nuit-cloud-servers scripts/server-vlan5.sh)" >&2
      return 3
    }
  fi
  # the VLAN's own addresses must not already be somewhere else on this machine, with any prefix
  for a in "$NCS_VLAN_ADDRESS" ${NCS_VLAN_TENANT_ADDRESSES:-}; do
    if ip -4 -o addr show 2>/dev/null | awk -v v="$vif" -v a="$a" '$2 != v {split($4, x, "/"); if (x[1] == a) f = 1} END {exit !f}'; then echo "  $a is already on another interface of this machine: nothing was changed" >&2; return 1; fi
  done

  file="$(p "$(vlan_file)")"
  vsys="$(printf '%s' "$vif" | tr . /)"   # sysctl writes a dot in an interface name as a slash
  pfx="$NCS_VLAN_PREFIX"; own="$NCS_VLAN_ADDRESS"
  d="$(p "$ADDR_DIR")/vlan"; rm -rf "$d"; mkdir -p "$d"; chmod 700 "$d"
  addr_snapshot "$d"
  vlan_live "$vif" >"$d/before.vif"
  ip -4 route show table "$VLAN_TABLE" 2>/dev/null | awk '$1 == "default"' >"$d/before.table"
  promote_was="$(cat "$(p "/proc/sys/net/ipv4/conf/$vif/promote_secondaries")" 2>/dev/null || true)"
  printf '%s' "$promote_was" | grep -Eq '^[0-9]$' || promote_was=""
  existing=0; [ -f "$file" ] && { cp -p "$file" "$d/previous"; existing=1; }
  : >"$d/added.rules"; : >"$d/added.addrs"; : >"$d/meant"; : >"$d/reprefix"; : >"$d/stale"
  # On a VLAN in service, what changes is worked out before anything does (#197). The addresses kept are the node's own, the given
  # tenant addresses and the file's tenant lines (the tenant-address task's); one of them on the interface with another prefix is moved
  # to the VLAN's prefix (reprefix). The old own address (the file's address line, else the interface's first address), unless it is
  # kept as a tenant address, and anything else in the VLAN's network with another prefix, go once the change is proved (stale).
  # Both lists run secondaries first, so a network's primary is the last of it to go.
  old="$(vlan_stanza_address "$file" "$vif")"; [ -n "$old" ] || old="$(head -n 1 "$d/before.vif")"; old="${old%/*}"
  addr_is_ipv4 "$old" || old=""
  keep=" $own ${NCS_VLAN_TENANT_ADDRESSES:-} $(vlan_file_tenants "$file" | tr '\n' ' ') "
  if [ "$live" = 1 ]; then
    vlan_reverse <"$d/before.vif" | while IFS=/ read -r a q; do
      case "$keep" in *" $a "*) [ "$q" = "$pfx" ] || ! vlan_in_net "$a" || echo "$a/$q" >>"$d/reprefix"; continue ;; esac
      if [ "$a" = "$old" ] || { [ "$q" != "$pfx" ] && vlan_in_net "$a"; }; then echo "$a/$q" >>"$d/stale"; fi
    done
    # what the change means to take away is not counted as lost: those addresses, and the connected routes of their networks
    cat "$d/reprefix" "$d/stale" | while IFS=/ read -r a q; do
      echo "addr $vif $a/$q" >>"$d/meant"; echo "route $(vlan_net_cidr "$a" "$q") dev $vif " >>"$d/meant"
    done
    echo "route $(vlan_net_cidr "$own" "$pfx") dev $vif " >>"$d/meant"   # its source may become the node's own address
  fi
  if [ "$live" = 1 ]; then
    undo_link="# the VLAN was up before: it stays up; only what this run added is taken away and what it removed put back"
  else
    undo_link="ifdown --force $vif 2>/dev/null"
  fi
  undo_items="$(for i in $VLAN_ROUTE_ITEMS; do printf '    %s) %s 2>/dev/null ;;\n' "$i" "$(vlan_item_undo "$i" "$mif")"; done)"
  cat >"$d/rollback.sh" <<ROLLBACK
#!/bin/sh
# Take the tenant VLAN change off the machine again. Run by the safety timer, or at once if the change could not be proved to work.
PATH="\$PATH:/usr/local/sbin:/usr/sbin:/sbin"
echo "nuit-cloud: undoing the tenant VLAN change"
$undo_link
if [ "$existing" = 1 ]; then cp -p "$d/previous" "$file"; else rm -f "$file"; fi
[ "$live" = 1 ] || [ "$link_was" = 1 ] || ip link del $vif 2>/dev/null
# a VLAN in service: the addresses it had come back first, each network's primary first, so no tenant address is ever missing
if [ "$live" = 1 ]; then while read -r cidr; do ip addr add "\$cidr" dev $vif 2>/dev/null; done <"$d/before.vif"; fi
# the routing items this run added (the ones that were there before stay)
while read -r n; do
  case "\$n" in
$undo_items
  esac
done <"$d/added.rules"
# then what this run added goes, the last added first (secondaries before the primary they hang on)
awk '{l[NR] = \$0} END {for (i = NR; i > 0; i--) print l[i]}' "$d/added.addrs" | while read -r cidr; do ip addr del "\$cidr" dev $vif 2>/dev/null; done
if [ -f "$d/before.addrs" ]; then while read -r ifc cidr; do ip addr add "\$cidr" dev "\$ifc" 2>/dev/null; done <"$d/before.addrs"; fi
if [ -f "$d/before.routes" ]; then while IFS= read -r line; do ip route replace \$line 2>/dev/null; done <"$d/before.routes"; fi
if [ "$live" = 1 ]; then
  if [ -s "$d/before.table" ]; then while IFS= read -r line; do ip route replace \$line table $VLAN_TABLE 2>/dev/null; done <"$d/before.table"
  elif [ -f "$d/table.added" ]; then ip route del default table $VLAN_TABLE 2>/dev/null; fi
  [ -z "$promote_was" ] || sysctl -qw net.ipv4.conf.$vsys.promote_secondaries=$promote_was >/dev/null 2>&1
fi
rm -f "$d/armed"
exit 0
ROLLBACK
  chmod 700 "$d/rollback.sh"
  systemd-run --quiet --unit=nuit-cloud-vlan-rollback --collect --on-active="${ADDR_ROLLBACK_SECONDS}s" /bin/sh "$d/rollback.sh" || { echo "  could not arm the safety timer; nothing was changed" >&2; return 1; }
  : >"$d/armed"

  mkdir -p "$(dirname "$file")"
  # the tenant lines already there (the tenant-address task's, or a hand setup's) are kept, with the VLAN's prefix; the given ones are added
  if [ "$existing" = 1 ]; then vlan_render "$A_PARENT" "$d/previous" "$vif" "$mif" >"$file"; else vlan_render "$A_PARENT" "" "$vif" "$mif" >"$file"; fi
  [ -e "$(p /etc/modules-load.d/8021q.conf)" ] || { mkdir -p "$(p /etc/modules-load.d)"; echo 8021q >"$(p /etc/modules-load.d/8021q.conf)"; }   # load the VLAN module at boot
  systemctl restart systemd-modules-load.service >/dev/null 2>&1 || true   # the agent's own sandbox may not load modules; systemd can
  if [ "$live" = 1 ]; then
    # As it runs (the file's lines did not run: no ifup). Checked on a real kernel (Linux 7.0, #197): one address can be on an interface
    # with two prefixes at once (each is its own entry, with its own local route), so a tenant address gets its new prefix before its
    # old one goes and is never missing; the first address of a network is its primary and the source of the network's connected
    # route; and removing a primary removes its secondaries with it unless promote_secondaries is on.
    sysctl -qw "net.ipv4.conf.$vsys.promote_secondaries=1" >/dev/null 2>&1 || true
    # 1. the node's own address with the VLAN's prefix, first (the primary of the network), then every tenant address with it
    for a in "$own" ${NCS_VLAN_TENANT_ADDRESSES:-} $(sed 's#/.*##' "$d/reprefix"); do
      vlan_live "$vif" | grep -qxF "$a/$pfx" && continue
      if ! ip addr add "$a/$pfx" dev "$vif"; then sh "$d/rollback.sh"; echo "  could not add $a/$pfx to $vif, so the change was taken away" >&2; return 1; fi
      echo "$a/$pfx" >>"$d/added.addrs"
    done
    # 2. the network's connected route is from the node's own address (a tenant address may have held the network first)
    if [ "$(vlan_route_src "$vif")" != "$own" ]; then
      if ! ip route replace "$(vlan_net_cidr "$own" "$pfx")" dev "$vif" proto kernel scope link src "$own"; then
        sh "$d/rollback.sh"; echo "  could not make $own the source of the VLAN's connected route, so the change was taken away" >&2; return 1
      fi
    fi
    # 3. the kept addresses' old prefixes go (each is on the interface with the VLAN's prefix already), secondaries first
    while IFS= read -r c; do
      vlan_live "$vif" | grep -qxF "$c" || continue
      if ! ip addr del "$c" dev "$vif"; then sh "$d/rollback.sh"; echo "  could not remove $c from $vif, so the change was taken away" >&2; return 1; fi
    done <"$d/reprefix"
    vlan_drop_old_routes "$vif" "$d/reprefix"
    # 4. the tenant VLAN's own table still sends by its gateway
    if ! vlan_table_ok "$vif"; then
      if ! ip route replace default via "$NCS_VLAN_GATEWAY" dev "$vif" table "$VLAN_TABLE"; then
        sh "$d/rollback.sh"; echo "  could not route table $VLAN_TABLE by $NCS_VLAN_GATEWAY, so the change was taken away" >&2; return 1
      fi
      : >"$d/table.added"
    fi
  elif ! ifup "$vif" >"$d/ifup.out" 2>&1; then
    sh "$d/rollback.sh"
    echo "  ifup $vif failed ($(tr '\n' ' ' <"$d/ifup.out" | cut -c1-200)), so the machine was put back" >&2
    return 1
  fi
  # pod routing: the file's post-up lines add the items at ifup; on a VLAN that was up already they are added here, and in either case
  # anything still missing is added now (a rule by its selector, so one there already, whatever its priority, is not doubled)
  if ! vlan_route_ensure "$mif" "$d/added.rules"; then
    sh "$d/rollback.sh"
    echo "  could not add the pod routing rules (iif $VLAN_POD_IF, mark $VLAN_MGMT_MARK on $mif), so the machine was put back" >&2
    return 1
  fi

  # nothing that was there may be gone (apart from what was meant to go), and the platform must still be reachable
  if ! vlan_intact "$d"; then
    lost_list="$(tr '\n' ';' <"$d/lost")"
    sh "$d/rollback.sh"
    echo "  the change would have lost other network configuration ($lost_list), so the machine was put back" >&2
    return 1
  fi
  if ! vlan_prove; then
    sh "$d/rollback.sh"
    echo "  the platform could not be reached after the change, so the machine was put back" >&2
    return 1
  fi
  # 5. only now, with the platform reached: the old own address (unless it is kept as a tenant address) and what else in the VLAN's
  #    network still has another prefix, secondaries first; then the platform must still be reached, or all of it comes back
  if [ -s "$d/stale" ]; then
    while IFS= read -r c; do
      vlan_live "$vif" | grep -qxF "$c" || continue
      if ! ip addr del "$c" dev "$vif"; then sh "$d/rollback.sh"; echo "  could not remove $c from $vif, so the change was taken away" >&2; return 1; fi
    done <"$d/stale"
    vlan_drop_old_routes "$vif" "$d/stale"
    if ! vlan_intact "$d"; then
      lost_list="$(tr '\n' ';' <"$d/lost")"
      sh "$d/rollback.sh"
      echo "  removing the old addresses would have lost other network configuration ($lost_list), so the machine was put back" >&2
      return 1
    fi
    if ! vlan_prove; then
      sh "$d/rollback.sh"
      echo "  the platform could not be reached once the old addresses were removed, so the machine was put back" >&2
      return 1
    fi
  fi
  systemctl stop nuit-cloud-vlan-rollback.timer >/dev/null 2>&1 || true
  rm -f "$d/armed"
  if [ "$live" = 1 ]; then
    echo "  $vif was up already and was left up; added as it runs: $(wc -l <"$d/added.rules" | tr -d ' ') pod routing item(s), $(wc -l <"$d/added.addrs" | tr -d ' ') address(es) with /$pfx; moved to /$pfx: $(wc -l <"$d/reprefix" | tr -d ' '); removed: $( [ -s "$d/stale" ] && tr '\n' ' ' <"$d/stale" | sed 's/ $//' || echo nothing)"
  fi
  if ping -c 2 -W 2 -I "$vif" "$NCS_VLAN_GATEWAY" >/dev/null 2>&1; then
    echo "  $vif carries $NCS_VLAN_ADDRESS/$pfx and the tenant addresses; the VLAN gateway $NCS_VLAN_GATEWAY answers"
  else
    echo "  $vif carries $NCS_VLAN_ADDRESS/$pfx and the tenant addresses. Note: the VLAN gateway $NCS_VLAN_GATEWAY did not answer yet; the VLAN is assumed to reach this port, so this is left in place"
  fi
}

# ─── SSH hardening, with the platform in the check ──────────────────────────────────────────────────────────────────────────────
# Writes /etc/ssh/sshd_config.d/00-nuit-cloud.conf (PermitRootLogin no, PasswordAuthentication no; the 00- makes it win, since sshd
# keeps the first value it sees). This is the one change that can lock everyone out, so:
#   * it refuses unless the login account's authorized_keys holds at least one of the keys the platform names
#     (NCS_SSH_REQUIRE_FINGERPRINTS, SHA256 fingerprints), sshd accepts keys at all, sshd's configuration is valid and includes
#     sshd_config.d, and the keys file is one sshd will honour (owned by the account, not group- or world-writable);
#   * before anything changes a timer is armed (nuit-cloud-ssh-rollback) that takes the file away and reloads sshd;
#   * after the reload it checks what sshd now uses and that it still listens, then leaves the timer ARMED: the platform tests sshd
#     from outside (answers, offers a key and no password) and only then sends `ssh-confirm`, which cancels the timer. If the
#     platform's test fails or never happens, passwords come back by themselves.
SSH_DROPIN="${SSH_DROPIN:-/etc/ssh/sshd_config.d/00-nuit-cloud.conf}"
SSH_CONFIRM_SECONDS="${NCS_SSH_CONFIRM_SECONDS:-600}"

f_ncs_ssh_harden_ok() { f_ncs_ssh_ok; }

ssh_reload() { systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null; }

f_ncs_ssh_harden_do() {
  required="${NCS_SSH_REQUIRE_FINGERPRINTS:-}"
  [ -n "$required" ] || { echo "  the platform named no administrator key that must still be able to log in: apply this profile from the portal" >&2; return 3; }
  for f in $required; do printf '%s' "$f" | grep -Eq '^SHA256:[A-Za-z0-9+/]{43}$' || { echo "  '$f' is not a key fingerprint (SHA256:…)" >&2; return 3; }; done
  command -v sshd >/dev/null 2>&1 && command -v ssh-keygen >/dev/null 2>&1 && command -v systemd-run >/dev/null 2>&1 || { echo "  sshd, ssh-keygen and systemd-run are needed" >&2; return 3; }
  sshd -t 2>/dev/null || { echo "  sshd's configuration is not valid as it stands: nothing was changed" >&2; return 3; }
  grep -Eqi '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' "$(p /etc/ssh/sshd_config)" 2>/dev/null \
    || { echo "  /etc/ssh/sshd_config does not include sshd_config.d/*.conf, so the override would do nothing: edit it by hand" >&2; return 3; }
  [ "$(sshd_effective pubkeyauthentication)" = yes ] || { echo "  sshd does not accept keys (PubkeyAuthentication is not yes): turning passwords off would lock everyone out" >&2; return 3; }

  home="$(getent passwd "$NC_USER" 2>/dev/null | cut -d: -f6)"
  [ -n "$home" ] || { echo "  there is no account $NC_USER" >&2; return 3; }
  keys="$home/.ssh/authorized_keys"
  case "$(sshd_effective authorizedkeysfile)" in *.ssh/authorized_keys*) ;; *) echo "  sshd reads keys from somewhere other than .ssh/authorized_keys: set it up by hand" >&2; return 3 ;; esac
  [ -s "$keys" ] || { echo "  $keys is missing or empty: $NC_USER has no key to log in with" >&2; return 3; }
  owner="$(stat -c %U "$keys" 2>/dev/null || stat -f %Su "$keys" 2>/dev/null)"
  [ "$owner" = "$NC_USER" ] || [ "$owner" = root ] || { echo "  $keys is owned by $owner, so sshd ignores it" >&2; return 3; }
  [ -z "$(find "$keys" \( -perm -020 -o -perm -002 \) 2>/dev/null)" ] || { echo "  $keys is writable by others, so sshd ignores it" >&2; return 3; }
  have="$(ssh-keygen -lf "$keys" 2>/dev/null | awk '{print $2}')"
  found=0; for f in $required; do printf '%s\n' "$have" | grep -qx "$f" && found=1; done
  [ "$found" = 1 ] || { echo "  none of the keys the platform expects ($required) is in $keys: add one before passwords are turned off" >&2; return 3; }

  d="$(p "$ADDR_DIR")/ssh"; rm -rf "$d"; mkdir -p "$d"; chmod 700 "$d"
  dropin="$(p "$SSH_DROPIN")"; existing=0
  [ -f "$dropin" ] && { cp -p "$dropin" "$d/previous"; existing=1; }
  cat >"$d/rollback.sh" <<ROLLBACK
#!/bin/sh
# Put SSH back as it was before it was hardened. Run by the safety timer, or at once if the change could not be proved to work.
PATH="\$PATH:/usr/local/sbin:/usr/sbin:/sbin"
echo "nuit-cloud: undoing the SSH hardening"
if [ "$existing" = 1 ]; then cp -p "$d/previous" "$dropin"; else rm -f "$dropin"; fi
sshd -t && { systemctl reload ssh || systemctl reload sshd; }
rm -f "$d/armed"
exit 0
ROLLBACK
  chmod 700 "$d/rollback.sh"
  systemd-run --quiet --unit=nuit-cloud-ssh-rollback --collect --on-active="${SSH_CONFIRM_SECONDS}s" /bin/sh "$d/rollback.sh" || { echo "  could not arm the safety timer; nothing was changed" >&2; return 1; }
  : >"$d/armed"

  mkdir -p "$(dirname "$dropin")"
  printf '# Written by the nuit-cloud agent: SSH is for keys only. The 00- prefix makes these win over later files.\nPermitRootLogin no\nPasswordAuthentication no\n' >"$dropin"
  if ! sshd -t 2>"$d/sshd-t.out"; then
    sh "$d/rollback.sh"; echo "  sshd rejected the new configuration ($(tr '\n' ' ' <"$d/sshd-t.out" | cut -c1-160)), so it was taken away" >&2; return 1
  fi
  ssh_reload || { sh "$d/rollback.sh"; echo "  sshd could not be reloaded, so the change was taken away" >&2; return 1; }
  sleep 2
  root="$(sshd_effective permitrootlogin)"; pass="$(sshd_effective passwordauthentication)"
  if [ "$root" = yes ] || [ "$pass" != no ]; then
    sh "$d/rollback.sh"; echo "  sshd still uses PermitRootLogin $root / PasswordAuthentication $pass (another file wins?), so the change was taken away" >&2; return 1
  fi
  port="$(sshd_effective port)"; port="${port:-22}"
  ss -ltnH 2>/dev/null | awk '{print $4}' | grep -q ":$port\$" || { sh "$d/rollback.sh"; echo "  sshd is not listening on port $port after the reload, so the change was taken away" >&2; return 1; }
  echo "  SSH now refuses passwords and root login (sshd uses PermitRootLogin $root, PasswordAuthentication $pass). Waiting for the platform to test sshd from outside and confirm within ${SSH_CONFIRM_SECONDS}s; otherwise passwords come back."
}

# ─── Taking a machine off Wi-Fi, with the platform in the check ──────────────────────────────────────────────────────────────────
# A Nuit Cloud Server is wired. This turns a machine's wireless interfaces off and removes what remembers the network (the stanzas
# in /etc/network/interfaces*, and wpa_supplicant files that hold a passphrase), but only when a wired interface carries the
# default route AND the platform can still be reached with the wireless interfaces down. Order matters: the interfaces go down
# first and come straight back if the platform cannot be reached, and the configuration is removed only after that proof, because
# removing a passphrase cannot be undone.
wifi_ifaces() {
  for d in "$(p /sys/class/net)"/*; do
    [ -e "$d" ] || continue
    if [ -e "$d/wireless" ] || [ -e "$d/phy80211" ]; then basename "$d"; fi
  done
}
wifi_files() { # the ifupdown files and wpa_supplicant files that mention wireless
  for f in "$(p /etc/network/interfaces)" "$(p /etc/network/interfaces.d)"/* "$(p /etc/wpa_supplicant)"/*.conf; do
    [ -f "$f" ] && printf '%s\n' "$f"
  done
}
f_ncs_wifi_off_ok() {
  ifs="$(wifi_ifaces)"
  for i in $ifs; do
    [ "$(cat "$(p /sys/class/net)/$i/operstate" 2>/dev/null)" = up ] && return 1
    for f in $(wifi_files); do
      case "$f" in
        */wpa_supplicant/*) grep -Eq '^[[:space:]]*(psk|password)=' "$f" && return 1 ;;
        *) grep -Eq "^[[:space:]]*(auto|allow-hotplug|iface)[[:space:]]+$i([[:space:]]|\$)|^[[:space:]]*wpa-(ssid|psk|conf)" "$f" && return 1 ;;
      esac
    done
  done
  return 0
}
wifi_proof() {
  if [ -n "${NCS_PLATFORM_URL:-}" ]; then
    for _ in 1 2 3 4 5 6; do
      curl -fsS --max-time 15 -o /dev/null "$NCS_PLATFORM_URL/api/agent/download/VERSION" 2>/dev/null && return 0
      sleep 5
    done
    return 1
  fi
  return 1
}
wifi_default_dev() { _d="$(ip -4 route show default 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="dev"){print $(i+1); exit}}')"; [ -z "$_d" ] || addr_lower "$_d"; }
f_ncs_wifi_off_do() {
  ifs="$(wifi_ifaces)"
  [ -n "$ifs" ] || { echo "  no wireless interface here"; return 0; }
  dev="$(wifi_default_dev)"
  case " $(echo $ifs) " in *" $dev "*) echo "  the default route is on the wireless interface $dev: connect a cable first, then run this again" >&2; return 3 ;; esac
  [ -n "$dev" ] || { echo "  there is no default route: nothing is carrying the machine's traffic" >&2; return 3; }
  f_ncs_wired_ok || { echo "  no wired connection is up: connect a cable, then run this again" >&2; return 3; }
  [ -n "${NCS_PLATFORM_URL:-}" ] || { echo "  the platform's address was not passed in, so the change cannot be proved safe" >&2; return 3; }
  # 1. wireless interfaces down (reversible)
  for i in $ifs; do ip link set "$i" down; done
  # 2. proof: the platform must still be reachable by the wired path
  if ! wifi_proof; then
    for i in $ifs; do ip link set "$i" up; done
    echo "  the platform could not be reached with wireless off, so the wireless interfaces were switched back on" >&2
    return 1
  fi
  # 3. only now remove what remembers the network (not undoable)
  for i in $ifs; do
    for f in $(wifi_files); do
      case "$f" in
        */wpa_supplicant/*) grep -Eq '^[[:space:]]*(psk|password)=' "$f" && rm -f "$f" ;;
        *)
          awk -v ifc="$i" '
            function starts(l) { return l ~ /^[[:space:]]*(auto|allow-[a-z]+|iface|source|source-directory|mapping)[[:space:]]/ }
            { if (starts($0)) skip = ($0 ~ "^[[:space:]]*(auto|allow-[a-z]+|iface)[[:space:]]+" ifc "([[:space:]]|$)")
              if (skip) next
              print }' "$f" >"$f.nuit-cloud.new" && cat "$f.nuit-cloud.new" >"$f"; rm -f "$f.nuit-cloud.new" ;;
      esac
    done
  done
  command -v rfkill >/dev/null 2>&1 && rfkill block wifi 2>/dev/null || true
  echo "  wireless interfaces ($(echo $ifs)) are off, nothing remembers the network, and the platform answers over $dev"
}

# ─── The platform's SSH login ──────────────────────────────────────────────────────────────────────────────────────────────────────
# Keeps the platform's SSH public keys authorised for the login the platform uses, $NC_USER. The account is never created (MANUAL if it
# is missing), and the directory and file keep the owner and modes sshd insists on. Public keys only; nothing secret passes through,
# and no key body is ever printed. Two forms, from the signed task:
#
#   NCS_LOGIN_KEYS (the key registry, nuit-cloud/nuit-cloud#92): 1 to 64 words `<id>=<base64>`, each an ssh-ed25519 key. Each is
#     written as the line `ssh-ed25519 <base64> nuit-cloud:<id>`; that trailing comment marks a line as the platform's. The marked
#     lines are kept equal to the list: a missing key is added, a marked line whose id or key is no longer listed is removed (how a
#     revocation lands), and an unmarked line holding a listed key is adopted (rewritten as its marked line, in place). Every other
#     line is left exactly as it was. The new file is written beside the old and renamed over it, so no moment has a partial file.
#   NCS_PLATFORM_SSH_KEY (an older platform): one key line, added if missing; nothing is ever removed.
login_home() { getent passwd "$NC_USER" 2>/dev/null | cut -d: -f6; }
login_key_ok() { # the key's body (type and base64) is already authorised
  home="$(login_home)"; [ -n "$home" ] || return 1
  body="$(printf '%s' "$NCS_PLATFORM_SSH_KEY" | awk '{print $1" "$2}')"
  [ -f "$home/.ssh/authorized_keys" ] && awk -v b="$body" '{ if (($1" "$2)==b) f=1 } END { exit f?0:1 }' "$home/.ssh/authorized_keys"
}
# login_keys_valid: NCS_LOGIN_KEYS holds 1 to 64 `<id>=<ssh-ed25519 base64>` words with unique ids and unique keys (checked again here,
# as strictly as the agent checked the task). The character check comes first, so word splitting below can never glob.
login_keys_valid() {
  case "${NCS_LOGIN_KEYS:-}" in '' | *[!a-zA-Z0-9+/=\ -]*) return 1 ;; esac
  lk_n=0
  for lk_w in $NCS_LOGIN_KEYS; do
    printf '%s\n' "$lk_w" | grep -Eq '^[a-z0-9][a-z0-9-]{0,39}=AAAAC3NzaC1lZDI1NTE5AAAAI[A-Za-z0-9+/]{43}$' || return 1
    lk_n=$((lk_n + 1))
  done
  [ "$lk_n" -ge 1 ] && [ "$lk_n" -le 64 ] || return 1
  [ "$(printf '%s\n' $NCS_LOGIN_KEYS | cut -d= -f1 | sort -u | wc -l | tr -d ' ')" = "$lk_n" ] \
    && [ "$(printf '%s\n' $NCS_LOGIN_KEYS | cut -d= -f2 | sort -u | wc -l | tr -d ' ')" = "$lk_n" ]
}
# LOGIN_SYNC_AWK reads an authorized_keys file and the list (-v keys), and prints `<add> <remove> <adopt> <unmanaged>`; with -v out=<file>
# it also writes the file as it should be. A marked line is exactly three fields: a key type, the base64, `nuit-cloud:<id>`. It holds no
# dollar-brace, percent or doubled dollar, because systemd expands those in a transient unit's command line (a test checks).
LOGIN_SYNC_AWK='
function emit(line) { if (out != "") print line > out }
BEGIN {
  n = split(keys, ks, " ")
  for (i = 1; i <= n; i++) { eq = index(ks[i], "="); id = substr(ks[i], 1, eq - 1); ids[i] = id; body[id] = substr(ks[i], eq + 1); byBody[body[id]] = id }
  add = 0; rem = 0; adopt = 0; unmanaged = 0
}
NF == 3 && $1 ~ /^(ssh|ecdsa|sk)-/ && $3 ~ /^nuit-cloud:[a-z0-9][a-z0-9-]*$/ {
  id = substr($3, 12)
  if ((id in body) && $1 == "ssh-ed25519" && $2 == body[id] && !(id in have)) { have[id] = 1; emit($0) } else rem++
  next
}
NF >= 2 && $1 == "ssh-ed25519" && ($2 in byBody) {
  id = byBody[$2]; adopt++
  if (!(id in have)) { have[id] = 1; emit("ssh-ed25519 " $2 " nuit-cloud:" id) }
  next
}
{ if ($0 !~ /^[ \t]*(#|$)/) unmanaged++; emit($0) }
END {
  for (i = 1; i <= n; i++) if (!(ids[i] in have)) { add++; emit("ssh-ed25519 " body[ids[i]] " nuit-cloud:" ids[i]) }
  if (out != "") close(out)
  print add, rem, adopt, unmanaged
}'
login_counts() { # what keeping in step would do, changing nothing
  lk_f="$(login_home)/.ssh/authorized_keys"; [ -f "$lk_f" ] || lk_f=/dev/null
  awk -v keys="$NCS_LOGIN_KEYS" -v out= "$LOGIN_SYNC_AWK" "$lk_f"
}
f_ncs_login_ok() {
  if [ -n "${NCS_LOGIN_KEYS+set}" ]; then
    login_keys_valid || { [ "${MODE:-}" != check ] || echo "  the task's login keys are not usable (1 to 64 ssh-ed25519 keys, unique ids and keys)"; return 1; }
    f_user_ok || return 1
    lk_c="$(login_counts)" || return 1
    set -- $lk_c
    [ "$1" = 0 ] && [ "$2" = 0 ] && [ "$3" = 0 ] && return 0
    [ "${MODE:-}" != check ] || echo "  the platform's keys are not in step: $1 to add, $2 to remove, $3 to adopt; $4 unmanaged line(s) left alone"
    return 1
  fi
  [ -n "${NCS_PLATFORM_SSH_KEY:-}" ] && f_user_ok && login_key_ok
}
# The agent's service is sandboxed (ProtectHome=read-only), so the write is done by a transient systemd unit, which is not: it
# inherits nothing from the agent's mount namespace. Without systemd (and in tests) it runs directly.
LOGIN_WRITE='home="$1"; user="$2"; grp="$3"; key="$4"; dir="$home/.ssh"; file="$dir/authorized_keys"
mkdir -p "$dir" || exit 1; chmod 700 "$dir" || exit 1
[ -f "$file" ] || : >"$file" || exit 1
body="$(printf "%s" "$key" | awk "{print \$1\" \"\$2}")"
if ! awk -v b="$body" "{ if ((\$1\" \"\$2)==b) f=1 } END { exit f?0:1 }" "$file"; then
  if [ -s "$file" ] && [ "$(tail -c1 "$file" | wc -l | tr -d " ")" = 0 ]; then echo >>"$file" || exit 1; fi
  printf "%s\n" "$key" >>"$file" || exit 1
fi
chmod 600 "$file" || exit 1
chown "$user:$grp" "$dir" "$file" 2>/dev/null || [ -n "${ROOT:-}" ]'
# LOGIN_SYNC keeps the registry's keys in step: the new file is written in the same directory (so the rename is atomic), given mode 600
# and the account as owner, and only then renamed over the old one; on any failure the old file stays and the temporary one goes. It
# prints the counts. Like LOGIN_SYNC_AWK it holds no dollar-brace, percent or doubled dollar (systemd would expand them).
LOGIN_SYNC='home="$1"; user="$2"; grp="$3"; keys="$4"; prog="$5"; dir="$home/.ssh"; file="$dir/authorized_keys"
[ ! -L "$dir" ] || { echo "  $dir is a symbolic link: left to a person" >&2; exit 1; }
[ ! -d "$file" ] || { echo "  $file is a directory: left to a person" >&2; exit 1; }
mkdir -p "$dir" || exit 1; chmod 700 "$dir" || exit 1
chown "$user:$grp" "$dir" 2>/dev/null || [ -n "$ROOT" ] || exit 1
src="$file"; [ -f "$file" ] || src=/dev/null
tmp="$(mktemp "$dir/.authorized_keys.nuit-cloud.XXXXXX")" || exit 1
if counts="$(awk -v keys="$keys" -v out="$tmp" "$prog" "$src")" && chmod 600 "$tmp" && { chown "$user:$grp" "$tmp" 2>/dev/null || [ -n "$ROOT" ]; } && mv -f "$tmp" "$file"; then
  echo "$counts"
else
  rm -f "$tmp"; exit 1
fi'
f_ncs_login_do() {
  if [ -n "${NCS_LOGIN_KEYS+set}" ]; then
    login_keys_valid || { echo "  the task's login keys are not usable (1 to 64 ssh-ed25519 keys, unique ids and keys): nothing was changed" >&2; return 3; }
    f_user_ok || { echo "  the account $NC_USER does not exist: create it first (adduser $NC_USER), then run this again" >&2; return 3; }
    home="$(login_home)"; grp="$(id -gn "$NC_USER" 2>/dev/null || echo "$NC_USER")"
    if [ -z "${ROOT:-}" ] && [ -d /run/systemd/system ] && command -v systemd-run >/dev/null 2>&1; then
      lk_c="$(systemd-run --quiet --wait --collect --pipe /bin/sh -c "$LOGIN_SYNC" sh "$home" "$NC_USER" "$grp" "$NCS_LOGIN_KEYS" "$LOGIN_SYNC_AWK")" || return 1
    else
      lk_c="$(sh -c "$LOGIN_SYNC" sh "$home" "$NC_USER" "$grp" "$NCS_LOGIN_KEYS" "$LOGIN_SYNC_AWK")" || return 1
    fi
    set -- $lk_c
    echo "  the platform's keys are in step for $NC_USER: $1 added, $2 removed, $3 adopted; $4 unmanaged line(s) left alone"
    return 0
  fi
  case "${NCS_PLATFORM_SSH_KEY:-}" in ssh-ed25519\ *) ;; *) echo "  the task carries no ssh-ed25519 public key" >&2; return 3 ;; esac
  case "$NCS_PLATFORM_SSH_KEY" in *'
'*) echo "  the key must be a single line" >&2; return 3 ;; esac
  f_user_ok || { echo "  the account $NC_USER does not exist: create it first (adduser $NC_USER), then run this again" >&2; return 3; }
  home="$(login_home)"; grp="$(id -gn "$NC_USER" 2>/dev/null || echo "$NC_USER")"
  if [ -z "${ROOT:-}" ] && [ -d /run/systemd/system ] && command -v systemd-run >/dev/null 2>&1; then
    systemd-run --quiet --wait --collect --pipe /bin/sh -c "$LOGIN_WRITE" sh "$home" "$NC_USER" "$grp" "$NCS_PLATFORM_SSH_KEY" || return 1
  else
    sh -c "$LOGIN_WRITE" sh "$home" "$NC_USER" "$grp" "$NCS_PLATFORM_SSH_KEY" || return 1
  fi
  echo "  the platform's key is authorised for $NC_USER (other keys left as they were)"
}

# ─── A baseline firewall that leaves the platform's ports open and nothing else ───────────────────────────────────────────────────
# One nftables table (`inet nuit_cloud`) with an INPUT chain that drops by default and accepts: loopback, the container bridges (pods keep
# the host as their gateway and resolver, as before), replies to what the machine started, ICMP, DHCP, SSH from the platform's own
# ranges (NCS_FIREWALL_SSH_FROM, default the private ranges) and the TCP ports the task names (NCS_FIREWALL_TCP, e.g. 80 443 on a machine
# that serves routes; open to anyone). Forwarded traffic is not touched: a pod's published ports are DNATed and forwarded, so the
# tenants' services are unaffected and nothing here isolates tenants from each other (that is its own design, issue #91).
# Pod rules (nuit-cloud/nuit-cloud#91), only when the task names them (NCS_FIREWALL_POD_MODE; without it the ruleset is exactly the above):
# INPUT from the bridges takes DNS, DHCP and ICMP and counts the rest as `pod_input`; a forward chain (policy accept, so other tables keep
# their own say) lets pods reach their own range (10.89.0.0/16) and what NCS_FIREWALL_POD_ALLOW names, counts the rest of the private
# ranges as `pod_private`, and lets only NCS_FIREWALL_ADMIN_FROM reach the platform services' admin ports (DNATed from
# NCS_FIREWALL_ADMIN_TARGETS : NCS_FIREWALL_ADMIN_PORTS), counting the rest as `admin_other`. In `observe` mode every one of those is
# logged (prefix `nc-<counter> `, rate-limited) and counted but still accepted, so nothing that passes today stops passing; in `enforce`
# mode it is dropped. The internet is never touched. The mode is written in the file's header (`# pod-mode: ...`), which the agent's
# `firewall` observation reads.
# Like the SSH change, this can lock everyone out, so: the rules are syntax-checked first; a timer (nuit-cloud-firewall-rollback) that
# deletes the table is armed before anything is loaded; after loading, sshd must still be listening; and the timer is left ARMED
# until the platform has tested from outside (SSH reachable, the open ports not blocked, a port nobody opened dropped) and sent
# `firewall-confirm`. If that test fails or never happens, the firewall removes itself. The rules are reloaded at boot by a unit.
FW_TABLE="${FW_TABLE:-nuit_cloud}"
FW_RULES="${FW_RULES:-/etc/nuit-cloud/firewall.nft}"
FW_UNIT="${FW_UNIT:-/etc/systemd/system/nuit-cloud-firewall.service}"
FW_CONFIRM_SECONDS="${NCS_FIREWALL_CONFIRM_SECONDS:-600}"
FW_DEFAULT_SSH_FROM="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"

fw_words() { printf '%s' "$1" | tr ',' ' '; }
fw_ports() { fw_words "${NCS_FIREWALL_TCP:-}" | tr -s ' ' '\n' | grep -v '^$' | sort -n -u | tr '\n' ' '; }
fw_ssh_from() { v="$(fw_words "${NCS_FIREWALL_SSH_FROM:-}")"; [ -n "$v" ] || v="$FW_DEFAULT_SSH_FROM"; printf '%s' "$v" | tr -s ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' '; }
FW_BRIDGES='{ "ocisaas0", "cni0", "docker0", "br-*" }'
FW_POD_RANGE="10.89.0.0/16"
FW_PRIVATE="10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16"
fw_list() { fw_words "$1" | tr -s ' ' '\n' | grep -v '^$' | sort -u | tr '\n' ' '; }
fw_ipv4_ok() { printf '%s' "$1" | grep -Eq '^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$'; }
fw_net_ok() { case "$1" in */*) fw_ipv4_ok "${1%/*}" && printf '%s' "${1#*/}" | grep -Eq '^(3[0-2]|[12]?[0-9])$';; *) fw_ipv4_ok "$1";; esac; }
fw_port_ok() { printf '%s' "$1" | grep -Eq '^[1-9][0-9]{0,4}$' && [ "$1" -le 65535 ]; }
fw_allow_ok() { case "$1" in *:*) fw_net_ok "${1%:*}" && fw_port_ok "${1##*:}";; *) fw_net_ok "$1";; esac; }
# a comma list of at most $2 entries (at least $3), only digits, dots, slashes and colons: nothing a shell or nft could read as more
fw_csv_ok() {
  case "$1" in *[!0-9./:,]*|,*|*,|*,,*) return 1;; esac
  fw_n="$(printf '%s' "$1" | tr ',' '\n' | grep -c .)" || true
  [ "$fw_n" -le "$2" ] && [ "$fw_n" -ge "$3" ]
}
fw_pod_inputs_ok() {
  fw_m="${NCS_FIREWALL_POD_MODE:-}"; fw_a="${NCS_FIREWALL_POD_ALLOW:-}"
  fw_t="${NCS_FIREWALL_ADMIN_TARGETS:-}"; fw_ap="${NCS_FIREWALL_ADMIN_PORTS:-}"; fw_af="${NCS_FIREWALL_ADMIN_FROM:-}"
  if [ -z "$fw_m" ]; then [ -z "$fw_a$fw_t$fw_ap$fw_af" ]; return; fi
  case "$fw_m" in observe|enforce) ;; *) return 1;; esac
  fw_csv_ok "$fw_a" 32 0 || return 1
  for fw_e in $(fw_list "$fw_a"); do fw_allow_ok "$fw_e" || return 1; done
  [ -n "$fw_t$fw_ap$fw_af" ] || return 0
  fw_csv_ok "$fw_t" 16 1 && fw_csv_ok "$fw_ap" 8 1 && fw_csv_ok "$fw_af" 16 1 || return 1
  for fw_e in $(fw_list "$fw_t"); do fw_ipv4_ok "$fw_e" || return 1; done
  for fw_e in $(fw_list "$fw_ap"); do fw_port_ok "$fw_e" || return 1; done
  for fw_e in $(fw_list "$fw_af"); do fw_net_ok "$fw_e" || return 1; done
}
fw_inputs_ok() {
  for pt in $(fw_ports); do printf '%s' "$pt" | grep -Eq '^[0-9]{1,5}$' && [ "$pt" -ge 1 ] && [ "$pt" -le 65535 ] || return 1; done
  for c in $(fw_ssh_from); do printf '%s' "$c" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$' || return 1; done
  fw_pod_inputs_ok
}
fw_set() { fw_list "$1" | sed 's/ *$//; s/ /, /g'; } # a sorted, de-duplicated nft set body
fw_ports_set() { fw_words "$1" | tr -s ' ' '\n' | grep -v '^$' | sort -n -u | tr '\n' ' ' | sed 's/ *$//; s/ /, /g'; }
# one counted rule: `<match>` logged (at most 10 a minute, the log rule has no verdict so the count and verdict below still apply to every
# packet) and counted under <counter>; accepted in observe mode, dropped in enforce mode
fw_counted() {
  fw_verdict=accept; [ "$NCS_FIREWALL_POD_MODE" != enforce ] || fw_verdict=drop
  echo "    $2 limit rate 10/minute log prefix \"nc-$1 \""
  echo "    $2 counter name $1 $fw_verdict"
}
fw_render_pod_input() {
  echo "    iifname $FW_BRIDGES udp dport { 53, 67 } accept"
  echo "    iifname $FW_BRIDGES tcp dport 53 accept"
  echo "    iifname $FW_BRIDGES meta l4proto { icmp, ipv6-icmp } accept"
  fw_counted pod_input "iifname $FW_BRIDGES"
}
fw_render_forward() {
  echo "  chain forward {"
  echo "    type filter hook forward priority 0; policy accept;"
  echo "    ct state established,related accept"
  if [ -n "${NCS_FIREWALL_ADMIN_TARGETS:-}" ]; then
    fw_admin="ct status dnat ct original ip daddr { $(fw_set "$NCS_FIREWALL_ADMIN_TARGETS") } ct original proto-dst { $(fw_ports_set "$NCS_FIREWALL_ADMIN_PORTS") }"
    echo "    $fw_admin ip saddr { $(fw_set "$NCS_FIREWALL_ADMIN_FROM") } accept"
    fw_counted admin_other "$fw_admin"
  fi
  echo "    iifname $FW_BRIDGES ip daddr $FW_POD_RANGE accept"
  # Containers on this machine talking to each other (Docker's own networks, a pod reaching a service published on this node): in on a bridge
  # and out on a bridge never leaves the machine. After the admin rules, so a local pod still cannot reach the platform services' admin ports.
  echo "    iifname $FW_BRIDGES oifname $FW_BRIDGES accept"
  for fw_e in $(fw_list "${NCS_FIREWALL_POD_ALLOW:-}"); do
    case "$fw_e" in
      *:*) echo "    iifname $FW_BRIDGES ip daddr ${fw_e%:*} meta l4proto { tcp, udp } th dport ${fw_e##*:} accept";;
      *) echo "    iifname $FW_BRIDGES ip daddr $fw_e accept";;
    esac
  done
  fw_counted pod_private "iifname $FW_BRIDGES ip daddr { $FW_PRIVATE }"
  echo "  }"
}
fw_render() { # the ruleset on stdout
  v4="$(fw_ssh_from | tr -s ' ' ',' | sed 's/,$//')"
  fw_pods="${NCS_FIREWALL_POD_MODE:-}"
  echo "# Written by the nuit-cloud agent (profile nuit-cloud-firewall). Edit the task, not this file."
  [ -z "$fw_pods" ] || echo "# pod-mode: $fw_pods"
  echo "table inet $FW_TABLE"
  echo "delete table inet $FW_TABLE"
  echo "table inet $FW_TABLE {"
  if [ -n "$fw_pods" ]; then
    for c in pod_input pod_private admin_other; do echo "  counter $c {"; echo "  }"; done
  fi
  echo "  chain input {"
  echo "    type filter hook input priority 0; policy drop;"
  echo "    iif lo accept"
  if [ -n "$fw_pods" ]; then
    echo "    ct state established,related accept"
    fw_render_pod_input
  else
    echo "    iifname $FW_BRIDGES accept"
    echo "    ct state established,related accept"
  fi
  echo "    ct state invalid drop"
  echo "    ip protocol icmp accept"
  echo "    ip6 nexthdr icmpv6 accept"
  echo "    udp dport 68 accept"
  echo "    tcp dport 22 ip saddr { $v4 } accept"
  echo "    tcp dport 22 ip6 saddr { fc00::/7, fe80::/10 } accept"
  ports="$(fw_ports | sed 's/ *$//' | tr -s ' ' ',')"
  [ -z "$ports" ] || echo "    tcp dport { $ports } accept"
  echo "  }"
  [ -z "$fw_pods" ] || fw_render_forward
  echo "}"
}
fw_loaded() { nft list table inet "$FW_TABLE" >/dev/null 2>&1; }
f_ncs_firewall_ok() {
  [ -f "$(p "$FW_RULES")" ] && fw_inputs_ok && [ "$(fw_render)" = "$(cat "$(p "$FW_RULES")")" ] && fw_loaded && systemctl is-enabled --quiet nuit-cloud-firewall 2>/dev/null
}
f_ncs_firewall_do() {
  fw_inputs_ok || { echo "  the task's ports, ranges or pod rules are not valid (ports 1-65535, ranges like 10.0.0.0/8, pod mode observe or enforce, at most 32 allowed destinations; admin targets, ports and sources all given or none)" >&2; return 3; }
  command -v systemd-run >/dev/null 2>&1 || { echo "  systemd-run is needed for the safety timer" >&2; return 3; }
  if ! command -v nft >/dev/null 2>&1; then apt-get install -y -qq nftables >/dev/null 2>&1 || true; fi
  command -v nft >/dev/null 2>&1 || { echo "  nftables is not installed and could not be installed" >&2; return 3; }
  port="$(sshd_effective port)"; port="${port:-22}"
  [ "$port" = 22 ] || { echo "  sshd listens on port $port, not 22: the baseline only opens 22, so set it up by hand" >&2; return 3; }
  d="$(p "$ADDR_DIR")/firewall"; rm -rf "$d"; mkdir -p "$d"; chmod 700 "$d"
  rules="$(p "$FW_RULES")"; mkdir -p "$(dirname "$rules")"
  [ -f "$rules" ] && cp -p "$rules" "$d/previous-rules"
  fw_render >"$d/new.nft"
  nft -c -f "$d/new.nft" 2>"$d/check.out" || { echo "  nft rejected the rules ($(tr '\n' ' ' <"$d/check.out" | cut -c1-160)): nothing was changed" >&2; return 1; }
  cat >"$d/rollback.sh" <<ROLLBACK
#!/bin/sh
# Take the firewall away again. Run by the safety timer, or at once if the change could not be proved to work.
PATH="\$PATH:/usr/local/sbin:/usr/sbin:/sbin"
echo "nuit-cloud: removing the firewall"
nft delete table inet $FW_TABLE 2>/dev/null
systemctl disable nuit-cloud-firewall 2>/dev/null
if [ -f "$d/previous-rules" ]; then cp -p "$d/previous-rules" "$rules"; else rm -f "$rules"; fi
rm -f "$d/armed"
exit 0
ROLLBACK
  chmod 700 "$d/rollback.sh"
  systemd-run --quiet --unit=nuit-cloud-firewall-rollback --collect --on-active="${FW_CONFIRM_SECONDS}s" /bin/sh "$d/rollback.sh" || { echo "  could not arm the safety timer; nothing was changed" >&2; return 1; }
  : >"$d/armed"
  cp "$d/new.nft" "$rules"; chmod 644 "$rules"
  unit="$(p "$FW_UNIT")"; mkdir -p "$(dirname "$unit")"
  cat >"$unit" <<UNIT
[Unit]
Description=Nuit Cloud baseline firewall
Before=network-pre.target
Wants=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f $FW_RULES

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload 2>/dev/null || true
  if ! nft -f "$rules"; then sh "$d/rollback.sh"; echo "  nft could not load the rules, so they were taken away" >&2; return 1; fi
  fw_loaded || { sh "$d/rollback.sh"; echo "  the table is not there after loading, so the change was taken away" >&2; return 1; }
  ss -ltnH 2>/dev/null | awk '{print $4}' | grep -q ":22\$" || { sh "$d/rollback.sh"; echo "  sshd is not listening on port 22, so the firewall was taken away" >&2; return 1; }
  systemctl enable nuit-cloud-firewall >/dev/null 2>&1 || true
  echo "  the firewall is loaded: SSH from $(fw_ssh_from)${NCS_FIREWALL_TCP:+, TCP $(fw_ports)}, nothing else inbound${NCS_FIREWALL_POD_MODE:+; pod rules in $NCS_FIREWALL_POD_MODE mode}. Waiting for the platform to test from outside and confirm within ${FW_CONFIRM_SECONDS}s; otherwise it is removed."
}

# ─── Weekly housekeeping ────────────────────────────────────────────────────────────────────────────────────────────────────────
# A systemd timer (a minimal Debian has no cron) that once a week empties the apt package cache, drops packages nothing needs and trims
# the journal to JOURNAL_MAX. It removes nothing a running service uses; container images belong to the container runtime's own cleanup.
f_ncs_housekeeping_ok() { [ -e "$(p /etc/systemd/system/nuit-cloud-housekeeping.timer)" ] && [ -e "$(p /etc/systemd/system/nuit-cloud-housekeeping.service)" ]; }
f_ncs_housekeeping_do() {
  write_if_missing /etc/systemd/system/nuit-cloud-housekeeping.service <<UNIT
[Unit]
Description=Nuit Cloud weekly housekeeping

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'apt-get clean; apt-get -y -qq autoremove --purge; journalctl --vacuum-size=${JOURNAL_MAX}'
UNIT
  write_if_missing /etc/systemd/system/nuit-cloud-housekeeping.timer <<'UNIT'
[Unit]
Description=Weekly Nuit Cloud housekeeping

[Timer]
OnCalendar=weekly
Persistent=true
RandomizedDelaySec=1h

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now nuit-cloud-housekeeping.timer
}

# A laptop shut on a shelf is a server whose lid switch suspends it: systemd's default is to sleep when the lid closes (and on idle with no one logged in on some installs), and a suspended
# machine answers nothing (a laptop that held a Pi-hole went dark this way, and its agent, its checks and its DNS went with it). On a laptop this stops every sleep: the lid, idle and the sleep
# keys are ignored and the sleep targets are masked so nothing else can suspend it either. A machine that is not a laptop is already as it should be.
NCS_LID_CONF=/etc/systemd/logind.conf.d/10-nuit-cloud-lid.conf
ncs_is_laptop() {
  t="$(cat "$(p /sys/class/dmi/id/chassis_type)" 2>/dev/null || true)"
  case "$t" in 8|9|10|14|30|31|32) return 0;; esac   # portable, laptop, notebook, sub-notebook, tablet, convertible, detachable
  for b in "$(p /sys/class/power_supply)"/BAT*; do [ -e "$b" ] && return 0; done
  return 1
}
f_ncs_laptop_awake_ok() {
  ncs_is_laptop || return 0
  c="$(p "$NCS_LID_CONF")"
  [ -e "$c" ] || return 1
  for line in HandleLidSwitch=ignore HandleLidSwitchExternalPower=ignore HandleLidSwitchDocked=ignore IdleAction=ignore; do grep -qx "$line" "$c" || return 1; done
  for t in sleep suspend hibernate hybrid-sleep; do [ "$(systemctl is-enabled "$t.target" 2>/dev/null || true)" = masked ] || return 1; done
  return 0
}
f_ncs_laptop_awake_do() {
  mkdir -p "$(dirname "$(p "$NCS_LID_CONF")")"
  cat >"$(p "$NCS_LID_CONF")" <<'CONF'
# Written by nuit-cloud (the ncs_laptop_awake feature): a laptop used as a server must not sleep, whatever its lid or its idle time.
[Login]
HandleLidSwitch=ignore
HandleLidSwitchExternalPower=ignore
HandleLidSwitchDocked=ignore
HandleSuspendKey=ignore
HandleHibernateKey=ignore
IdleAction=ignore
CONF
  systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
  # logind reads its configuration again on SIGHUP, without ending anyone's session
  systemctl kill -s HUP systemd-logind.service >/dev/null 2>&1 || true
}

# ─── Working name resolution for a machine with a static address ──────────────────────────────────────────────────────────────────────────
# A machine that took its resolvers from DHCP and was then given a static address keeps the file dhcpcd wrote, naming whatever DHCP offered
# that day (found 6 Nov 2026: every machine still listed two Pi-holes that had been removed, so none could resolve a name). The platform
# names the resolvers (NCS_DNS_SERVERS, one to three IPv4 addresses, taken from its own setting and never from a request) and this makes
# /etc/resolv.conf list exactly those, in that order. A `domain`/`search` line already there is kept. The same servers go in
# resolv.conf.head, which dhcpcd puts first whenever it writes the file again, so a later lease cannot put the dead ones back in front.
# A resolv.conf that is a symlink (systemd-resolved) is somebody else's to manage: left to a person.
NCS_RESOLV_CONF="${NCS_RESOLV_CONF:-/etc/resolv.conf}"
resolver_words() { printf '%s' "${NCS_DNS_SERVERS:-}" | tr ',' ' ' | tr -s ' ' '\n' | grep -v '^$'; }
resolvers_valid() {
  n=0
  for a in $(resolver_words); do
    printf '%s' "$a" | grep -Eq '^([0-9]{1,3}\.){3}[0-9]{1,3}$' || return 1
    n=$((n + 1))
  done
  [ "$n" -ge 1 ] && [ "$n" -le 3 ]
}
resolvers_current() { awk '$1=="nameserver" {print $2}' "$(p "$NCS_RESOLV_CONF")" 2>/dev/null; }
f_ncs_resolvers_ok() {
  [ -n "${NCS_DNS_SERVERS:-}" ] || return 0   # nothing is asked: there is nothing to be out of line with
  resolvers_valid || return 1
  [ "$(resolvers_current)" = "$(resolver_words)" ]
}
f_ncs_resolvers_do() {
  resolvers_valid || { echo "  the task carries no usable resolver list (one to three IPv4 addresses)" >&2; return 3; }
  f="$(p "$NCS_RESOLV_CONF")"
  if [ -L "$f" ]; then echo "  $NCS_RESOLV_CONF is a symlink (systemd-resolved?): set its resolvers where it is managed, then run this again" >&2; return 3; fi
  mkdir -p "$(dirname "$f")"
  keep="$(awk '$1=="domain" || $1=="search"' "$f" 2>/dev/null || true)"
  names="$(resolver_words | sed 's/^/nameserver /')"
  tmp="$f.nuit-cloud.$$"
  { echo "# Written by nuit-cloud (the ncs_resolvers feature): the platform's resolvers."; [ -z "$keep" ] || echo "$keep"; echo "$names"; } >"$tmp" && chmod 644 "$tmp" && mv "$tmp" "$f" || return 1
  { echo "# Written by nuit-cloud (the ncs_resolvers feature): kept first when dhcpcd writes resolv.conf again."; echo "$names"; } >"$f.head" || return 1
  echo "  resolvers set to: $(resolver_words | tr '\n' ' ')"
}

# ─── Services a Control Plane Server runs for the platform (the nuit-cloud-control-plane profile; docs/control-plane/control-plane-services.md) ───
# The scheduler (Ofelia and the sweep scripts), the agent's https front (Debian's nginx forwarding only /api/agent/) and a dyndns probe, each
# a NATIVE systemd service (no container engine) that starts at boot with no platform. The signed task names the services and the bundle
# (NCS_CP_BUNDLE_VERSION, NCS_CP_BUNDLE_SHA256) that holds Ofelia, the scheduler's scripts and configuration and the probe's script; the AGENT has
# already downloaded it, checked its checksum and unpacked it to <NCS_CP_INSTALL_DIR>/<version>/, and has written the secrets to root-only files
# under NCS_CP_CONFIG_DIR (scheduler.env, probe.env, tls/cert.pem, tls/key.pem) and a `revision` file. It tells these features only where those
# are (NCS_CP_CONFIG_DIR, NCS_CP_INSTALL_DIR), which revision it fetched (NCS_CP_REVISION) and the NAMES of secrets the platform did not have
# (NCS_CP_MISSING). Nothing here reads those files out loud: a feature never prints or copies an env file or a key; a unit points at it with
# EnvironmentFile= (read by systemd as root before the service drops its privileges) and nginx reads its own certificate and key.
# `applied-<service>` (the revision and bundle a service was last started under) is how a changed configuration is told from an unchanged one.
# `current` (a symlink to the unpacked version in use) is switched atomically; `previous` keeps the one before for rollback; older ones are deleted.
cp_dir() { printf '%s' "${NCS_CP_CONFIG_DIR:-/etc/nuit-cloud/control-plane}"; }
cp_install() { printf '%s' "${NCS_CP_INSTALL_DIR:-/opt/nuit-cloud/control-plane}"; }
cp_unit_file() { p "/etc/systemd/system/nuit-cloud-cp-$1.service"; }
cp_wants() { case " ${NCS_CP_SERVICES:-} " in *" $1 "*) return 0 ;; esac; return 1; }
cp_secret_name() {
  case "$1" in
    scheduler) echo control-plane/scheduler ;;
    tls-front) echo control-plane/agent-tls ;;
    probe) echo control-plane/probe ;;
  esac
}
cp_is_missing() { case " ${NCS_CP_MISSING:-} " in *" $(cp_secret_name "$1") "*) return 0 ;; esac; return 1; }
cp_revision_on_disk() { cat "$(cp_dir)/revision" 2>/dev/null || true; }
cp_applied() { cat "$(cp_dir)/applied-$1" 2>/dev/null || true; }
cp_marker() { # what a service was started under: the revision, and for the bundle's services the bundle too
  if [ "$1" = tls-front ]; then printf '%s' "${NCS_CP_REVISION:-}"; else printf '%s %s' "${NCS_CP_REVISION:-}" "${NCS_CP_BUNDLE_SHA256:-}"; fi
}
cp_bundle_valid() {
  printf '%s' "${NCS_CP_BUNDLE_VERSION:-}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$' && printf '%s' "${NCS_CP_BUNDLE_SHA256:-}" | grep -Eq '^[0-9a-f]{64}$'
}
# `current` is the signed bundle: its VERSION and the checksum the agent recorded when it unpacked it are the task's
cp_bundle_current() {
  d="$(cp_install)/current"
  [ -d "$d" ] && [ "$(cat "$d/VERSION" 2>/dev/null || true)" = "$NCS_CP_BUNDLE_VERSION" ] && [ "$(cat "$d/.bundle-sha256" 2>/dev/null || true)" = "$NCS_CP_BUNDLE_SHA256" ]
}
cp_bundle_unpacked() {
  d="$(cp_install)/$NCS_CP_BUNDLE_VERSION"
  [ -d "$d" ] && [ "$(cat "$d/.bundle-sha256" 2>/dev/null || true)" = "$NCS_CP_BUNDLE_SHA256" ] && [ "$(cat "$d/VERSION" 2>/dev/null || true)" = "$NCS_CP_BUNDLE_VERSION" ]
}
# cp_symlink_swap <target> <link>: point <link> at <target> atomically (a symlink made beside it, then renamed over it)
cp_symlink_swap() {
  rm -f "$2.nuit-new"
  ln -s "$1" "$2.nuit-new" || return 1
  if mv -T "$2.nuit-new" "$2" 2>/dev/null; then return 0; fi
  mv -fh "$2.nuit-new" "$2" 2>/dev/null && return 0
  rm -f "$2.nuit-new"; return 1
}
# Make `current` the signed version (keeping the one it was as `previous`) and delete every other unpacked version.
cp_bundle_switch() {
  inst="$(cp_install)"
  cp_bundle_unpacked || { echo "  the bundle $NCS_CP_BUNDLE_VERSION is not unpacked under $inst (the agent does that when it applies)" >&2; return 1; }
  old="$(readlink "$inst/current" 2>/dev/null || true)"
  if [ "$old" != "$NCS_CP_BUNDLE_VERSION" ]; then
    if [ -n "$old" ] && [ -d "$inst/$old" ]; then cp_symlink_swap "$old" "$inst/previous" || return 1; fi
    cp_symlink_swap "$NCS_CP_BUNDLE_VERSION" "$inst/current" || return 1
  fi
  keep1="$(readlink "$inst/current" 2>/dev/null || true)"; keep2="$(readlink "$inst/previous" 2>/dev/null || true)"
  for dd in "$inst"/*; do
    [ -d "$dd" ] && [ ! -L "$dd" ] || continue
    bb="$(basename "$dd")"
    [ "$bb" = "$keep1" ] || [ "$bb" = "$keep2" ] || rm -rf "$dd"
  done
  return 0
}
cp_user_ok() { id -u "$1" >/dev/null 2>&1; }
cp_user_make() { cp_user_ok "$1" || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$1"; }
cp_pkgs_ok() { for pk in "$@"; do dpkg -s "$pk" >/dev/null 2>&1 || return 1; done; }
cp_pkgs_do() { cp_pkgs_ok "$@" && return 0; net_resolves || return 3; apt-get update -qq && apt-get install -y -qq "$@"; }
# cp_put <file> <mode>: content on stdin; replaces the file (via a temp file beside it) only if it differs; returns 10 when it did
cp_put() {
  cp_f="$1"; cp_mode="$2"; mkdir -p "$(dirname "$cp_f")"
  cat >"$cp_f.nuit-new" || { rm -f "$cp_f.nuit-new"; return 1; }
  if [ -f "$cp_f" ] && cmp -s "$cp_f.nuit-new" "$cp_f"; then rm -f "$cp_f.nuit-new"; return 0; fi
  chmod "$cp_mode" "$cp_f.nuit-new" && mv "$cp_f.nuit-new" "$cp_f" || return 1
  return 10
}

# What a unit must not be able to do: gain privileges, write anywhere but its own state, see home directories or devices, change the kernel,
# or open anything but IP and local sockets. No capabilities at all.
cp_hardening() {
  cat <<HARD
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
HARD
}

cp_render_scheduler() {
  cat <<UNIT
# Written by nuit-cloud (the ncs_cp_scheduler feature). Starts at boot, with no platform.
[Unit]
Description=Nuit Cloud control plane: scheduler (Ofelia)
After=network-online.target
Wants=network-online.target

[Service]
User=ncs-scheduler
EnvironmentFile=$(cp_dir)/scheduler.env
Environment=HOME=/tmp
ExecStart=$(cp_install)/current/ofelia daemon --config /etc/ofelia/config.ini
Restart=always
RestartSec=10
$(cp_hardening)
MemoryMax=128M
TasksMax=64

[Install]
WantedBy=multi-user.target
UNIT
}

cp_render_probe() {
  cat <<UNIT
# Written by nuit-cloud (the ncs_cp_probe feature). Starts at boot, with no platform.
[Unit]
Description=Nuit Cloud control plane: dyndns probe
After=network-online.target
Wants=network-online.target

[Service]
User=ncs-probe
EnvironmentFile=$(cp_dir)/probe.env
StateDirectory=nuit-cloud-probe
StateDirectoryMode=0700
Environment=TOKEN_STATE_FILE=/var/lib/nuit-cloud-probe/update-token
Environment=PROBE_VERSION=$NCS_CP_BUNDLE_VERSION
Environment=HOME=/tmp
ExecStart=/bin/sh $(cp_install)/current/probe/probe.sh
Restart=always
RestartSec=10
$(cp_hardening)
MemoryMax=64M
TasksMax=32

[Install]
WantedBy=multi-user.target
UNIT
}

# The scripts and configuration of the scheduler go to /etc/ofelia, where config.ini says they are (`sh /etc/ofelia/<name>.sh`).
cp_ofelia_etc() { p /etc/ofelia; }
cp_ofelia_src() { printf '%s' "$(cp_install)/current/scheduler"; }
cp_ofelia_files_ok() {
  src="$(cp_ofelia_src)"; [ -d "$src" ] || return 1
  for ff in "$src"/*; do [ -f "$ff" ] || continue; cmp -s "$ff" "$(cp_ofelia_etc)/$(basename "$ff")" || return 1; done
  return 0
}
# returns 10 when it changed something
cp_ofelia_files_do() {
  src="$(cp_ofelia_src)"; changed_any=0; mkdir -p "$(cp_ofelia_etc)" && chmod 755 "$(cp_ofelia_etc)" || return 1
  for ff in "$src"/*; do
    [ -f "$ff" ] || continue
    cp_rc=0; cp_put "$(cp_ofelia_etc)/$(basename "$ff")" 644 <"$ff" || cp_rc=$?
    [ "$cp_rc" = 10 ] && changed_any=1
    [ "$cp_rc" = 0 ] || [ "$cp_rc" = 10 ] || return 1
  done
  [ "$changed_any" = 1 ] && return 10
  return 0
}

# cp_bundle_service_ok <service> <user> <render function> <env file name> <packages...>
cp_bundle_service_ok() {
  svc="$1"; user="$2"; render="$3"; envfile="$4"; shift 4
  cp_wants "$svc" || return 0
  cp_bundle_valid || return 1
  if cp_is_missing "$svc"; then return 1; fi
  [ -n "${NCS_CP_REVISION:-}" ] && [ "$(cp_revision_on_disk)" = "$NCS_CP_REVISION" ] || return 1
  [ -s "$(cp_dir)/$envfile" ] || return 1
  cp_user_ok "$user" || return 1
  cp_pkgs_ok "$@" || return 1
  cp_bundle_current || return 1
  if [ "$svc" = scheduler ]; then cp_ofelia_files_ok || return 1; fi
  "$render" | cmp -s - "$(cp_unit_file "$svc")" || return 1
  systemctl is-enabled --quiet "nuit-cloud-cp-$svc.service" || return 1
  systemctl is-active --quiet "nuit-cloud-cp-$svc.service" || return 1
  [ "$(cp_applied "$svc")" = "$(cp_marker "$svc")" ]
}

cp_bundle_service_do() { # same arguments
  svc="$1"; user="$2"; render="$3"; envfile="$4"; shift 4
  unit="nuit-cloud-cp-$svc.service"; changed=0; unit_changed=0
  cp_wants "$svc" || return 0
  cp_bundle_valid || { echo "  the task carries no usable bundle (version and sha256)" >&2; return 3; }
  if cp_is_missing "$svc"; then
    echo "  the platform has no usable secret for $svc yet ($(cp_secret_name "$svc")): put it there, then apply this again" >&2
    return 3
  fi
  [ -n "${NCS_CP_REVISION:-}" ] || { echo "  the task carries no configuration revision" >&2; return 3; }
  { [ "$(cp_revision_on_disk)" = "$NCS_CP_REVISION" ] && [ -s "$(cp_dir)/$envfile" ]; } || { echo "  the configuration for $svc is not written (the agent writes it when it applies)" >&2; return 1; }
  cp_rc=0; cp_pkgs_do "$@" || cp_rc=$?; [ "$cp_rc" = 0 ] || return "$cp_rc"
  cp_user_make "$user" || return 1
  was="$(readlink "$(cp_install)/current" 2>/dev/null || true)"
  cp_bundle_switch || return 1
  [ "$was" = "$NCS_CP_BUNDLE_VERSION" ] || changed=1
  if [ "$svc" = scheduler ]; then
    cp_rc=0; cp_ofelia_files_do || cp_rc=$?
    [ "$cp_rc" = 10 ] && changed=1
    [ "$cp_rc" = 0 ] || [ "$cp_rc" = 10 ] || return 1
  fi
  cp_rc=0; "$render" | cp_put "$(cp_unit_file "$svc")" 644 || cp_rc=$?
  if [ "$cp_rc" = 10 ]; then changed=1; unit_changed=1; elif [ "$cp_rc" != 0 ]; then return 1; fi
  if [ "$unit_changed" = 1 ]; then systemctl daemon-reload || return 1; fi
  systemctl enable "$unit" >/dev/null 2>&1 || return 1
  if [ "$changed" = 1 ] || [ "$(cp_applied "$svc")" != "$(cp_marker "$svc")" ] || ! systemctl is-active --quiet "$unit"; then
    systemctl restart "$unit" || return 1
  fi
  n=0
  while [ "$n" -lt "${NCS_CP_START_WAIT_SECONDS:-10}" ]; do systemctl is-active --quiet "$unit" && break; n=$((n + 1)); sleep 1; done
  systemctl is-active --quiet "$unit" || { echo "  $unit did not stay up (journalctl -u $unit says why)" >&2; return 1; }
  cp_marker "$svc" >"$(cp_dir)/applied-$svc.nuit-new" && mv "$(cp_dir)/applied-$svc.nuit-new" "$(cp_dir)/applied-$svc" || return 1
  echo "  $svc runs from bundle $NCS_CP_BUNDLE_VERSION (unit $unit, enabled at boot)"
}

f_ncs_cp_scheduler_ok() { cp_bundle_service_ok scheduler ncs-scheduler cp_render_scheduler scheduler.env curl ca-certificates; }
f_ncs_cp_scheduler_do() { cp_bundle_service_do scheduler ncs-scheduler cp_render_scheduler scheduler.env curl ca-certificates; }
f_ncs_cp_probe_ok() { cp_bundle_service_ok probe ncs-probe cp_render_probe probe.env curl ca-certificates; }
f_ncs_cp_probe_do() { cp_bundle_service_do probe ncs-probe cp_render_probe probe.env curl ca-certificates; }

# ─── The agent's https front: Debian's own nginx ───────────────────────────────────────────────────────────────────────────────────────────
# Listens on 8443 only and forwards ONLY /api/agent/ to the platform (NCS_CP_TLS_UPSTREAM), 404 for everything else; the certificate and key are the
# files the agent wrote (nginx's master reads the key as root). Modest hardening by a drop-in: only what is known safe for Debian's stock unit.
cp_ngx_conf_file() { p /etc/nginx/conf.d/nuit-cloud-agent-front.conf; }
cp_ngx_default_site() { p /etc/nginx/sites-enabled/default; }
cp_ngx_dropin_file() { p /etc/systemd/system/nginx.service.d/10-nuit-cloud.conf; }
cp_nginx_conf() {
  cat <<CONF
# Written by nuit-cloud (the ncs_cp_tls_front feature). Forwards ONLY /api/agent/ to the platform; everything else is 404.
server {
  listen 8443 ssl;
  server_name agent.ops.nuitcloud.com;
  ssl_certificate     $(cp_dir)/tls/cert.pem;
  ssl_certificate_key $(cp_dir)/tls/key.pem;
  ssl_protocols TLSv1.2 TLSv1.3;
  client_max_body_size 1m;
  location /api/agent/ {
    proxy_pass http://$NCS_CP_TLS_UPSTREAM;
    proxy_set_header Host agent.ops.nuitcloud.com;
    proxy_set_header X-Forwarded-For \$remote_addr;
    proxy_read_timeout 60s;
  }
  location / { return 404; }
}
CONF
}
cp_nginx_dropin() {
  cat <<DROP
# Written by nuit-cloud (the ncs_cp_tls_front feature): modest limits on Debian's nginx, nothing that could stop it starting.
[Service]
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
MemoryMax=64M
TasksMax=64
DROP
}
f_ncs_cp_tls_front_ok() {
  cp_wants tls-front || return 0
  if cp_is_missing tls-front; then return 1; fi
  [ -n "${NCS_CP_REVISION:-}" ] && [ "$(cp_revision_on_disk)" = "$NCS_CP_REVISION" ] || return 1
  [ -s "$(cp_dir)/tls/cert.pem" ] && [ -s "$(cp_dir)/tls/key.pem" ] || return 1
  [ -n "${NCS_CP_TLS_UPSTREAM:-}" ] || return 1
  dpkg -s nginx >/dev/null 2>&1 || return 1
  [ ! -e "$(cp_ngx_default_site)" ] || return 1
  cp_nginx_conf | cmp -s - "$(cp_ngx_conf_file)" || return 1
  cp_nginx_dropin | cmp -s - "$(cp_ngx_dropin_file)" || return 1
  systemctl is-enabled --quiet nginx.service || return 1
  systemctl is-active --quiet nginx.service || return 1
  [ "$(cp_applied tls-front)" = "$(cp_marker tls-front)" ]
}
f_ncs_cp_tls_front_do() {
  cp_wants tls-front || return 0
  if cp_is_missing tls-front; then
    echo "  the platform has no usable secret for tls-front yet ($(cp_secret_name tls-front)): put it there, then apply this again" >&2
    return 3
  fi
  [ -n "${NCS_CP_REVISION:-}" ] || { echo "  the task carries no configuration revision" >&2; return 3; }
  { [ "$(cp_revision_on_disk)" = "$NCS_CP_REVISION" ] && [ -s "$(cp_dir)/tls/cert.pem" ] && [ -s "$(cp_dir)/tls/key.pem" ]; } || { echo "  the certificate and key are not written (the agent writes them when it applies)" >&2; return 1; }
  [ -n "${NCS_CP_TLS_UPSTREAM:-}" ] || { echo "  the platform has not said where the https front forwards /api/agent/ to (tlsFront.upstream)" >&2; return 3; }
  changed=0; unit_changed=0
  if ! dpkg -s nginx >/dev/null 2>&1; then
    net_resolves || return 3
    # the package starts nginx on install, with Debian's default site on port 80: say no to that start, and take the site out before it ever runs
    rcd="$(p /usr/sbin/policy-rc.d)"; made=0
    if [ ! -e "$rcd" ]; then mkdir -p "$(dirname "$rcd")" && printf '#!/bin/sh\nexit 101\n' >"$rcd" && chmod 755 "$rcd" && made=1; fi
    cp_rc=0; { apt-get update -qq && apt-get install -y -qq nginx; } || cp_rc=$?
    if [ "$made" = 1 ]; then rm -f "$rcd"; fi
    [ "$cp_rc" = 0 ] || return "$cp_rc"
    changed=1
  fi
  if [ -e "$(cp_ngx_default_site)" ] || [ -L "$(cp_ngx_default_site)" ]; then rm -f "$(cp_ngx_default_site)" || return 1; changed=1; fi
  cp_rc=0; cp_nginx_dropin | cp_put "$(cp_ngx_dropin_file)" 644 || cp_rc=$?
  if [ "$cp_rc" = 10 ]; then unit_changed=1; elif [ "$cp_rc" != 0 ]; then return 1; fi
  if [ "$unit_changed" = 1 ]; then systemctl daemon-reload || return 1; fi
  conf="$(cp_ngx_conf_file)"; backup=""
  if [ -f "$conf" ]; then backup="$conf.nuit-old"; cp "$conf" "$backup" || return 1; fi
  cp_rc=0; cp_nginx_conf | cp_put "$conf" 644 || cp_rc=$?
  if [ "$cp_rc" = 10 ]; then changed=1; elif [ "$cp_rc" != 0 ]; then return 1; fi
  # nothing is reloaded or restarted before nginx itself says the configuration is good; a bad one is taken out again
  if ! out="$(nginx -t 2>&1)"; then
    if [ -n "$backup" ]; then mv "$backup" "$conf"; else rm -f "$conf"; fi
    echo "  nginx -t refused the configuration, so it was not applied (and the old one was put back): $(printf '%s' "$out" | head -n 1 | cut -c1-200)" >&2
    return 1
  fi
  [ -z "$backup" ] || rm -f "$backup"
  systemctl enable nginx.service >/dev/null 2>&1 || return 1
  if [ "$unit_changed" = 1 ] || ! systemctl is-active --quiet nginx.service; then
    systemctl restart nginx.service || return 1
  elif [ "$changed" = 1 ] || [ "$(cp_applied tls-front)" != "$(cp_marker tls-front)" ]; then
    systemctl reload nginx.service || return 1
  fi
  systemctl is-active --quiet nginx.service || { echo "  nginx did not stay up (journalctl -u nginx says why)" >&2; return 1; }
  cp_marker tls-front >"$(cp_dir)/applied-tls-front.nuit-new" && mv "$(cp_dir)/applied-tls-front.nuit-new" "$(cp_dir)/applied-tls-front" || return 1
  echo "  nginx forwards /api/agent/ on port 8443 to $NCS_CP_TLS_UPSTREAM"
}

# ─── The Ingress Service: the platform's routes in nginx (the nuit-cloud-ingress profile; nuit-cloud/nuit-cloud#184) ──────────────────────
# nginx reads its site files from NCS_INGRESS_CONF_DIR and the routes' certificates from directories below NCS_INGRESS_CERT_ROOT. It is either
# a container (NCS_INGRESS_CONTAINER names it, `nuit-nginx` on a Docker host) or, when NCS_INGRESS_CONTAINER is the word `host`, Debian's own
# nginx package on the machine itself (a Nuit Container Service machine has no Docker). Those three and the revision (NCS_INGRESS_REVISION)
# come from the signed task. For an apply the AGENT has already read the content with its own credential, computed its revision again (it
# must be the signed one) and staged it in a root-only directory on tmpfs (NCS_INGRESS_STAGE: sites.list, sites/<file>, certs.list
# "<n> <dir>", certs/<n>/{fullchain,privkey}.pem). ingress-apply.sh writes it: it backs up our files, writes the certificates and site files
# (each via a temporary file and a rename, owned like the directory it is in), removes the platform's site files that are no longer wanted
# (never anything else), runs `nginx -t` (in the container, or on the machine), puts every previous file back if nginx refuses, and otherwise
# reloads nginx and records the revision. Nothing here prints a certificate or a key; they are only ever copied from the stage. A container
# is checked as running, never started or rebuilt. With `host` the feature keeps the package itself: it installs nginx when it is missing
# (without letting the package start it with Debian's default site), makes the two directories, takes out Debian's default site (its
# default_server would make nginx refuse the platform's own), writes one include file in /etc/nginx/conf.d that names the site directory,
# and enables and starts nginx.
# The agent's own service may not write under /home (ProtectHome=read-only), so the writers run as a transient systemd unit (as ncs_login
# does); without systemd, and in tests, they run directly.
ING_STATE="${NCS_INGRESS_STATE_DIR:-/var/lib/nuit-cloud/ingress}"
ING_INCLUDE=/etc/nginx/conf.d/nuit-cloud-ingress.conf
ING_DEFAULT_SITE=/etc/nginx/sites-enabled/default
ing_path_ok() { # an absolute, clean directory of plain names (as the agent checks it)
  printf '%s' "$1" | grep -Eq '^(/[A-Za-z0-9_-][A-Za-z0-9._-]{0,127})+$' && [ "${#1}" -le 255 ]
}
ing_inputs_ok() {
  printf '%s' "${NCS_INGRESS_REVISION:-}" | grep -Eq '^[0-9a-f]{64}$' || return 1
  printf '%s' "${NCS_INGRESS_CONTAINER:-}" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$' || return 1
  ing_path_ok "${NCS_INGRESS_CONF_DIR:-}" && ing_path_ok "${NCS_INGRESS_CERT_ROOT:-}" || return 1
  case "$NCS_INGRESS_CONF_DIR/" in "$NCS_INGRESS_CERT_ROOT/"*) return 1 ;; esac
  case "$NCS_INGRESS_CERT_ROOT/" in "$NCS_INGRESS_CONF_DIR/"*) return 1 ;; esac
  return 0
}
ing_settings() { printf 'conf-dir=%s\ncert-root=%s\ncontainer=%s\n' "$NCS_INGRESS_CONF_DIR" "$NCS_INGRESS_CERT_ROOT" "$NCS_INGRESS_CONTAINER"; }
ing_host() { [ "$NCS_INGRESS_CONTAINER" = host ]; }
ing_running() {
  if ing_host; then systemctl is-active --quiet nginx; return; fi
  [ "$(docker inspect --format '{{.State.Running}}' "$NCS_INGRESS_CONTAINER" 2>/dev/null)" = true ]
}
# ing_privileged <script> <arg>...: run a writer as a transient systemd unit on a real machine (outside the agent's ProtectHome), else directly
ing_privileged() {
  if [ -z "${ROOT:-}" ] && [ -d /run/systemd/system ] && command -v systemd-run >/dev/null 2>&1; then
    systemd-run --quiet --wait --collect --pipe /bin/sh "$@"
  else
    sh "$@"
  fi
}
ing_include() {
  cat <<INC
# Managed by the nuit-cloud agent (the ncs_ingress feature): the Ingress Service's site files. Changes made here are replaced.
include $NCS_INGRESS_CONF_DIR/*.conf;
INC
}
# Debian's default site is enabled: the package's link to sites-available/default (relative or absolute), and nothing else
ing_default_site_on() {
  l="$(p "$ING_DEFAULT_SITE")"
  [ -L "$l" ] || return 1
  case "$(readlink "$l")" in ../sites-available/default | /etc/nginx/sites-available/default) return 0 ;; esac
  return 1
}
ing_host_base_ok() {
  dpkg -s nginx >/dev/null 2>&1 || return 1
  ing_include | cmp -s - "$(p "$ING_INCLUDE")" || return 1
  ! ing_default_site_on || return 1
  systemctl is-enabled --quiet nginx && systemctl is-active --quiet nginx
}
# the role's base on a machine whose nginx is Debian's: package, directories, no default site, the include file, enabled and started. It
# stays when nginx -t later refuses the site files (only those are put back); ingress-apply.sh reloads (or starts) nginx once nginx -t accepts.
ING_MKDIRS='for d; do
  if [ -L "$d" ] || { [ -e "$d" ] && [ ! -d "$d" ]; }; then echo "  $d is there but is not a directory; the agent writes only into real directories" >&2; exit 1; fi
  [ -d "$d" ] && continue
  mkdir -p "$d" && chmod 755 "$d" || { echo "  could not make $d" >&2; exit 1; }
done'
ing_host_base() {
  if ! dpkg -s nginx >/dev/null 2>&1; then
    net_resolves || return 3
    # the package starts nginx on install, with Debian's default site on port 80: say no to that start (as the control plane's front does)
    rcd="$(p /usr/sbin/policy-rc.d)"; made=0
    if [ ! -e "$rcd" ]; then mkdir -p "$(dirname "$rcd")" && printf '#!/bin/sh\nexit 101\n' >"$rcd" && chmod 755 "$rcd" && made=1; fi
    ib_rc=0; out="$( { apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx; } 2>&1)" || ib_rc=$?
    if [ "$made" = 1 ]; then rm -f "$rcd"; fi
    if [ "$ib_rc" != 0 ]; then
      echo "  could not install nginx: $(printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | tail -n 1 | cut -c1-300)" >&2
      return 1
    fi
    echo "  installed Debian's nginx"
  fi
  ing_privileged -c "$ING_MKDIRS" sh "$(p "$NCS_INGRESS_CONF_DIR")" "$(p "$NCS_INGRESS_CERT_ROOT")" || return 1
  l="$(p "$ING_DEFAULT_SITE")"
  if ing_default_site_on; then
    rm -f "$l" || { echo "  could not take out Debian's default site ($ING_DEFAULT_SITE)" >&2; return 1; }
    echo "  Debian's default site is no longer enabled"
  elif [ -e "$l" ] || [ -L "$l" ]; then
    echo "  $ING_DEFAULT_SITE is not Debian's link to its default site, so it was left alone (a default_server in it makes nginx refuse the platform's)"
  fi
  ib_rc=0; ing_include | cp_put "$(p "$ING_INCLUDE")" 644 || ib_rc=$?
  if [ "$ib_rc" = 10 ]; then echo "  $ING_INCLUDE names $NCS_INGRESS_CONF_DIR"
  elif [ "$ib_rc" != 0 ]; then echo "  could not write $ING_INCLUDE" >&2; return 1; fi
  systemctl enable nginx >/dev/null 2>&1 || { echo "  could not enable nginx" >&2; return 1; }
  # an nginx that will not start with the site files it has now is started again once nginx -t accepts the new ones (ingress-apply.sh)
  if ! systemctl is-active --quiet nginx && ! systemctl start nginx >/dev/null 2>&1; then
    echo "  nginx did not start with the site files it has now; it is started again after nginx -t accepts the new ones"
  fi
  return 0
}
ing_staged() {
  [ -n "$1" ] && [ -d "$1" ] && [ "$(cat "$1/revision" 2>/dev/null || true)" = "$NCS_INGRESS_REVISION" ]
}
f_ncs_ingress_ok() {
  ing_inputs_ok || return 1
  [ "$(cat "$(p "$ING_STATE")/revision" 2>/dev/null || true)" = "$NCS_INGRESS_REVISION" ] || return 1
  [ "$(cat "$(p "$ING_STATE")/settings" 2>/dev/null || true)" = "$(ing_settings)" ] || return 1
  if ing_host; then ing_host_base_ok; else ing_running; fi
}
f_ncs_ingress_do() {
  ing_inputs_ok || { echo "  the task carries no usable ingress settings (revision, confDir, certRoot, container)" >&2; return 1; }
  st="${NCS_INGRESS_STAGE:-}"
  if ing_host; then
    # nothing is installed or changed for a revision the agent has not staged
    ing_staged "$st" || { echo "  the ingress configuration for this revision was not fetched (the agent fetches it when it applies)" >&2; return 1; }
    ing_rc=0; ing_host_base || ing_rc=$?
    [ "$ing_rc" = 0 ] || return "$ing_rc"
  elif ! ing_running; then
    echo "  the nginx container $NCS_INGRESS_CONTAINER is not running: start it (the platform does not start or rebuild it), then apply this again. Nothing was changed." >&2
    return 3
  fi
  ing_staged "$st" || { echo "  the ingress configuration for this revision was not fetched (the agent fetches it when it applies)" >&2; return 1; }
  cp "$here/ingress-apply.sh" "$st/apply.sh" && chmod 700 "$st/apply.sh" || return 1
  ing_rc=0
  ing_privileged "$st/apply.sh" "${ROOT:-}" "$st" "$NCS_INGRESS_CONF_DIR" "$NCS_INGRESS_CERT_ROOT" "$NCS_INGRESS_CONTAINER" "$NCS_INGRESS_REVISION" "$ING_STATE" || ing_rc=$?
  [ "$ing_rc" = 0 ] || return 1
  if ing_host && ! ing_running; then echo "  nginx did not stay up (journalctl -u nginx says why)" >&2; return 1; fi
  case "${NCS_INGRESS_PROBLEMS:-0}" in 0|'') ;; *) echo "  the platform left out ${NCS_INGRESS_PROBLEMS} route(s) it could not render (the portal names them)" ;; esac
  return 0
}
