#!/bin/sh
# The Ingress Service on host nginx: where its site files listen, and whether nginx listens exactly there (nuit-cloud/nuit-cloud#205).
# Read-only: it reads site files, `ss -Htln` and (only for the full message) `nginx -T`, and changes nothing. ingress-apply.sh runs it after
# the reload, and the ncs_ingress feature's check runs it on the installed site files, so a machine whose recorded revision is in step but
# whose nginx also answers elsewhere is not satisfied, and the apply runs again.
#
#   ingress-listens.sh addrs <dir>
#       every "A.B.C.D:port" a site file of ours in <dir> (00-nuit-cloud-default.conf, nuit-cloud-<name>.conf; regular files only) names
#       in a listen directive, once each. Anything that is not a dotted IPv4 address with a port (`listen 80;`, `listen [::]:443 ssl;`) is
#       not an address here. Nothing printed: the files name no address.
#   ingress-listens.sh check <dir> <tries> <brief|full>
#       exit 0 when the files name no address (nothing else is asked), or when, on every port they name with an address, nginx listens on
#       each address:port they name and on nothing else: no socket on every address (0.0.0.0, *, [::]) and none on another address. Else
#       exit 1: `brief` says the first problem in one line (the feature's check; `ss` is asked once, so give 1 try), `full` says each one,
#       the sockets, and names from `nginx -T` the listen lines on those ports that are not on an address the files name (at most twenty,
#       as "<file>: <line>", nothing else of the configuration). Up to <tries> looks at `ss`, a second apart (a reload only signals nginx).
set -u
say() { printf '  %s\n' "$*" >&2; }
usage() { say "usage: ingress-listens.sh addrs <dir> | check <dir> <tries> <brief|full>"; exit 2; }
[ $# -ge 2 ] || usage
CMD="$1"; DIR="$2"
[ -d "$DIR" ] || { say "the site directory $DIR does not exist"; exit 1; }

site_ok() { printf '%s' "$1" | grep -Eq '^(00-nuit-cloud-default|nuit-cloud-[a-z0-9-]{1,120})\.conf$'; }
OCTET='(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])'
# comments dropped, one statement per line; only a strict dotted IPv4 address with a port in 1..65535
addrs() {
  for f in "$DIR"/*; do
    n="$(basename "$f")"
    site_ok "$n" && [ -f "$f" ] && [ ! -L "$f" ] || continue
    sed 's/#.*//' "$f" | tr '{};' '\n\n\n' | awk '$1 == "listen" { print $2 }'
  done | grep -E "^$OCTET(\.$OCTET){3}:[1-9][0-9]{0,4}\$" | awk -F: '$2 <= 65535' | LC_ALL=C sort -u
}

case "$CMD" in
  addrs) addrs; exit 0 ;;
  check) [ $# = 4 ] || usage ;;
  *) usage ;;
esac
TRIES="$3"; HOW="$4"
printf '%s' "$TRIES" | grep -Eq '^[1-9][0-9]?$' || usage
case "$HOW" in brief|full) ;; *) usage ;; esac

LISTENS="$(addrs)"
[ -n "$LISTENS" ] || exit 0
set -f # an address such as [::1]:80 is a word here, never a pattern
NAMED=" $(printf '%s\n' "$LISTENS" | tr '\n' ' ')"
PORTS=" $(printf '%s\n' "$LISTENS" | awk -F: '{ print $2 }' | LC_ALL=C sort -un | tr '\n' ' ')"

bounded() { if command -v timeout >/dev/null 2>&1; then timeout 60 "$@"; else "$@"; fi; }
# stray: the listening sockets on a named port whose local address is not one the files name, as "wild <port> <line>" for every address
# (0.0.0.0, *, [::]) and "other <addr:port> <line>" for any other address (10.31.0.13:80, 127.0.0.1:443, [::1]:80, ...)
stray() {
  printf '%s\n' "$sockets" | awk -v ports="$PORTS" -v named="$NAMED" '
    NF >= 4 {
      a = $4; i = match(a, /:[0-9]+$/); if (!i) next
      port = substr(a, i + 1); host = substr(a, 1, i - 1)
      if (index(ports, " " port " ") == 0 || index(named, " " a " ") > 0) next
      if (host == "0.0.0.0" || host == "*" || host == "[::]") print "wild " port " " $0
      else print "other " a " " $0
    }' | LC_ALL=C tr -c '[:print:]\n' '?' | cut -c1-300
}

