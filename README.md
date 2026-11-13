# nuit-cloud-agent

The program a **Nuit Cloud Server** runs so the nuit·cloud platform can commission and look after it.

- It **only calls out** to the platform over HTTPS; it listens on nothing. No SSH, no `sudo` list, no root password held by the platform.
- It does **only what the platform signs**, and only tasks in its own closed catalog: `ping`, `survey`, `profile-check`, `profile-apply`, `update-agent`, `runtime-smoke`, `address-scan`, `ssh-confirm`, `firewall-confirm`, `runtime-repair`, `set-hostname`, `connectivity-check`, `service-log`, `trust-key`, `retire-key`. A task is run only if its Ed25519 signature verifies against the key it names, one the agent trusts (given at enrollment, or added by a `trust-key` task), it is for this machine, it has not expired, and it has not been run before.
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

## The firewall's pod rules (nuit-cloud/nuit-cloud#91)

`profile-apply`/`profile-check` of `nuit-cloud-firewall` take an optional `firewall.pods`: `{mode: observe|enforce, allow: [...], admin?: {targets, ports, from}}`, no other keys. `allow` is 0 to 32 of `a.b.c.d`, `a.b.c.d/n`, `a.b.c.d:port` or `a.b.c.d/n:port` (a port means TCP and UDP to it); `admin.targets` 1 to 16 IPv4 addresses, `admin.ports` 1 to 8 ports, `admin.from` 1 to 16 IPv4 addresses or ranges. Without `pods` the ruleset is exactly what it was. With it, INPUT from the container bridges takes DNS, DHCP and ICMP and counts the rest (`pod_input`); a forward chain (policy accept) passes the pods' own range 10.89.0.0/16 and what `allow` names, counts the rest of the private ranges (`pod_private`), and lets only `admin.from` reach `admin.targets:ports` through DNAT (the rest is `admin_other`). Observe mode logs (`nc-<counter> `, 10 a minute) and counts but accepts; enforce mode drops. The `firewall` observation is `{loaded, mode: off|observe|enforce, counters: {pod_input, pod_private, admin_other: {packets, bytes}}, recent: [{rule, dst, port, proto: tcp|udp|other, count}]}`: the counters from `nft -j list counters table inet nuit_cloud`, `recent` the 20 most frequent destinations in the last 24 hours of the kernel log (no source addresses).

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
