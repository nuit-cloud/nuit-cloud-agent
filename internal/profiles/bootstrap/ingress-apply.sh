#!/bin/sh
# Writes the Ingress Service's staged configuration into place (the ncs_ingress feature in features.sh runs it, as a transient systemd unit
# on a real machine). Arguments, all from the signed task or the agent, never from the fetched content:
#   <root> <stage> <confDir> <certRoot> <container> <revision> <stateDir>
# <root> prefixes every path on the machine (empty on a real machine; a scratch directory in test-bootstrap.sh). The stage is the agent's.
# <container> is the nginx container's name, or the word `host`: nginx is then Debian's package on the machine itself, tested and reloaded
# directly (`nginx -t`, `systemctl reload nginx`, or `systemctl start nginx` when it is not running) and never through docker.
#
# It never prints a certificate or a key, and never puts one on a command line: they are only copied, file to file. Our files are the
# platform's site files in confDir (00-nuit-cloud-default.conf, nuit-cloud-<name>.conf) and fullchain.pem/privkey.pem in the certificate
# directories the stage names; nothing else in those directories is read, changed or removed. Exit 0 applied, 1 not applied (and the
# previous files are back in place), 3 not applied because a person (or another apply) has to do something first; nothing was written.
#
# With `host`, a site file may listen on a given IPv4 address (`listen 10.20.0.34:443 ssl;`, the Ingress Service's tenant-VLAN address,
# nuit-cloud/nuit-cloud#205). Before anything is written each such address must be on the machine (`ip -o -4 addr show`), else exit 3; after
# the reload nginx must listen on each address:port the files name (`ss -Htln`), else exit 1 and the revision is not recorded: `nginx -t`
# does not bind, and `systemctl reload nginx` succeeds even when nginx cannot bind and keeps its old configuration. Nor may nginx then listen
# on anything else on any of those ports, every address (0.0.0.0, *, [::]) or another address the files do not name, else exit 1: the
# `listen` lines that cause it are named from `nginx -T` (file and line, nothing else of the configuration). Those checks are
# ingress-listens.sh, which must be beside this script (the feature's check runs it too). It writes and reloads whenever it runs, the same
# revision too: the feature runs it again when the listen check fails on a machine whose recorded revision is in step. An optional eighth
# argument, the word `addresses`, does only the first check (the feature runs it before it installs or changes anything) and exits 0 or 3.
set -u
R="$1"; S="$2"; CONF="$3"; CROOT="$4"; C="$5"; REV="$6"; SD="$7"; MODE="${8:-}"
P() { printf '%s%s' "$R" "$1"; }
say() { printf '  %s\n' "$*"; }
fail() { say "$*" >&2; exit 1; }

