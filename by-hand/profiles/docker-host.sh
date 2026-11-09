# A standard Debian/Ubuntu machine the platform deploys containers onto (a pool or bring-your-own host).
# Add `--with nginx` to make it an nginx/ingress server too.
PROFILE_FEATURES="user docker docker_group docker_logs updates timesync prune"
