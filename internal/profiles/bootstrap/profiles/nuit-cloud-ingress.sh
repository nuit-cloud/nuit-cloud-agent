# The Ingress Service (nuit-cloud/nuit-cloud#184): the platform's routes, as site files and certificates for nginx on this machine: a container
# that already runs here, or, when the container setting is the word `host`, Debian's nginx package, which the feature installs and keeps.
# The signed task names the revision, the two directories and the container; the agent reads the content itself and stages it. See
# `ncs_ingress` in features.sh and ingress-apply.sh.
PROFILE_FEATURES="ncs_ingress"
