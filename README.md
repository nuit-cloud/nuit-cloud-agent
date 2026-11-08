# nuit-cloud-agent

The program a **Nuit Cloud Server** runs so the nuit·cloud platform can commission and look after it.

- It **only calls out** to the platform over HTTPS; it listens on nothing. No SSH, no `sudo` list, no root password held by the platform.
- It does **only what the platform signs**, and only tasks in its own closed catalog: `ping`, `survey`, `profile-check`, `profile-apply`, `update-agent`, `runtime-smoke`, `address-scan`, `ssh-confirm`, `firewall-confirm`, `runtime-repair`, `set-hostname`, `connectivity-check`. A task is run only if its Ed25519 signature verifies against a key the agent was given at enrollment, it is for this machine, it has not expired, and it has not been run before.
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
  --certificate-identity "https://github.com/mgjoyce/nuit-cloud-agent/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

The platform's `deploy/fetch-agent-release.sh` does exactly this before it will serve a build.
