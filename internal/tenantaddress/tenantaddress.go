// Package tenantaddress moves a tenant address on the tenant VLAN onto or off this machine (nuit-cloud/nuit-cloud#187), so an address
// can follow its service when the service moves to another Nuit Cloud Server.
//
// Every Nuit Cloud Server carries the tenant VLAN as a sub-interface, `<parent>.<vlanId>` or, where that would be longer than the kernel's
// 15 characters, `vlan<vlanId>` (nuit-cloud/nuit-cloud#197), configured by ifupdown from /etc/network/interfaces.d/nuit-cloud-vlan<vlanId>:
// the node's own address is the stanza's `address` line and each tenant address one line
//
//	post-up ip addr add 10.20.0.3/24 dev enp0s31f6.5 2>/dev/null || true
//
// The VLAN interface is the one interface the kernel reports as VLAN <vlanId> (`ip -d -j addr show`), whatever its name.
//
// (indented four spaces; written by the ncs_vlan feature, or by hand with an equivalent script). The task `tenant-address` changes that
// address both live (`ip addr add`/`del`) and in the file, so it survives a reboot. Every other line of the file is left byte for byte as
// it was. The ncs_vlan feature owns the interface and the node's own address (nuit-cloud/nuit-cloud#189): its check allows tenant lines it
// was not given, and its apply keeps every tenant line already in the file, so it never undoes this task's work.
//
//   - release removes the address from the VLAN interface and its line from the file. It never releases the node's own address. A primary
//     address with secondaries in its network is removed only with promote_secondaries on (turned on when it is off), so the others stay.
//   - take refuses an address that is on another interface of this machine, or that another machine on the VLAN still answers for (three
//     RFC 5227 ARP probes, about a second apart); then adds it live and to the file, and announces it with three gratuitous ARPs so the
//     router's ARP cache follows the address to this machine. Taking an address already here is not an error: the file line is made sure
//     of and the address announced again. The prefix is the VLAN's: the stanza's, or the one the node's own address already has while
//     the VLAN's network changes (nuit-cloud/nuit-cloud#197, /24 to /16); an address here with another prefix gets the task's before
//     the old entry goes, and its file line the new prefix.
//
// The platform makes sure the service using the address is stopped before a release; the agent refuses what would be unsafe on its own
// machine and on the wire.
package tenantaddress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Announcements is how many gratuitous ARPs a take sends, and Probes how many ARP probes it sends before adding an address.
const (
	Announcements = 3
	Probes        = 3
	// interval is the pause between two probes (spent listening for an answer) and between two announcements.
	interval = time.Second
	// maxIPOutput bounds what `ip -d -j addr show` may print.
	maxIPOutput = 4 << 20
	// maxFile bounds the VLAN file the agent will read and rewrite.
	maxFile = 256 << 10
)

// Params are what a tenant-address task carries.
type Params struct {
	Action  string // take or release
	Address netip.Addr
	Prefix  int
	VLANID  int
}

// only refuses any key that is not one of allowed: a key the agent does not understand is never silently ignored.
func only(m map[string]any, allowed ...string) error {
	for k := range m {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok {
			return fmt.Errorf("tenant-address carries only %s, not %q", strings.Join(allowed, ", "), k)
		}
	}
	return nil
}

// whole reads a JSON number that must be a whole number from lo to hi.
func whole(m map[string]any, k string, lo, hi int) (int, bool) {
	f, ok := m[k].(float64)
	if !ok || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, false
	}
	return int(f), true
}