# ─── every input checked again ──────────────────────────────────────────────────────────────────────────────────────────────────────────
path_ok() { printf '%s' "$1" | grep -Eq '^(/[A-Za-z0-9_-][A-Za-z0-9._-]{0,127})+$' && [ "${#1}" -le 255 ]; }
site_ok() { printf '%s' "$1" | grep -Eq '^(00-nuit-cloud-default|nuit-cloud-[a-z0-9-]{1,120})\.conf$'; }
# a certificate directory: strictly below certRoot, each part a plain name or `*.` and one
certdir_ok() {
  case "$1" in "$CROOT"/*) ;; *) return 1 ;; esac
  [ "${#1}" -le 255 ] || return 1
  printf '%s' "${1#"$CROOT"/}" | grep -Eq '^(\*\.)?[A-Za-z0-9_-][A-Za-z0-9._-]{0,252}(/(\*\.)?[A-Za-z0-9_-][A-Za-z0-9._-]{0,252})*$'
}
printf '%s' "$REV" | grep -Eq '^[0-9a-f]{64}$' || fail "the revision is not usable"
printf '%s' "$C" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$' || fail "the container name is not usable"
path_ok "$CONF" && path_ok "$CROOT" && path_ok "$SD" || fail "a directory the task names is not a plain absolute directory"
[ "$(cat "$S/revision" 2>/dev/null || true)" = "$REV" ] || fail "the staged configuration is not the signed revision"
[ -f "$S/sites.list" ] && [ -f "$S/certs.list" ] || fail "the staged configuration is incomplete"
while IFS= read -r f; do
  site_ok "$f" && [ -f "$S/sites/$f" ] || fail "the staged configuration names a site file the agent does not write"
done <"$S/sites.list"
while IFS=' ' read -r n d; do
  printf '%s' "$n" | grep -Eq '^[1-9][0-9]{0,3}$' && certdir_ok "$d" && [ -f "$S/certs/$n/fullchain.pem" ] && [ -f "$S/certs/$n/privkey.pem" ] \
    || fail "the staged configuration names a certificate directory that is not below $CROOT"
done <"$S/certs.list"

# ─── the addresses the site files listen on (with `host`): each must be on the machine before anything is written ──────────────────────
# the listen checks are ingress-listens.sh, beside this script (the feature copies both into the stage): the same read-only check the feature's
# own check runs on the installed site files. `addrs` lists every "A.B.C.D:port" the staged site files name, once each.
LCHECK="$(dirname "$0")/ingress-listens.sh"
[ -f "$LCHECK" ] || fail "ingress-listens.sh is not beside this script"
LISTENS=""
if [ "$C" = host ]; then
  LISTENS="$(sh "$LCHECK" addrs "$S/sites")" || fail "could not read the addresses the site files listen on"
  if [ -n "$LISTENS" ]; then
    here_addrs="$(ip -o -4 addr show 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "inet") { split($(i + 1), a, "/"); print a[1] } }')"
    for ap in $LISTENS; do
      printf '%s\n' "$here_addrs" | grep -qxF "${ap%:*}" \
        || { say "the Ingress Service address ${ap%:*} is not on this machine yet (the tenant VLAN apply puts it there)" >&2; exit 3; }
    done
  fi
fi
[ "$MODE" = addresses ] && exit 0
[ -z "$MODE" ] || fail "the mode $MODE is not one this script has"

CONFP="$(P "$CONF")"
[ -d "$CONFP" ] && [ ! -L "$CONFP" ] || fail "the site directory $CONF does not exist"
[ -d "$(P "$CROOT")" ] || fail "the certificate directory $CROOT does not exist"

owner_of() { ls -ldn "$1" | awk '{print $3":"$4}'; }
mode_of() { ls -ldn "$1" | cut -c1-10; }
want_mode() { case "$1" in 600) echo -rw-------;; 644) echo -rw-r--r--;; esac; }
ours() { [ -f "$1" ] || [ -L "$1" ]; }
bounded() { if command -v timeout >/dev/null 2>&1; then timeout 60 "$@"; else "$@"; fi; }

# ─── the backup: our files as they are now (in the stage: root-only, tmpfs, removed by the agent afterwards) ───────────────────────────────
B="$S/backup"
rm -rf "$B"; mkdir -p "$B/conf" "$B/certs" && chmod 700 "$B" || fail "could not make the backup"
: >"$B/created"
for f in "$CONFP"/*; do
  n="$(basename "$f")"
  site_ok "$n" && ours "$f" || continue
  cp -pP "$f" "$B/conf/$n" || fail "could not back up $n; nothing was changed"
done
while IFS=' ' read -r n d; do
  mkdir -p "$B/certs/$n" || fail "could not make the backup"
  for nm in fullchain.pem privkey.pem; do
    if ours "$(P "$d")/$nm"; then cp -pP "$(P "$d")/$nm" "$B/certs/$n/$nm" || fail "could not back up a certificate; nothing was changed"; fi
  done
done <"$S/certs.list"

# restore: every file of ours goes back to what the backup holds (a file that was not there is removed), and the directories made go
restore() {
  for f in "$CONFP"/*; do
    n="$(basename "$f")"
    site_ok "$n" && ours "$f" && rm -f "$f"
  done
  for f in "$B/conf"/*; do
    ours "$f" || continue
    n="$(basename "$f")"
    cp -pP "$f" "$CONFP/.$n.nuit-old" && mv -f "$CONFP/.$n.nuit-old" "$CONFP/$n"
  done
  while IFS=' ' read -r n d; do
    for nm in fullchain.pem privkey.pem; do
      t="$(P "$d")/$nm"
      if ours "$B/certs/$n/$nm"; then cp -pP "$B/certs/$n/$nm" "$t.nuit-old" && mv -f "$t.nuit-old" "$t"; else rm -f "$t"; fi
    done
  done <"$S/certs.list"
  sed -n '1!G;h;$p' "$B/created" | while IFS= read -r dd; do [ -n "$dd" ] && rmdir "$dd" 2>/dev/null; done
  return 0
}

# ─── write ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
changed=0; written=0; removed=0
# put <staged file> <target> <mode>: replace the target via a temporary file beside it and a rename, owned like its directory; a target
# that already has this content, mode and owner is left alone
put() {
  dir="$(dirname "$2")"; own="$(owner_of "$dir")"
  if [ -f "$2" ] && [ ! -L "$2" ] && cmp -s "$1" "$2" && [ "$(mode_of "$2")" = "$(want_mode "$3")" ] && [ "$(owner_of "$2")" = "$own" ]; then return 0; fi
  t="$dir/.$(basename "$2").nuit-new"
  rm -f "$t"
  cp "$1" "$t" && chmod "$3" "$t" && chown "$own" "$t" && mv -f "$t" "$2" || { rm -f "$t"; return 1; }
  changed=1; written=$((written + 1))
}
# mkdirs <dir>: make each missing part below certRoot, owned like its parent; refuse a part that is a symlink or not a directory
mkdirs() {
  cur="$CROOT"; rest="${1#"$CROOT"/}"
  set -f; old_ifs="$IFS"; IFS=/
  for part in $rest; do
    IFS="$old_ifs"
    cur="$cur/$part"; pc="$(P "$cur")"
    if [ -L "$pc" ]; then set +f; say "$cur is a symlink; the agent writes only into real directories" >&2; return 1; fi
    if [ ! -e "$pc" ]; then
      mkdir "$pc" && chmod 755 "$pc" && chown "$(owner_of "$(dirname "$pc")")" "$pc" || { set +f; return 1; }
      printf '%s\n' "$pc" >>"$B/created"
    elif [ ! -d "$pc" ]; then set +f; say "$cur is not a directory" >&2; return 1; fi
  done
  IFS="$old_ifs"; set +f
}
undo() { say "$1; the previous files were put back" >&2; restore; exit 1; }

while IFS=' ' read -r n d; do
  mkdirs "$d" || undo "could not make a certificate directory below $CROOT"
  put "$S/certs/$n/fullchain.pem" "$(P "$d")/fullchain.pem" 644 || undo "could not write a certificate"
  put "$S/certs/$n/privkey.pem" "$(P "$d")/privkey.pem" 600 || undo "could not write a certificate's key"
done <"$S/certs.list"
while IFS= read -r f; do
  put "$S/sites/$f" "$CONFP/$f" 644 || undo "could not write $f"
done <"$S/sites.list"
for f in "$CONFP"/nuit-cloud-*.conf; do
  n="$(basename "$f")"
  site_ok "$n" && ours "$f" || continue
  grep -qxF "$n" "$S/sites.list" && continue
  rm -f "$f" || undo "could not remove $n"
  changed=1; removed=$((removed + 1))
done

# ─── nginx decides ─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
# in the container, or (C=host) Debian's nginx on the machine
ngx() { if [ "$C" = host ]; then bounded nginx "$@"; else bounded docker exec "$C" nginx "$@"; fi; }
if ! out="$(ngx -t 2>&1)"; then
  restore
  say "nginx -t refused the configuration, so the previous files were put back and nginx was not reloaded:" >&2
  printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | head -n 5 | cut -c1-300 | sed 's/^/    /' >&2
  exit 1
fi
reloaded=""
if [ "$C" = host ]; then
  # the feature only runs this when something drifted (the role's base too: the include file, Debian's default site), so a host nginx is
  # always reloaded once nginx -t accepts; one that is not running (it would not start with the files it had) is started instead
  if ! systemctl is-active --quiet nginx; then
    rout="$(bounded systemctl start nginx 2>&1)" || { say "nginx accepted the configuration but did not start: $(printf '%s' "$rout" | head -n 1 | cut -c1-300) (journalctl -u nginx says why)" >&2; exit 1; }
    reloaded="; nginx started"
  else
    rout="$(bounded systemctl reload nginx 2>&1)" || { say "nginx accepted the configuration but did not reload: $(printf '%s' "$rout" | head -n 1 | cut -c1-300)" >&2; exit 1; }
    reloaded="; nginx reloaded"
  fi
elif [ "$changed" = 1 ]; then
  rout="$(ngx -s reload 2>&1)" || { say "nginx accepted the configuration but did not reload: $(printf '%s' "$rout" | head -n 1 | cut -c1-300)" >&2; exit 1; }
  reloaded="; nginx reloaded"
fi
# a reload only signals nginx, and nginx that cannot bind an address logs it and keeps its old configuration: so (with `host`) it must now
# listen on every address:port the files name, within ten seconds, or the revision is not recorded. On those ports it must also listen on
# nothing else: not on every address (0.0.0.0, *, [::]: another configuration on the machine, in sites-enabled, conf.d, nginx.conf or an old
# file, has a listen without an address) and not on another address the files do not name; the Ingress Service would answer there too
# (the management address). ingress-listens.sh says which, with the sockets and the listen lines from nginx -T that cause it.
if [ -n "$LISTENS" ]; then
  sh "$LCHECK" check "$S/sites" 10 full || exit 1
fi

# ─── record (the revision last: it says the files above are this revision) ────────────────────────────────────────────────────────────
SDP="$(P "$SD")"
mkdir -p "$SDP" && chmod 755 "$SDP" || fail "could not record the revision under $SD"
printf 'conf-dir=%s\ncert-root=%s\ncontainer=%s\n' "$CONF" "$CROOT" "$C" >"$SDP/.settings.nuit-new" && mv -f "$SDP/.settings.nuit-new" "$SDP/settings" || fail "could not record the settings under $SD"
printf '%s\n' "$REV" >"$SDP/.revision.nuit-new" && mv -f "$SDP/.revision.nuit-new" "$SDP/revision" || fail "could not record the revision under $SD"
say "revision $(printf '%s' "$REV" | cut -c1-12): $(wc -l <"$S/sites.list" | tr -d ' ') site files, $(wc -l <"$S/certs.list" | tr -d ' ') certificates; $written written, $removed removed$reloaded"
exit 0
