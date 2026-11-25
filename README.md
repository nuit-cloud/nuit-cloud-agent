# nuit-cloud-agent

The program a **Nuit Cloud Server** runs so the nuit·cloud platform can commission and look after it.

- It **only calls out** to the platform over HTTPS; it listens on nothing. No SSH, no `sudo` list, no root password held by the platform.
- It does **only what the platform signs**, and only tasks in its own closed catalog: `ping`, `survey`, `profile-check`, `profile-apply`, `update-agent`, `runtime-smoke`, `address-scan`, `ssh-confirm`, `firewall-confirm`, `runtime-repair`, `set-hostname`, `connectivity-check`, `service-log`, `trust-key`, `retire-key`, `volume-export`, `volume-import`, `volume-remove`, `tenant-address`, `root-console`, `ssh-key-remove`. A task is run only if its Ed25519 signature verifies against the key it names, one the agent trusts (given at enrollment, or added by a `trust-key` task), it is for this machine, it has not expired, and it has not been run before.
- One static binary (no runtime on the machine), for linux/amd64 and linux/arm64.

The design and the reasons are in the platform repository: `docs/adr/0001-machines-are-commissioned-by-a-pull-agent.md`.

## Using it

```
# once, as root, with an enrollment code made in the admin portal (Platform tenant):
nuit-cloud-agent enroll --url https://platform.example --code nce_... 

# what the service runs:
nuit-cloud-agent run

nuit-cloud-agent status     # who this machine is on the platform (never the credential)
nuit-cloud-agent survey     # read this machine and print what the platform would be told; changes nothing
```

`--url` must be `https://`. `--insecure-http` allows plain `http://` for a trusted test network only, because the enrollment code and the agent's credential must not cross a network in the clear.

The agent keeps its identity in `/var/lib/nuit-cloud-agent/state.json` (mode 0600, root only), written atomically.

## Installing on a machine

The platform serves an installer. On the machine, as root (`su -`), with an enrollment code from the admin portal (Platform tenant > Nuit Cloud Servers > Enrollment codes, which shows these exact commands):

```
apt-get install -y ca-certificates curl        # a bare Debian cannot fetch anything securely without these
curl -fsSL https://<platform agent address>/api/agent/install.sh | NUIT_CLOUD_ENROLLMENT_CODE=nce_... sh
```

It downloads the build for the machine's CPU from the platform, verifies its SHA-256, installs it with a systemd unit, enrols, and starts it. Re-running it is safe.

### By hand (a trusted test network without https)

```
install -m 0755 nuit-cloud-agent-linux-amd64 /usr/local/bin/nuit-cloud-agent          # uname -m: x86_64 = amd64, aarch64 = arm64
install -m 0644 deploy/nuit-cloud-agent.service /etc/systemd/system/
nuit-cloud-agent enroll --url http://<platform address>:4173 --code nce_... --insecure-http
mkdir -p /etc/systemd/system/nuit-cloud-agent.service.d
printf '[Service]\nEnvironment=NUIT_CLOUD_AGENT_INSECURE_HTTP=1\n' > /etc/systemd/system/nuit-cloud-agent.service.d/insecure-http.conf
systemctl daemon-reload && systemctl enable --now nuit-cloud-agent
```

## Development

```
make test      # go vet and go test
make build     # static binaries in dist/ (VERSION=1.2.3 to set the version)
```

`internal/taskverify/testdata/vectors.json` holds tasks signed by the platform's own TypeScript signing code with a public test key, so the agent is tested against exactly what the platform produces. Regenerate it from the platform repository if the signing format ever changes.

## connectivity-check (read-only)

Params `{targets: [{id, kind: tcp|https|dns, host, port?, path?, sni?}]}` (1 to 32; `path` and `sni` for https only). Result `{results: [{id, ok, ms, detail}]}` in target order. `detail` is `connected`, `HTTP <status>` or `resolved <ipv4>` on success, otherwise one class word: `refused`, `timeout`, `unreachable`, `dns-failed`, `tls-verify-failed`, `tls-handshake-failed`, `reset`, `other`. Hosts are re-validated by the agent (DNS name or non-loopback, non-link-local, non-multicast IPv4); no redirects are followed, no credentials sent; 8 in parallel, 5 s each, 60 s overall.

## service-log (read-only)