// Parse checks a tenant-address task's params: exactly action (take or release), address (a dotted IPv4 address in 10/8, 172.16/12 or
// 192.168/16, neither the network nor the broadcast address of its prefix), prefix (16 to 30) and vlanId (1 to 4094).
func Parse(m map[string]any) (Params, error) {
	if err := only(m, "action", "address", "prefix", "vlanId"); err != nil {
		return Params{}, err
	}
	for _, k := range []string{"action", "address", "prefix", "vlanId"} {
		if _, ok := m[k]; !ok {
			return Params{}, fmt.Errorf("tenant-address needs %s", k)
		}
	}
	action, _ := m["action"].(string)
	if action != "take" && action != "release" {
		return Params{}, errors.New("the action is not take or release")
	}
	s, _ := m["address"].(string)
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.String() != s {
		return Params{}, errors.New("the address is not a dotted IPv4 address")
	}
	if !a.IsPrivate() {
		return Params{}, fmt.Errorf("%s is not a private address (10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16)", s)
	}
	prefix, ok := whole(m, "prefix", 16, 30)
	if !ok {
		return Params{}, errors.New("the prefix is not a whole number from 16 to 30")
	}
	vlan, ok := whole(m, "vlanId", 1, 4094)
	if !ok {
		return Params{}, errors.New("the vlanId is not a whole number from 1 to 4094")
	}
	block := netip.PrefixFrom(a, prefix).Masked()
	if a == block.Addr() || a == broadcast(block) {
		return Params{}, fmt.Errorf("%s is the network or broadcast address of %s", s, block)
	}
	return Params{Action: action, Address: a, Prefix: prefix, VLANID: vlan}, nil
}

func broadcast(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Link is a network interface as the ARP layer needs it.
type Link struct {
	Name  string
	Index int
	MAC   net.HardwareAddr
}

// Claim is what an ARP probe heard: another machine (MAC) that answers for the address, or (Probing) is itself probing for it. A zero
// Claim (nil MAC) means nobody did.
type Claim struct {
	MAC     net.HardwareAddr
	Probing bool
}

// ARP sends the ARP packets a take needs; injectable so tests need no root and no network.
type ARP interface {
	// Probe sends one RFC 5227 ARP probe for addr on the link (sender IP 0.0.0.0) and listens for wait: the first other machine that
	// claims the address is returned.
	Probe(ctx context.Context, link Link, addr netip.Addr, wait time.Duration) (Claim, error)
	// Announce sends one gratuitous ARP for addr from the link (a broadcast request with sender and target IP both addr).
	Announce(ctx context.Context, link Link, addr netip.Addr) error
}

// Mover does the task against one machine.
type Mover struct {
	// Root is prefixed to /etc/network/interfaces.d and /proc/sys (empty: the real root); tests use a scratch directory.
	Root string
	// IP runs `ip` with these arguments (no shell) and returns its standard output; nil runs the real one.
	IP func(ctx context.Context, args ...string) (string, error)
	// ARP sends the probes and announcements; nil uses a packet socket (Linux only).
	ARP ARP
	// Sleep waits between two announcements; nil waits for real.
	Sleep func(ctx context.Context, d time.Duration) error
}

// TakeResult is what a take reports. PrefixChanged says the address was here with another prefix and now has the task's (the tenant
// VLAN's network grew from /24 to /16, nuit-cloud/nuit-cloud#197).
type TakeResult struct {
	Action        string `json:"action"`
	Address       string `json:"address"`
	AlreadyHere   bool   `json:"alreadyHere"`
	FileChanged   bool   `json:"fileChanged"`
	Announcements int    `json:"announcements"`
	PrefixChanged bool   `json:"prefixChanged,omitempty"`
}

// ReleaseResult is what a release reports.
type ReleaseResult struct {
	Action      string `json:"action"`
	Address     string `json:"address"`
	WasPresent  bool   `json:"wasPresent"`
	FileChanged bool   `json:"fileChanged"`
}

func (m Mover) ip(ctx context.Context, args ...string) (string, error) {
	if m.IP != nil {
		return m.IP(ctx, args...)
	}
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "ip", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%v: %s", err, msg)
	}
	return out.String(), nil
}

func (m Mover) arp() ARP {
	if m.ARP != nil {
		return m.ARP
	}
	return packetARP{}
}

func (m Mover) sleep(ctx context.Context, d time.Duration) error {
	if m.Sleep != nil {
		return m.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// File is where the VLAN's configuration is, below root.
func File(root string, vlanID int) string {
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, "etc", "network", "interfaces.d", "nuit-cloud-vlan"+strconv.Itoa(vlanID))
}

// ifaceAddr is one address of an interface, as `ip -d -j addr show` gives it.
type ifaceAddr struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
	Secondary bool   `json:"secondary"`
}

