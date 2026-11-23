#!/bin/sh
# The Ingress Service on host nginx: where its site files listen, and whether nginx listens exactly there (nuit-cloud/nuit-cloud#205).
# Read-only: it reads site files, `ss -Htlnp` (or `ss -Htln`) and (only for the full message) `nginx -T`, and changes nothing. ingress-apply.sh runs it after
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
#       Each socket's owning process comes from `ss -Htlnp` (`ss -Htln` when that says nothing): a socket another program holds is named
#       as that program's ("apache2 listens on every address on port 80"), not as nginx's. `full` exits 4 instead of 1 when every stray
#       socket is nginx's and the configuration nginx would load (`nginx -T`) names no listen line for any of them: nginx is running an
#       older configuration than the one on disk (a reload cannot move a port from every address to one address), and only a restart
#       rebinds. ingress-apply.sh restarts nginx once on 4; this script never changes anything.
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
# stray: the listening sockets on a named port whose local address is not one the files name, as "wild <port> <owner> <line>" for every
# address (0.0.0.0, *, [::]) and "other <addr:port> <owner> <line>" for any other address (10.31.0.13:80, 127.0.0.1:443, [::1]:80, ...).
# <owner> is the process names ss gives for the socket (users:(("nginx",pid=..),..)), comma-separated, or - when ss names none
stray() {
  printf '%s\n' "$sockets" | awk -v ports="$PORTS" -v named="$NAMED" '
    NF >= 4 {
      a = $4; i = match(a, /:[0-9]+$/); if (!i) next
      port = substr(a, i + 1); host = substr(a, 1, i - 1)
      if (index(ports, " " port " ") == 0 || index(named, " " a " ") > 0) next
      own = ""; u = index($0, "users:("); s = (u ? substr($0, u) : "")
      while (match(s, /\("[^"]*"/)) {
        n = substr(s, RSTART + 2, RLENGTH - 3); s = substr(s, RSTART + RLENGTH)
        gsub(/[^A-Za-z0-9._-]/, "_", n); n = substr(n, 1, 32); if (n == "") n = "_"
        if (index("," own ",", "," n ",") == 0) own = own (own == "" ? "" : ",") n
      }
      if (own == "") own = "-"
      if (host == "0.0.0.0" || host == "*" || host == "[::]") print "wild " port " " own " " $0
      else print "other " a " " own " " $0
    }' | LC_ALL=C tr -c '[:print:]\n' '?' | cut -c1-300
}
# listen: the sockets, with their owning processes when ss can say (it needs root for other users' sockets)
listening() {
  out="$(ss -Htlnp 2>/dev/null)"
  [ -n "$out" ] || out="$(ss -Htln 2>/dev/null)"
  printf '%s\n' "$out"
}
# nginxish <owner>: the socket is nginx's or ss did not say whose it is (then it is spoken of as nginx's, as before owners were read)
nginxish() { [ "$1" = nginx ] || [ "$1" = - ]; }

tries=0
while :; do
  sockets="$(listening)"
  held=" $(printf '%s\n' "$sockets" | awk 'NF >= 4 { print $4 }' | tr '\n' ' ')"
  missing=""
  for ap in $LISTENS; do case "$held" in *" $ap "*) ;; *) missing="$ap"; break ;; esac; done
  strays="$(stray)"
  [ -z "$missing" ] && [ -z "$strays" ] && exit 0
  tries=$((tries + 1))
  [ "$tries" -lt "$TRIES" ] || break
  sleep 1
done

if [ "$HOW" = brief ]; then
  # one line, the first problem: what `profile-check` reports beside the drift
  set -- $(printf '%s\n' "$strays" | awk '$1 == "wild"' | head -n 1)
  if [ $# -gt 0 ]; then
    if nginxish "$3"; then say "nginx also listens on every address on port $2"; else say "$3 listens on every address on port $2"; fi
    exit 1
  fi
  set -- $(printf '%s\n' "$strays" | awk '$1 == "other"' | head -n 1)
  if [ $# -gt 0 ]; then
    if nginxish "$3"; then say "nginx listens on $2, which the site files do not name"; else say "$3 listens on $2, which the site files do not name"; fi
    exit 1
  fi
  say "nginx does not listen on $missing"
  exit 1
fi

# culprits: from `nginx -T` (a fixed, read-only command, asked once), each listen line on one of the ports whose address is not one the
# files name (a port alone or `*:80` is every address), as "<file>: <line>", at most 20 lines; nothing else of the configuration is printed.
# The last line is "none" when there is no such line.
culprits() {
  printf '%s\n' "$dump" | awk -v ports=" $1 " -v named="$NAMED" '
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
      if (found == 0) print "none"
    }' | LC_ALL=C tr -c '[:print:]\n' '?' | cut -c1-300
}
if [ -n "$strays" ]; then
  # each stray once per (port or address, owner): nginx's (or an unnamed owner's) is another configuration; another program's is that program
  for k in wild other; do printf '%s\n' "$strays" | awk -v k="$k" '$1 == k && !seen[$2 " " $3]++ { print $1, $2, $3 }'; done |
    while read -r kind where own; do
      case "$kind" in
        wild) if nginxish "$own"; then say "nginx also listens on every address on port $where: another configuration has a listen without an address"
              else say "$own listens on every address on port $where: another program holds the port, so nginx cannot listen there"; fi ;;
        other) if nginxish "$own"; then say "nginx listens on $where, which the site files do not name: another configuration listens there"
               else say "$own listens on $where, which the site files do not name: another program holds it"; fi ;;
      esac
    done
  printf '%s\n' "$strays" | cut -d' ' -f4- | head -n 10 | sed 's/^/    /' >&2
  # the listen lines are looked for only on the ports of the strays nginx holds (or that ss did not name an owner for)
  cports="$(printf '%s\n' "$strays" | awk '$3 == "nginx" || $3 == "-" { p = $2; sub(/.*:/, "", p); print p }' | LC_ALL=C sort -un | tr '\n' ' ')"
  foreign="$(printf '%s\n' "$strays" | awk '$3 != "nginx" && $3 != "-"' | head -n 1)"
  [ -n "$cports" ] || exit 1
  dump_rc=0; dump="$(bounded nginx -T 2>/dev/null)" || dump_rc=$?
  if [ "$dump_rc" != 0 ] || [ -z "$dump" ]; then say "nginx -T did not show the configuration, so the listen lines that cause it are not named"; exit 1; fi
  named_lines="$(culprits "${cports% }")"
  if [ "$named_lines" = none ]; then
    printf '    %s\n' "nginx -T names no such listen line (a server block with no listen line at all listens on every address on port 80)" >&2
    # stale: every stray socket is nginx's own, and the configuration it would load does not ask for any of them
    [ -z "$foreign" ] && [ -z "$(printf '%s\n' "$strays" | awk '$3 == "-"')" ] && exit 4
    exit 1
  fi
  printf '%s\n' "$named_lines" | sed 's/^/    /' >&2
  exit 1
fi
say "nginx did not take the new configuration: it does not listen on $missing; journalctl -u nginx says why"
exit 1