Params `{unit: scheduler|probe|tls-front, since, until?, lines?, contains?}`; no other keys. `since`/`until` are RFC3339 with a zone (`until` defaults to now, `since` < `until`, window at most 7 days); `lines` 1 to 5000 (default 1000); `contains` 1 to 64 characters of `[A-Za-z0-9 _.:=/@-]`, a literal substring filter applied by the agent. `unit` maps through a fixed table (`nuit-cloud-cp-scheduler.service`, `nuit-cloud-cp-probe.service`, `nginx.service`). The agent runs `journalctl --no-pager --utc -o short-iso -u <unit> --since 'YYYY-MM-DD HH:MM:SS' --until 'YYYY-MM-DD HH:MM:SS'` (exec, no shell, 20 s). Result `{unit, lines, truncated, earliest, latest, note?}`; `note` is `no-journal`, `unit-not-found` or `unreadable` with no lines. Lines are scrubbed of bearer tokens, `nce_`/`nca_`/`smt_`/`svc_` codes, SSH keys and PEM blocks (lines with `PRIVATE KEY` are dropped); the newest `lines` are kept, at most 200 KB.

## trust-key and retire-key (the task-signing key rollover)

The agent trusts a list of task-signing keys (`trustedKeys` in its state file, at most four) and checks a task only against the key its envelope's `keyId` names. `trust-key` params `{keyId, publicKey}` and nothing else: `publicKey` is the raw Ed25519 key, base64url without padding (43 characters, 32 bytes), and `keyId` must be the platform's id for it (the first 16 hex characters of the SHA-256 of the raw key). It is accepted only as an ordinary signed task, so only a key the agent already trusts can add another; trusting a key already trusted changes nothing. `retire-key` params `{keyId}` and nothing else; it never removes the key that signed the task itself nor the last key, and says why; retiring a key that is not trusted changes nothing. Both report `{keyId, changed, trustedKeyIds}`. `ping` reports `trustedKeyIds`, and the `task-keys` observation is `{keyIds}` (sorted).

## File-mounted secrets on the Nuit Container Service (nuit-cloud/nuit-cloud#222)

The `nuit-container-service` profile's `ncs_file_secrets` feature (before `oci_saas_runtime`) makes `/run/nuit-cloud/kubelet` a directory of the platform user's with mode 0700 (made again at boot by `/etc/tmpfiles.d/nuit-cloud-kubelet.conf`) and tells the runtime's containerd it is the kubelet dir: `Environment=OCI_SAAS_KUBELET_DIR=/run/nuit-cloud/kubelet` in `/etc/systemd/system/containerd-ocisaas.service.d/20-nuit-kubelet-dir.conf`. container-management writes a secret's files to `/run/nuit-cloud/kubelet/pods/<uid>/volumes/kubernetes.io~secret/<name>/` just before `crictl create` and removes them after; the runtime copies them into a tmpfs in the microVM. The check is drift when the directory's owner or mode, the tmpfiles entry or the drop-in differ, or when the running containerd's environment (`/proc/<MainPID>/environ`) lacks the value. The apply restarts `containerd-ocisaas` only when the running containerd lacks it (or the drop-in changed and its environment cannot be read), and says so: only containerd restarts (`KillMode=process`), running pods keep running, and their shims keep the environment they started with, so file secrets work for pods started afterwards. The `runtime` observation adds `fileSecrets: true, fileSecretsDir: "/run/nuit-cloud/kubelet"` when the running containerd has exactly that value and the directory exists; otherwise both are left out.

## Copying a persistent volume between machines (nuit-cloud/nuit-cloud#186)

The Nuit Container Service keeps a persistent volume as a sparse ext4 image, `/var/lib/oci-saas/volumes/t-<tenant uuid>/<name>.img`, and a running pod's VMM holds an exclusive `flock` on it. Three tasks copy one through the platform; each names the image as `volume`, `t-<lower-case tenant uuid>/<name>.img` with `<name>` matching `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$` and no `..`, always below the volumes root (nothing through a symbolic link). `transferId` is a lower-case UUID, `sha256` 64 lower-case hex. No other keys are accepted.

