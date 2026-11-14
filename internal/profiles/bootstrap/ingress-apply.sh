#!/bin/sh
# Writes the Ingress Service's staged configuration into place (the ncs_ingress feature in features.sh runs it, as a transient systemd unit
# on a real machine). Arguments, all from the signed task or the agent, never from the fetched content:
#   <root> <stage> <confDir> <certRoot> <container> <revision> <stateDir>
# <root> prefixes every path on the machine (empty on a real machine; a scratch directory in test-bootstrap.sh). The stage is the agent's.
#
# It never prints a certificate or a key, and never puts one on a command line: they are only copied, file to file. Our files are the
# platform's site files in confDir (00-nuit-cloud-default.conf, nuit-cloud-<name>.conf) and fullchain.pem/privkey.pem in the certificate
# directories the stage names; nothing else in those directories is read, changed or removed. Exit 0 applied, 1 not applied (and the
# previous files are back in place).
set -u
R="$1"; S="$2"; CONF="$3"; CROOT="$4"; C="$5"; REV="$6"; SD="$7"
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
if ! out="$(bounded docker exec "$C" nginx -t 2>&1)"; then
  restore
  say "nginx -t refused the configuration, so the previous files were put back and nginx was not reloaded:" >&2
  printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | head -n 5 | cut -c1-300 | sed 's/^/    /' >&2
  exit 1
fi
reloaded=""
if [ "$changed" = 1 ]; then
  rout="$(bounded docker exec "$C" nginx -s reload 2>&1)" || { say "nginx accepted the configuration but did not reload: $(printf '%s' "$rout" | head -n 1 | cut -c1-300)" >&2; exit 1; }
  reloaded="; nginx reloaded"
fi

# ─── record (the revision last: it says the files above are this revision) ────────────────────────────────────────────────────────────
SDP="$(P "$SD")"
mkdir -p "$SDP" && chmod 755 "$SDP" || fail "could not record the revision under $SD"
printf 'conf-dir=%s\ncert-root=%s\ncontainer=%s\n' "$CONF" "$CROOT" "$C" >"$SDP/.settings.nuit-new" && mv -f "$SDP/.settings.nuit-new" "$SDP/settings" || fail "could not record the settings under $SD"
printf '%s\n' "$REV" >"$SDP/.revision.nuit-new" && mv -f "$SDP/.revision.nuit-new" "$SDP/revision" || fail "could not record the revision under $SD"
say "revision $(printf '%s' "$REV" | cut -c1-12): $(wc -l <"$S/sites.list" | tr -d ' ') site files, $(wc -l <"$S/certs.list" | tr -d ' ') certificates; $written written, $removed removed$reloaded"
exit 0