tries=0
while :; do
  sockets="$(ss -Htln 2>/dev/null)"
  held=" $(printf '%s\n' "$sockets" | awk 'NF >= 4 { print $4 }' | tr '\n' ' ')"
  missing=""
  for ap in $LISTENS; do case "$held" in *" $ap "*) ;; *) missing="$ap"; break ;; esac; done
  strays="$(stray)"
  [ -z "$missing" ] && [ -z "$strays" ] && exit 0
  tries=$((tries + 1))
  [ "$tries" -lt "$TRIES" ] || break
  sleep 1
done

wild="$(printf '%s\n' "$strays" | awk '$1 == "wild" { print $2 }' | LC_ALL=C sort -un | tr '\n' ' ')"
other="$(printf '%s\n' "$strays" | awk '$1 == "other" { print $2 }' | LC_ALL=C sort -u | tr '\n' ' ')"

if [ "$HOW" = brief ]; then
  # one line, the first problem: what `profile-check` reports beside the drift
  set -- $wild; if [ $# -gt 0 ]; then say "nginx also listens on every address on port $1"; exit 1; fi
  set -- $other; if [ $# -gt 0 ]; then say "nginx listens on $1, which the site files do not name"; exit 1; fi
  say "nginx does not listen on $missing"
  exit 1
fi

# culprits: from `nginx -T` (a fixed, read-only command), each listen line on one of the ports whose address is not one the files name
# (a port alone or `*:80` is every address), as "<file>: <line>", at most 20 lines; nothing else of the configuration is printed
culprits() {
  bounded nginx -T 2>/dev/null | awk -v ports=" $1 " -v named="$NAMED" '
    function stmt(x,   n, w, t, port, host, i) {
      sub(/^[ \t]+/, "", x); sub(/[ \t]+$/, "", x)
      n = split(x, w, /[ \t]+/)
      if (n < 2 || w[1] != "listen") return
      t = w[2]; gsub(/"/, "", t)
      if (t ~ /^unix:/) return
      if (t ~ /^[0-9]+$/) { host = "*"; port = t }
      else if (match(t, /:[0-9]+$/)) { host = substr(t, 1, RSTART - 1); port = substr(t, RSTART + 1) }
      else { host = t; port = "80" }
      if (index(ports, " " port " ") == 0 || index(named, " " host ":" port " ") > 0) return
      x = w[1]; for (i = 2; i <= n; i++) x = x " " w[i]
      found++; if (found <= 20) printf "%s: %s;\n", file, x
    }
    /^# configuration file .*:$/ { file = substr($0, 22, length($0) - 22); buf = ""; next }
    {
      line = $0; sub(/#.*/, "", line); buf = buf " " line
      while (match(buf, /[;{}]/)) { stmt(substr(buf, 1, RSTART - 1)); buf = substr(buf, RSTART + 1) }
    }
    END {
      if (found > 20) printf "... and %d more\n", found - 20
      if (found == 0) print "nginx -T names no such listen line (a server block with no listen line at all listens on every address on port 80)"
    }' | LC_ALL=C tr -c '[:print:]\n' '?' | cut -c1-300
}
if [ -n "$strays" ]; then
  for p in $wild; do say "nginx also listens on every address on port $p: another configuration has a listen without an address"; done
  for ap in $other; do say "nginx listens on $ap, which the site files do not name: another configuration listens there"; done
  printf '%s\n' "$strays" | cut -d' ' -f3- | head -n 10 | sed 's/^/    /' >&2
  cports="$(printf '%s\n' "$strays" | awk '{ p = $2; sub(/.*:/, "", p); print p }' | LC_ALL=C sort -un | tr '\n' ' ')"
  culprits "${cports% }" | sed 's/^/    /' >&2
  exit 1
fi
say "nginx did not take the new configuration: it does not listen on $missing; journalctl -u nginx says why"
exit 1
