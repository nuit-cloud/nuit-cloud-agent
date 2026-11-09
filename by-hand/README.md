# Profiles run by hand

The agent carries exactly the profiles the platform asks for (`internal/profiles/bootstrap/profiles/`, embedded in the binary and
checked by `TestTheRealEmbeddedProfilesAreTheOnesThePlatformAsksFor`). The profiles here use the same `bootstrap.sh` and
`features.sh` but are not embedded: no signed task can name them.

| Profile | What it is |
|---|---|
| `docker-host` | A bring-your-own Docker host (the stated fallback to Nuit Container Service); `--with nginx` adds nginx in Docker |
| `nginx-server` | The original standard server: a `docker-host` that runs nginx in Docker for routes |
| `raspberry-pi-3` | Not a platform target, kept as a record (`raspberry-pi-3.md`) |

To run one: copy `internal/profiles/bootstrap/` to the machine, add the profile to its `profiles/`, and as root
`sh bootstrap/bootstrap.sh <profile> --check`, then without `--check`. `make test-bootstrap` tests them with the agent's own.