- `volume-export` `{transferId, volume}`: refuses a missing image (`no volume <volume> on this machine`) or one a pod holds (`<volume> is in use by a running pod`); otherwise holds the lock (`LOCK_EX|LOCK_NB`) for the whole read, gzips the image and sends the gzip stream in chunks of at most 262144 bytes, in order, each as `POST /api/agent/volume-transfer/put` `{transferId, index, data (standard base64), sha256 (of the chunk's bytes)}` with the agent's credential. Result `{size, sha256 (of the raw image), chunks, compressedBytes}`.
- `volume-import` `{transferId, volume, size (1 byte to 1 TiB), sha256, chunks (1 to 2^23)}`: an image already there is never overwritten: the same size and SHA-256 is `already: true` (an import tried again), anything else fails and changes nothing. Otherwise it fetches each chunk (`POST /api/agent/volume-transfer/get` `{transferId, index}` answers `{data, sha256}`), checks its hash, gunzips and writes the image sparsely (all-zero 4 KiB blocks left as holes) to `.<name>.img.incoming-<transferId>` (0600; the tenant directory is made 0700 if missing), stops as soon as the stream is longer than `size`, truncates to `size`, fsyncs, checks the size and SHA-256, and puts it in place with a hard link (which, like the runtime's own create, never replaces an image that appeared meanwhile), then fsyncs the directory. Any failure removes the temporary file. Result `{size, sha256, already}`.
- `volume-remove` `{volume, sha256?}`: a missing image is `{removed: false}`; one a pod holds is refused; with `sha256`, the image is hashed under the lock and kept if it differs; otherwise it is deleted under the lock and the directory fsynced, `{removed: true}`.

A chunk is tried again up to 3 times (1, 2 and 4 seconds apart) after a network error or a 5xx; a 4xx ends the task with the platform's `error` text. A chunk whose connection failed may reach the platform twice, so the platform must accept a repeated put of the same index.

## Moving a tenant address between machines (nuit-cloud/nuit-cloud#187)

`tenant-address` moves a tenant address on the tenant VLAN onto this machine (`take`) or off it (`release`), live and in the VLAN configuration, so an address can follow its service to another machine. Params `{action: take|release, address, prefix (16 to 30), vlanId (1 to 4094)}`, no other keys: `address` a dotted IPv4 address in 10/8, 172.16/12 or 192.168/16, not the network or broadcast address of its prefix. The VLAN interface is the one interface the kernel reports as 802.1Q VLAN `<vlanId>` (`ip -d -j addr show`, its `linkinfo`; none or several fail), whatever its name: `<parent>.<vlanId>`, or `vlan<vlanId>` where the dotted name would pass the kernel's 15 characters (nuit-cloud/nuit-cloud#197). Its name must be plain (`^[A-Za-z0-9][A-Za-z0-9_-]*(\.[0-9]{1,4})?$`, at most 15 characters); `/etc/network/interfaces.d/nuit-cloud-vlan<vlanId>` must be a regular file with `iface <vif> inet static` and an `address` line in that stanza (the node's own address), or the task fails with nothing done. A tenant address is one line, `    post-up ip addr add <a>/<prefix> dev <vif> 2>/dev/null || true`, as `ncs_vlan` writes it; the file is rewritten through a temporary file (a dot name, which ifupdown never sources) with the same mode and owner and a rename, and every other line is kept byte for byte.

- `release`: refuses the node's own address, and a primary address with secondaries in its network (Linux would remove them with it). Removes the address from the VLAN interface (`ip addr del`, with the prefix it has; skipped when it is not there) and every line `ip addr add <a>/` from the file. Result `{action: "release", address, wasPresent, fileChanged}`.
- `take`: refuses the node's own address, a prefix other than the node's, and an address on any other interface of this machine. An address not yet here is probed first: three RFC 5227 ARP probes (sender IP 0.0.0.0) on the VLAN interface, listening a second after each; an ARP request or reply from another hardware address with the address as its sender (`<a> is still answered on the VLAN by <mac>: another machine holds it`), or another machine's probe for it, fails the task with nothing changed. Then `ip addr add <a>/<prefix> dev <vif>` and the file line (after the last `ip addr add` line, or after the `address` line); if the file cannot be written the live address is removed again. Last, three gratuitous ARPs (broadcast requests with sender and target IP the address, from the interface's MAC) a second apart. An address already on the interface is not probed or added again: its file line is made sure of and it is announced again. Result `{action: "take", address, alreadyHere, fileChanged, announcements: 3}`.