// iface is one interface, as `ip -d -j addr show` gives it.
type iface struct {
	Index    int         `json:"ifindex"`
	Name     string      `json:"ifname"`
	MAC      string      `json:"address"`
	Addrs    []ifaceAddr `json:"addr_info"`
	LinkInfo *linkInfo   `json:"linkinfo,omitempty"`
}

// linkInfo is what `ip -d` says about the kind of an interface; for a VLAN, its 802.1Q id.
type linkInfo struct {
	Kind string `json:"info_kind"`
	Data struct {
		Protocol string `json:"protocol,omitempty"`
		ID       int    `json:"id"`
	} `json:"info_data"`
}

// vlanID is the 802.1Q VLAN id the kernel reports for the interface, and whether it is a VLAN interface at all.
func (i iface) vlanID() (int, bool) {
	if i.LinkInfo == nil || i.LinkInfo.Kind != "vlan" {
		return 0, false
	}
	if p := i.LinkInfo.Data.Protocol; p != "" && p != "802.1Q" {
		return 0, false
	}
	return i.LinkInfo.Data.ID, true
}

// has says whether the interface holds a (IPv4), and the entry for it.
func (i iface) has(a netip.Addr) (ifaceAddr, bool) {
	for _, x := range i.Addrs {
		if x.Family != "inet" {
			continue
		}
		if p, err := netip.ParseAddr(x.Local); err == nil && p == a {
			return x, true
		}
	}
	return ifaceAddr{}, false
}

// entries are every IPv4 entry the interface has for a. One address can be on an interface with two prefixes at once, each its own entry
// (checked on a real kernel, nuit-cloud/nuit-cloud#197): so it is while the tenant VLAN's network grows from /24 to /16.
func (i iface) entries(a netip.Addr) []ifaceAddr {
	var out []ifaceAddr
	for _, x := range i.Addrs {
		if x.Family != "inet" {
			continue
		}
		if p, err := netip.ParseAddr(x.Local); err == nil && p == a {
			out = append(out, x)
		}
	}
	return out
}

// hasEntry says whether the interface holds a with this prefix.
func (i iface) hasEntry(a netip.Addr, prefix int) bool {
	for _, e := range i.entries(a) {
		if e.PrefixLen == prefix {
			return true
		}
	}
	return false
}

// hasSecondaries says whether the network of e (its address with its prefix) has secondary addresses on the interface other than a's own
// entries: Linux removes them with their primary unless promote_secondaries is on.
func (i iface) hasSecondaries(e ifaceAddr, a netip.Addr) bool {
	p, err := netip.ParseAddr(e.Local)
	if err != nil {
		return false
	}
	block, err := p.Prefix(e.PrefixLen)
	if err != nil {
		return false
	}
	for _, x := range i.Addrs {
		o, err := netip.ParseAddr(x.Local)
		if err == nil && x.Family == "inet" && x.Secondary && o != a && x.PrefixLen == e.PrefixLen && block.Contains(o) {
			return true
		}
	}
	return false
}

// secondariesFirst is entries with the secondary ones first, so a primary is the last of them to go.
func secondariesFirst(entries []ifaceAddr) []ifaceAddr {
	var out []ifaceAddr
	for _, e := range entries {
		if e.Secondary {
			out = append(out, e)
		}
	}
	for _, e := range entries {
		if !e.Secondary {
			out = append(out, e)
		}
	}
	return out
}

// promoteFile is where the kernel shows the interface's promote_secondaries, below root.
func promoteFile(root, vif string) string {
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, "proc", "sys", "net", "ipv4", "conf", vif, "promote_secondaries")
}

// promote makes sure that removing the primary address of a network on the interface promotes one of its secondaries instead of removing
// them all (promote_secondaries; the ncs_vlan feature turns it on for the tenant VLAN too, nuit-cloud/nuit-cloud#197).
func (m Mover) promote(vif string) error {
	f := promoteFile(m.Root, vif)
	b, err := os.ReadFile(f)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) == "1" {
		return nil
	}
	w, err := os.OpenFile(f, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := w.Write([]byte("1\n"))
	if cerr := w.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if b, err = os.ReadFile(f); err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "1" {
		return errors.New("it is still off")
	}
	return nil
}

