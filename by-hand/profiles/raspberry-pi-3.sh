# NOT A PLATFORM TARGET: a Pi 3 cannot be an oci-saas node and workloads belong on Nuit Container Service (see raspberry-pi-3.md).
# Kept as a record only. Raspberry Pi 3 / 3B+ (1 GB RAM, SD card, 100 Mbit NIC on USB 2) as a plain docker-host. Tuned to save memory and SD wear, with smaller log caps. Needs a reboot the first time
# (memory cgroup, gpu_mem). Use Raspberry Pi OS Lite 64-bit. Add `--with nginx` only for a very light ingress.
LOG_MAX_SIZE=5m
LOG_MAX_FILE=2
PROFILE_FEATURES="pi_arch user docker docker_group docker_logs updates timesync prune pi_cgroup pi_gpumem pi_zram pi_journal pi_watchdog"
