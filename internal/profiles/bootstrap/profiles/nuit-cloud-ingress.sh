# The Ingress Service (nuit-cloud/nuit-cloud#184): the platform's routes, as site files and certificates for an nginx container that already
# runs on this machine. The signed task names the revision, the two directories and the container; the agent reads the content itself and
# stages it. See `ncs_ingress` in features.sh and ingress-apply.sh.
PROFILE_FEATURES="ncs_ingress"