// vifName is the shape of an interface name the agent will write into the VLAN file: a kernel interface name (at most 15 characters,
// checked beside it) of plain characters, nothing a shell would read as anything but a word: <parent>.<id> or vlan<id>.
var vifName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*(\.[0-9]{1,4})?$`)

// machine is what both actions read first: the interfaces, the VLAN interface and the VLAN file.
type machine struct {
	ifaces  []iface
	vif     iface
	file    string
	content []byte
	mode    os.FileMode
	uid     int
	gid     int
	// node is the node's own address on the VLAN (the stanza's address line), and nodePrefix its prefix (0 when the line has none).
	node       netip.Addr
	nodePrefix int
}

func (m Mover) read(ctx context.Context, p Params) (machine, error) {
	out, err := m.ip(ctx, "-d", "-j", "addr", "show")
	if err != nil {
		return machine{}, fmt.Errorf("could not list this machine's interfaces: %w", err)
	}
	if len(out) > maxIPOutput {
		return machine{}, errors.New("could not list this machine's interfaces: the list is too long")
	}
	var ifaces []iface
	if err := json.Unmarshal([]byte(out), &ifaces); err != nil {
		return machine{}, fmt.Errorf("could not read this machine's interfaces: %w", err)
	}
	// the interface the kernel says carries the VLAN, whatever its name (<parent>.<id>, or vlan<id> where that would not fit)
	var found []iface
	for _, i := range ifaces {
		if id, ok := i.vlanID(); ok && id == p.VLANID {
			found = append(found, i)
		}
	}
	switch {
	case len(found) == 0:
		return machine{}, fmt.Errorf("this machine has no interface for VLAN %d", p.VLANID)
	case len(found) > 1:
		names := make([]string, len(found))
		for i, f := range found {
			names[i] = f.Name
		}
		return machine{}, fmt.Errorf("this machine has %d interfaces for VLAN %d (%s); it must have exactly one", len(found), p.VLANID, strings.Join(names, ", "))
	}
	vif := found[0]
	if !vifName.MatchString(vif.Name) || len(vif.Name) > 15 {
		return machine{}, fmt.Errorf("the VLAN interface's name %q is not one the agent writes into the VLAN configuration", vif.Name)
	}
	mc := machine{ifaces: ifaces, vif: vif, file: File(m.Root, p.VLANID)}
	noVLAN := fmt.Errorf("this machine has no tenant VLAN set up by the platform (no %s configuring %s)", File("", p.VLANID), vif.Name)
	fi, err := os.Lstat(mc.file)
	if errors.Is(err, os.ErrNotExist) {
		return machine{}, noVLAN
	}
	if err != nil {
		return machine{}, fmt.Errorf("could not read the VLAN configuration: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return machine{}, errors.New("the VLAN configuration is not a regular file; it is left as it is")
	}
	if fi.Size() > maxFile {
		return machine{}, errors.New("the VLAN configuration is too long to be the platform's")
	}
	mc.content, err = os.ReadFile(mc.file)
	if err != nil {
		return machine{}, fmt.Errorf("could not read the VLAN configuration: %w", err)
	}
	mc.mode = fi.Mode().Perm()
	mc.uid, mc.gid = -1, -1
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		mc.uid, mc.gid = int(st.Uid), int(st.Gid)
	}
	node, prefix, ok := stanzaAddress(mc.content, vif.Name)
	if !ok {
		return machine{}, noVLAN
	}
	mc.node, mc.nodePrefix = node, prefix
	return mc, nil
}

var (
	addressLine = regexp.MustCompile(`^\s*address\s+([0-9.]+)(?:/([0-9]{1,2}))?\s*$`)
	stanzaStart = regexp.MustCompile(`^\s*(iface|auto|allow-[A-Za-z0-9-]+|mapping|source|source-directory)\s`)
)

// stanzaAddress finds `iface <vif> inet static` in the file and the address line of that stanza: the node's own address.
func stanzaAddress(content []byte, vif string) (netip.Addr, int, bool) {
	ifaceLine := regexp.MustCompile(`^\s*iface\s+` + regexp.QuoteMeta(vif) + `\s+inet\s+static\s*$`)
	in := false
	for _, line := range strings.Split(string(content), "\n") {
		if ifaceLine.MatchString(line) {
			in = true
			continue
		}
		if !in {
			continue
		}
		if stanzaStart.MatchString(line) {
			break
		}
		if mm := addressLine.FindStringSubmatch(line); mm != nil {
			a, err := netip.ParseAddr(mm[1])
			if err != nil || !a.Is4() {
				return netip.Addr{}, 0, false
			}
			prefix := 0
			if mm[2] != "" {
				prefix, _ = strconv.Atoi(mm[2])
			}
			return a, prefix, true
		}
	}
	return netip.Addr{}, 0, false
}

// lines splits content into lines, each with its own newline (the last may have none), so that joining them again gives back every byte.
func lines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	return strings.SplitAfter(string(content), "\n")
}

// addsAddress says whether a line of the file adds exactly the address a (`ip addr add <a>/`, so not 10.20.0.30 for 10.20.0.3): the
// same test the profile check makes.
func addsAddress(line string, a netip.Addr) bool {
	return strings.Contains(line, "ip addr add "+a.String()+"/")
}

// TenantLine is the line the agent writes for a tenant address, the form the ncs_vlan feature writes.
func TenantLine(a netip.Addr, prefix int, vif string) string {
	return fmt.Sprintf("    post-up ip addr add %s/%d dev %s 2>/dev/null || true", a, prefix, vif)
}

// withoutAddress is the file without every line that adds a; changed says whether there was one.
func withoutAddress(content []byte, a netip.Addr) ([]byte, bool) {
	var b strings.Builder
	changed := false
	for _, l := range lines(content) {
		if addsAddress(l, a) {
			changed = true
			continue
		}
		b.WriteString(l)
	}
	return []byte(b.String()), changed
}

// withAddress is the file with a line for a: after the last line that adds an address, or after the stanza's address line when there is
// none. A file that already has a line for a with this prefix is returned as it is; one whose line has another prefix gets this one in
// that line, the rest of it left as it was (the network grew, nuit-cloud/nuit-cloud#197).
func withAddress(content []byte, a netip.Addr, prefix int, vif string) ([]byte, bool, error) {
	ls := lines(content)
	at := -1
	withPrefix := regexp.MustCompile(`ip addr add ` + regexp.QuoteMeta(a.String()) + `/([0-9]{1,2})`)
	for i, l := range ls {
		if addsAddress(l, a) {
			mm := withPrefix.FindStringSubmatchIndex(l)
			if mm == nil || l[mm[2]:mm[3]] == strconv.Itoa(prefix) {
				return content, false, nil
			}
			ls[i] = l[:mm[2]] + strconv.Itoa(prefix) + l[mm[3]:]
			return []byte(strings.Join(ls, "")), true, nil
		}
		if strings.Contains(l, "ip addr add ") {
			at = i
		}
	}
	if at < 0 {
		ifaceLine := regexp.MustCompile(`^\s*iface\s+` + regexp.QuoteMeta(vif) + `\s+inet\s+static\s*$`)
		in := false
		for i, l := range ls {
			t := strings.TrimRight(l, "\r\n")
			if ifaceLine.MatchString(t) {
				in = true
				continue
			}
			if in && stanzaStart.MatchString(t) {
				break
			}
			if in && addressLine.MatchString(t) {
				at = i
				break
			}
		}
	}
	if at < 0 {
		return nil, false, errors.New("the VLAN configuration has no address line to add the address after")
	}
	var b strings.Builder
	for i, l := range ls {
		b.WriteString(l)
		if i == at {
			if !strings.HasSuffix(l, "\n") {
				b.WriteString("\n")
			}
			b.WriteString(TenantLine(a, prefix, vif) + "\n")
		}
	}
	return []byte(b.String()), true, nil
}

// write replaces the VLAN file atomically: a temporary file beside it (its name starts with a dot, so ifupdown's `source` never reads it),
// with the same mode and owner, synced, renamed over it, and the directory synced.
func (mc machine) write(content []byte) error {
	dir := filepath.Dir(mc.file)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(mc.file)+".new-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := tmp.Chmod(mc.mode); err != nil {
		return err
	}
	if mc.uid >= 0 {
		if err := tmp.Chown(mc.uid, mc.gid); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, mc.file); err != nil {
		return err
	}
	done = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Release takes the address off this machine: live from the VLAN interface, and its line out of the VLAN file.
func (m Mover) Release(ctx context.Context, p Params) (ReleaseResult, error) {
	mc, err := m.read(ctx, p)
	if err != nil {
		return ReleaseResult{}, err
	}
	a := p.Address
	if a == mc.node {
		return ReleaseResult{}, fmt.Errorf("%s is this machine's own address on the VLAN; it is never released", a)
	}
	res := ReleaseResult{Action: "release", Address: a.String()}
	// every entry of the address goes (it may be there with two prefixes while the network grows, #197)
	live := mc.vif.entries(a)
	for _, e := range live {
		if !e.Secondary && mc.vif.hasSecondaries(e, a) {
			// Linux removes the secondary addresses of a network with its primary one unless promote_secondaries is on (a former primary,
			// such as the node's old own address kept as a tenant address)
			if err := m.promote(mc.vif.Name); err != nil {
				return ReleaseResult{}, fmt.Errorf("%s is the primary address of its network on %s and promote_secondaries could not be turned on (%v); removing it would remove the other addresses with it", a, mc.vif.Name, err)
			}
			break
		}
	}
	content, changed := withoutAddress(mc.content, a)
	for _, e := range secondariesFirst(live) {
		if _, err := m.ip(ctx, "addr", "del", fmt.Sprintf("%s/%d", a, e.PrefixLen), "dev", mc.vif.Name); err != nil {
			return ReleaseResult{}, fmt.Errorf("could not remove %s/%d from %s: %w", a, e.PrefixLen, mc.vif.Name, err)
		}
	}
	res.WasPresent = len(live) > 0
	if changed {
		if err := mc.write(content); err != nil {
			return ReleaseResult{}, fmt.Errorf("removed %s from %s, but could not take it out of the VLAN configuration (it would come back at the next boot): %w", a, mc.vif.Name, err)
		}
	}
	res.FileChanged = changed
	return res, nil
}

// Take puts the address on this machine (live on the VLAN interface and in the VLAN file) and announces it, unless another machine on the
// VLAN still answers for it.
func (m Mover) Take(ctx context.Context, p Params) (TakeResult, error) {
	mc, err := m.read(ctx, p)
	if err != nil {
		return TakeResult{}, err
	}
	a := p.Address
	if a == mc.node {
		return TakeResult{}, fmt.Errorf("%s is this machine's own address on the VLAN, not a tenant address", a)
	}
	// The task's prefix is the VLAN's: the stanza's, or, while the VLAN's network is changing (/24 to /16, #197), the one the node's own
	// address already has on the interface. Anything else is a real mismatch.
	if mc.nodePrefix != 0 && p.Prefix != mc.nodePrefix && !mc.vif.hasEntry(mc.node, p.Prefix) {
		return TakeResult{}, fmt.Errorf("the prefix /%d is not the VLAN's on this machine (/%d)", p.Prefix, mc.nodePrefix)
	}
	for _, i := range mc.ifaces {
		if i.Name == mc.vif.Name {
			continue
		}
		if _, ok := i.has(a); ok {
			return TakeResult{}, fmt.Errorf("%s is on %s on this machine, not on the VLAN interface %s", a, i.Name, mc.vif.Name)
		}
	}
	mac, err := net.ParseMAC(mc.vif.MAC)
	if err != nil || len(mac) != 6 {
		return TakeResult{}, fmt.Errorf("%s has no Ethernet address to announce %s from", mc.vif.Name, a)
	}
	link := Link{Name: mc.vif.Name, Index: mc.vif.Index, MAC: mac}
	content, fileChange, err := withAddress(mc.content, a, p.Prefix, mc.vif.Name)
	if err != nil {
		return TakeResult{}, err
	}
	// An address here already with another prefix (the network grew, #197) gets the task's beside it first, and the old entry goes after:
	// it is never missing. Removing an old entry that is the primary of its network needs promote_secondaries, or the network's other
	// addresses would go with it; that is made sure of before anything changes.
	entries := mc.vif.entries(a)
	here := len(entries) > 0
	var old []ifaceAddr
	for _, e := range entries {
		if e.PrefixLen != p.Prefix {
			old = append(old, e)
		}
	}
	atPrefix := len(entries) > len(old)
	for _, e := range old {
		if !e.Secondary && mc.vif.hasSecondaries(e, a) {
			if err := m.promote(mc.vif.Name); err != nil {
				return TakeResult{}, fmt.Errorf("%s is on %s with /%d as the primary address of its network, and promote_secondaries could not be turned on (%v); moving it to /%d would remove the other addresses with it", a, mc.vif.Name, e.PrefixLen, err, p.Prefix)
			}
			break
		}
	}
	res := TakeResult{Action: "take", Address: a.String(), AlreadyHere: here}
	arp := m.arp()
	added := false
	if here && !atPrefix {
		if _, err := m.ip(ctx, "addr", "add", fmt.Sprintf("%s/%d", a, p.Prefix), "dev", mc.vif.Name); err != nil {
			return TakeResult{}, fmt.Errorf("could not add %s/%d to %s: %w", a, p.Prefix, mc.vif.Name, err)
		}
		added = true
	}
	if !here {
		for i := 0; i < Probes; i++ {
			c, err := arp.Probe(ctx, link, a, interval)
			if err != nil {
				return TakeResult{}, fmt.Errorf("could not check whether another machine holds %s: %w", a, err)
			}
			if c.MAC != nil && c.Probing {
				return TakeResult{}, fmt.Errorf("%s is being taken on the VLAN by %s: another machine is taking it", a, c.MAC)
			}
			if c.MAC != nil {
				return TakeResult{}, fmt.Errorf("%s is still answered on the VLAN by %s: another machine holds it", a, c.MAC)
			}
		}
		if _, err := m.ip(ctx, "addr", "add", fmt.Sprintf("%s/%d", a, p.Prefix), "dev", mc.vif.Name); err != nil {
			return TakeResult{}, fmt.Errorf("could not add %s to %s: %w", a, mc.vif.Name, err)
		}
		added = true
	}
	if fileChange {
		if err := mc.write(content); err != nil {
			if added {
				// the task's own time may be what ran out: the address is still taken off again
				undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				if _, derr := m.ip(undo, "addr", "del", fmt.Sprintf("%s/%d", a, p.Prefix), "dev", mc.vif.Name); derr != nil {
					return TakeResult{}, fmt.Errorf("could not add %s to the VLAN configuration (%v), and could not remove it from %s again: %w", a, err, mc.vif.Name, derr)
				}
				return TakeResult{}, fmt.Errorf("could not add %s to the VLAN configuration, so it was removed from %s again: %w", a, mc.vif.Name, err)
			}
			return TakeResult{}, fmt.Errorf("%s is on %s, but could not be added to the VLAN configuration: %w", a, mc.vif.Name, err)
		}
	}
	res.FileChanged = fileChange
	for _, e := range secondariesFirst(old) {
		if _, err := m.ip(ctx, "addr", "del", fmt.Sprintf("%s/%d", a, e.PrefixLen), "dev", mc.vif.Name); err != nil {
			return TakeResult{}, fmt.Errorf("%s is on %s with /%d, but its old /%d could not be removed (taking it again does): %w", a, mc.vif.Name, p.Prefix, e.PrefixLen, err)
		}
	}
	res.PrefixChanged = len(old) > 0
	for i := 0; i < Announcements; i++ {
		if i > 0 {
			if err := m.sleep(ctx, interval); err != nil {
				return TakeResult{}, fmt.Errorf("%s is on %s, but was announced only %d times: %w", a, mc.vif.Name, i, err)
			}
		}
		if err := arp.Announce(ctx, link, a); err != nil {
			return TakeResult{}, fmt.Errorf("%s is on %s, but could not be announced on the VLAN (taking it again announces it): %w", a, mc.vif.Name, err)
		}
		res.Announcements++
	}
	return res, nil
}
