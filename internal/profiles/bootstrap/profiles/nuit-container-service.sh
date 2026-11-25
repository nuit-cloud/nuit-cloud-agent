# A Nuit Cloud Server that plays the Nuit Container Service role: the base profile plus hardware virtualisation (/dev/kvm),
# cgroup v2, room for tenants' volumes and the runtime itself. Tenants' containers then run on it as microVMs.
# ncs_file_secrets comes before the runtime, so a first install starts the runtime with the setting already in place (no restart).
. "$here/profiles/nuit-cloud-server.sh"
PROFILE_FEATURES="$PROFILE_FEATURES kvm cgroup2 ncs_volumes ncs_file_secrets oci_saas_runtime ncs_log_root"
