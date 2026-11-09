# Raspberry Pi 3 as a nuit-cloud server

**Status: not a platform target.** The Raspberry Pi 3 is **not planned for any platform role**: workloads belong on Nuit Container Service (`oci-saas`) wherever it fits, and a Pi 3 cannot be an `oci-saas` node. Do not propose it for new work. The profile below passes its stub-based test but has **never been run on a real Pi 3**; it is kept only as a record of what a plain Docker host on a Pi would need, should a Pi ever be used outside the platform. Treat everything below as untested on hardware.

## Why it is not a target

- **Not an `oci-saas` ("Nuit Container Service") node.** That runtime runs each container as a KVM microVM (nuit-cloud/container-management's `lib/docs/oci-saas-connector.md`). The Pi 3 is not expected to provide usable KVM (its interrupt controller lacks virtualization support; a Pi 4/5 does) and 1 GB of RAM leaves nothing for guests. The exact hardware requirements are owned by the `oci-saas` work and were not confirmed.
- **Not a busy ingress.** `--with nginx` works, but the NIC is 100 Mbit on the shared USB 2 bus and there is 1 GB of RAM.

## Hardware and OS

- Pi 3 or 3B+, **a good power supply** (an under-voltage Pi corrupts SD cards and throttles), a decent SD card (A1/A2) or, better, USB boot from an SSD.
- **Raspberry Pi OS Lite, 64-bit** (Bookworm or later). A 32-bit OS is reported as needing a reinstall (`pi_arch`); many images in use here are arm64-first.
- Set a hostname, enable SSH, and create the `nuitcloud` user with the platform's public key installed (`ssh-copy-id`). The bootstrap deliberately does not create this user.

## Run it

Copy this repository's `internal/profiles/bootstrap/` to the Pi as `bootstrap/`, put `by-hand/profiles/raspberry-pi-3.sh` in its
`profiles/`, then as root:

```sh
su - -c "sh /path/to/bootstrap/bootstrap.sh raspberry-pi-3 --check"   # what would change; changes nothing
su - -c "sh /path/to/bootstrap/bootstrap.sh raspberry-pi-3"           # make it so
sudo reboot                                                            # the first run edits boot files
su - -c "sh /path/to/bootstrap/bootstrap.sh raspberry-pi-3 --check"   # should now report everything ok
```

Add `--with nginx` for a (light) nginx-in-Docker server.

## What the profile does

| Feature | Why |
|---|---|
| `pi_arch` | refuses to pretend a 32-bit OS is fine (manual: reinstall) |
| `user` | requires `nuitcloud` to exist (manual) |
| `docker`, `docker_group` | Docker; `nuitcloud` joins the `docker` group (no sudo) |
| `docker_logs` | container logs capped at 5 MB x 2 (an existing `daemon.json` is left alone, reported `MANUAL`) |
| `updates`, `timesync`, `prune` | unattended security updates, a correct clock, weekly image prune |
| `pi_cgroup` | adds `cgroup_enable=memory cgroup_memory=1` to `cmdline.txt` so a service's memory limit works (reboot) |
| `pi_gpumem` | `gpu_mem=16` in `config.txt`, headless (reboot) |
| `pi_zram` | compressed-RAM swap instead of the SD-card swap file |
| `pi_journal` | journal capped at 50 MB, written every 5 minutes (fewer SD writes) |
| `pi_watchdog` | the hardware watchdog reboots a hung Pi |

Output per feature is `ok`, `done`, `DRIFT` (check mode), `MANUAL` (a person must act) or `FAILED`. Exit status is 0 only when everything is `ok` or `done`.

## Then register it

In the portal, register the Pi as a container host (SSH as `nuitcloud`). Flag it `pool` only from the platform tenant, and only if you want tenants' services placed on it; with 1 GB, set per-tenant pool quotas first.

## Known unknowns (why this is deferred)

- Not run on a real Pi 3: package names (`zram-tools`), the `/boot/firmware` vs `/boot` paths and the `zramswap` unit name are unverified.
- Whether the platform's Docker commands, the pool and monitoring behave well on armv8 with 1 GB is unknown.
- `oci-saas` requirements for any future Pi 4/5 profile are unconfirmed.