The `ncs_vlan` feature (profile `nuit-cloud-vlan`, `profile-apply`/`profile-check` with `vlan: {id (1 to 4094), address, prefix (16 to 30), gateway, tenantAddresses?}`) owns the interface and the node's own address; the tenant addresses are this task's (nuit-cloud/nuit-cloud#189). `tenantAddresses` is optional: absent or a list of 0 to 32 IPv4 addresses (anything else, `null` included, is refused). Its check is satisfied when the file has `iface <vif> inet static` with `address <address>/<prefix>`, every given tenant address has an `ip addr add <a>/` line (comments do not count), and the node's address and every given one are live on the interface; tenant lines it was not given are allowed. Its apply renders the whole file but keeps every tenant `ip addr add` line already there, as it is and in its order (one per address), then adds a line for each given address the file lacks; it never drops one. A file written by an older agent (exactly the given set), by this one, or by hand (`scripts/server-vlan5.sh`) keeps working with both.

The ARP packets go through a packet socket (`AF_PACKET`, `ETH_P_ARP`) bound to the VLAN interface; nothing is installed for it.

## The firewall's pod rules (nuit-cloud/nuit-cloud#91)

`profile-apply`/`profile-check` of `nuit-cloud-firewall` take an optional `firewall.pods`: `{mode: observe|enforce, allow: [...], admin?: {targets, ports, from}}`, no other keys. `allow` is 0 to 32 of `a.b.c.d`, `a.b.c.d/n`, `a.b.c.d:port` or `a.b.c.d/n:port` (a port means TCP and UDP to it); `admin.targets` 1 to 16 IPv4 addresses, `admin.ports` 1 to 8 ports, `admin.from` 1 to 16 IPv4 addresses or ranges. With or without `pods`, the forward chain passes only container traffic (nuit-cloud/nuit-cloud#202): replies, and anything in from or out to a container bridge (`ocisaas0`, `cni0`, `docker0`, `br-*`, `pterodactyl0`); forwarding between the machine's own interfaces (the tenant VLAN to the management VLAN, say) is logged (`nc-host_route `), counted (`host_route`) and dropped in every mode. With `pods`, INPUT from the container bridges takes DNS, DHCP and ICMP and counts the rest (`pod_input`); the forward chain, before its bridge accepts, passes the pods' own range 10.89.0.0/16 and what `allow` names, counts the rest of the private ranges (`pod_private`), and lets only `admin.from` reach `admin.targets:ports` through DNAT (the rest is `admin_other`). Observe mode logs (`nc-<counter> `, 10 a minute) and counts but accepts; enforce mode drops. The `firewall` observation is `{loaded, mode: off|observe|enforce, counters: {pod_input, pod_private, admin_other, host_route: {packets, bytes}}, recent: [{rule, dst, port, proto: tcp|udp|other, count}]}`: the counters from `nft -j list counters table inet nuit_cloud`, `recent` the 20 most frequent destinations in the last 24 hours of the kernel log (no source addresses).

## The SSH key registry (nuit-cloud/nuit-cloud#92)

`profile-apply`/`profile-check` of `nuit-cloud-login` take `login: {format: 2, keys: [{id, publicKey}]}`, no other keys (on the list or on a key): `format` the number 2; 1 to 64 keys (an empty list is refused: it would take every platform key away); `id` matches `^[a-z0-9][a-z0-9-]{0,39}$`; `publicKey` is `ssh-ed25519 <68 base64 characters>` whose blob is an ssh-ed25519 key, optionally followed by one space and a comment of 1 to 200 printable ASCII characters (ignored, never passed on); ids unique and key bodies unique. Anything else fails the task before the scripts run. The older `login: {publicKey}` still works as before (one key added if missing, nothing removed). The script gets `NCS_LOGIN_KEYS` (`<id>=<base64>` words) and keeps `nuitcloud`'s `~/.ssh/authorized_keys` in step: each key is the line `ssh-ed25519 <base64> nuit-cloud:<id>`, and a line is the platform's when it is exactly three fields ending `nuit-cloud:<id>`. A missing key is added, a marked line whose id or key is no longer listed is removed, an unmarked `ssh-ed25519 <base64> …` line (no options) holding a listed key is adopted in place, and every other line is kept as it was. The new file is written beside the old one, given 0600 and the account as owner, then renamed over it (directory 0700), in a transient systemd unit because the agent's service cannot write under `/home`. A check is clean when nothing would be added, removed or adopted; otherwise it says how many of each and how many unmanaged lines are left alone; an apply reports the same counts. No key body is ever printed. The survey's `authorizedKeys` is `[{fingerprint: "SHA256:…", type, managedId: string|null}]` for that file (`[]` when there is none, `null` with a `collectionProblems` entry when it cannot be read or there is no `nuitcloud` account).

## Root's console password (nuit-cloud/nuit-cloud#194)

`root-console` sets or locks root's password for the machine's keyboard, so a person can log in at the console when the machine cannot reach the platform. It changes nothing about SSH: `PermitRootLogin no` stays. The platform generates the password, keeps the plaintext only in OpenBao and signs only its crypt hash. Params, no other keys (anything else fails the task before the machine is touched):

- `{action: "set", hash}`: `hash` at most 512 characters and exactly one of SHA-512 crypt `^\$6\$(rounds=[0-9]{4,9}\$)?[./A-Za-z0-9]{1,16}\$[./A-Za-z0-9]{86}$` or yescrypt `^\$y\$[./A-Za-z0-9]+\$[./A-Za-z0-9]{1,86}\$[./A-Za-z0-9]{43}$` (so no newline, no `$1$`/`$5$`, no plaintext). The agent runs `chpasswd -e` with `root:<hash>\n` on its standard input (never in its arguments, the environment or a file), then reads `/etc/shadow` and confirms root's password field is exactly the hash. Output `root's console password is set (sha512-crypt|yescrypt)`, result `{action: "set", scheme, state: "set"}`.
- `{action: "lock"}`: `usermod -L root`, then confirms root's field starts with `!`. Output `root's console password is locked`, result `{action: "lock", state: "locked"}`.

The hash is never printed, in whole or in part beyond the scheme's name; a chpasswd error is reported with anything shaped like a hash taken out. Both commands run directly as the agent's children: the agent's unit has no `ProtectSystem`, read-only paths or capability limit, so it may write `/etc/shadow` (a test holds the unit to that; if it ever changes, the write must move to a transient unit as `ncs_login`'s does). The survey's `rootPassword` is root's state from `/etc/shadow`'s second field: `none` (empty), `locked` (starts with `!` or `*`, which includes a locked hash), `set` (anything else), or `null` with a `collectionProblems` entry (`root-password: ...`) when the file cannot be read or has no line for root. The field itself is never reported.

## Removing an unknown SSH key (nuit-cloud/nuit-cloud#200)

`ssh-key-remove` takes keys the platform's registry does not know out of `nuitcloud`'s `~/.ssh/authorized_keys`, by fingerprint, so nobody has to log in over SSH to do it. Params, exactly these and no other keys (anything else fails the task before the machine is touched): `{fingerprints: string[], keep: string[], requireOneOf: string[]}`, each fingerprint `^SHA256:[A-Za-z0-9+/]{43}$` (the survey's form: SHA-256 of the key blob, unpadded base64), each list without duplicates; `fingerprints` 1 to 16 (the keys to remove), `keep` 1 to 64 (the registry's active keys for this machine: never removed), `requireOneOf` 1 to 16 (administrator and break-glass keys: at least one must still be in the file afterwards). A fingerprint to remove that is also in `keep` or `requireOneOf` fails the task.

Lines are read with the survey's own rules (options before the key found by its key type; a line is the platform's only when it is exactly `<type> <base64> nuit-cloud:<id>`). Every **unmarked** line holding a listed key is removed, duplicates included; a platform-marked line is never removed by this task (it belongs to the login profile's Sync keys) and its fingerprint is reported as `managed`; a fingerprint on no line is `notFound`. Everything else stays byte for byte: comments, blank lines, lines that hold no key, other keys, CRLF line endings and whether the file ends in a newline. The task fails and writes nothing when the new file would hold none of `requireOneOf` (on any line, marked or not; `removing these keys would leave no administrator or break-glass key in authorized_keys: nothing was changed`), or when a line to remove holds a `keep` key. A missing file or nothing to remove is `done` with no write. A symbolic-link `~/.ssh`, a non-regular `authorized_keys`, or a home or group in `/etc/passwd` the agent will not write under, fails the task.

The new content is worked out in the agent, then written by a small script (in a transient systemd unit, because the agent's service has `ProtectHome=read-only`; directly without systemd): it refuses a symbolic-link `.ssh` or a non-regular file, checks the file's SHA-256 is still the one the agent read (else `authorized_keys changed while the removal was prepared: nothing was changed`), writes the content to a temporary file beside it, gives it mode 600 and `nuitcloud:<its group>`, and renames it over the old file; on any failure the old file stays and the temporary one goes. Output `removed N line(s) holding K key(s)[; M not in the file][; P on the platform's own lines, left to Sync keys]`, result `{removed: [fp], notFound: [fp], managed: [fp], linesRemoved: n}` (each list in the task's order; a fingerprint can be both removed and managed). Only fingerprints are ever reported, never a key body or comment.

## The Ingress Service (nuit-cloud/nuit-cloud#184)

`profile-apply`/`profile-check` of `nuit-cloud-ingress` require `ingress: {revision, confDir, certRoot, container}`, no other keys (`ingress` on any other profile is refused): `revision` 64 lower-case hex; `confDir` and `certRoot` absolute, clean directories of `[A-Za-z0-9._-]` parts not starting with a dot, at most 255 characters, neither inside the other; `container` matches `^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`; the exact word `host` (lower case, nothing else) means nginx is Debian's own package on the machine rather than a container. A check reads nothing from the platform: it is clean when `/var/lib/nuit-cloud/ingress/revision` is the signed revision, `settings` there names the same directories and container, and the container is running (with `host`: the `nginx` package is installed, `/etc/nginx/conf.d/nuit-cloud-ingress.conf` holds exactly a comment saying the agent manages it and `include <confDir>/*.conf;`, Debian's default-site link `/etc/nginx/sites-enabled/default` → `../sites-available/default` or `/etc/nginx/sites-available/default` is absent, and `nginx` is enabled and active). An apply reads `GET /api/agent/ingress-config` with the agent's own credential (`{revision, sites: [{file, content}], certificates: [{dir, fullchainPem, privkeyPem}], problems: [...]}`; at most 8 MiB, 512 sites, 512 certificates, 256 KiB a site, 64 KiB a PEM, no NUL), checks every `file` against `^(00-nuit-cloud-default|nuit-cloud-[a-z0-9-]{1,120})\.conf$` and every `dir` is strictly below `certRoot` (each part a plain name, or `*.` and one for a wildcard), and recomputes the revision: SHA-256 hex over, sites sorted by `file` and certificates by `dir` (byte order), `"site\0" file "\0" content "\0"` for each site, then `"cert\0" dir "\0" fullchainPem "\0" privkeyPem "\0"` for each certificate. It must equal the signed revision (and the answer's own), or nothing is done. The content is staged in a root-only directory under `/run/nuit-cloud-agent` and written by the profile, in a transient systemd unit because the agent's service cannot write under `/home`: certificates (`fullchain.pem` 0644, `privkey.pem` 0600) and site files (0644), each through a temporary file and a rename, owned like their directory; the platform's site files no longer wanted are removed (only `nuit-cloud-*.conf` names; never other files, never certificate directories); then `docker exec <container> nginx -t` (with `host`: `nginx -t` on the machine). If it fails, every previous file is put back and the task fails with nginx's own message; otherwise nginx is reloaded (when something changed) and the revision recorded. A container that is not running is MANUAL, with nothing changed. With `host`, before the content is written the profile keeps the role's base: it installs `nginx` when missing (`apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx`, with a `policy-rc.d` that stops the package starting it on Debian's default site; FAILED with apt's last line, MANUAL when the machine cannot resolve names), makes `confDir` and `certRoot` (root-owned, 0755) when missing, removes Debian's default-site link (a `sites-enabled/default` that is anything else is left alone, with a note), writes the include file, and enables and starts `nginx`; after `nginx -t` accepts, nginx is reloaded with `systemctl reload nginx` (started instead when it is not running). When `nginx -t` refuses, the site files and certificates are put back but the base stays. The `ingress` observation, sent only by a machine where an apply has succeeded, is `{revision: string|null, containerRunning, sites, nginxOk: bool|null}`: `sites` counts the platform's site files in `confDir`, `nginxOk` is `nginx -t` in the container (null when it is not running or does not answer within 15 s). With `host`, `containerRunning` is `systemctl is-active nginx` and `nginxOk` is `nginx -t` on the machine; docker is never asked.

## Status

Version 0.2.x. Built: enrolment, check-in, signed-task verification, `ping`, `survey`, `profile-check`, `profile-apply`. Not yet built: self-update, signed releases, and automatic credential rotation. See the tickets in the platform repository under the epic "Commission a Nuit Cloud Server from the admin portal".

## License

MIT. See LICENSE.

## Releases and how to verify one

A release (a `v*` tag) is built by `.github/workflows/release.yml` on GitHub's runners and carries both Linux binaries,
`SHA256SUMS`, and `SHA256SUMS.sigstore.json`: a keyless [cosign](https://github.com/sigstore/cosign) signature over the
checksums. There is no long-lived key to lose: the signing identity is the workflow itself, recorded in a certificate and in
the public Rekor log. To check a download:

```sh
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity "https://github.com/nuit-cloud/nuit-cloud-agent/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

The platform's `deploy/fetch-agent-release.sh` does exactly this before it will serve a build.
