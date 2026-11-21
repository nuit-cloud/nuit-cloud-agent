# Authorises the platform's SSH public keys for its login account (nuitcloud); never creates the account. With the key registry
# (nuit-cloud/nuit-cloud#92) the lines it marks `nuit-cloud:<id>` are kept equal to the registry's list (added, revoked, adopted) and
# every other line is left alone; an older platform's single key is added and nothing is removed. Applied on purpose from the portal; the
# keys are the platform's, taken from its registry, never a request.
PROFILE_FEATURES="ncs_login"
