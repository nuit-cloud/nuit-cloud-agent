package tenantaddress

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentFile is what the ncs_vlan feature writes for 10.20.0.2/24 with the tenant addresses 10.20.0.3, 10.20.0.30 and 10.20.0.4.
const agentFile = `# Managed by the nuit-cloud agent (ncs_vlan): the tenant VLAN slice the platform allocated for this machine.
auto enp0s31f6.5
iface enp0s31f6.5 inet static
    address 10.20.0.2/24
    pre-up modprobe 8021q 2>/dev/null || true
    pre-up ip link show enp0s31f6.5 >/dev/null 2>&1 || ip link add link enp0s31f6 name enp0s31f6.5 type vlan id 5
    pre-up sysctl -qw net.ipv4.conf.enp0s31f6/5.promote_secondaries=1 2>/dev/null || true
    post-up ip addr add 10.20.0.3/24 dev enp0s31f6.5 2>/dev/null || true
    post-up ip addr add 10.20.0.30/24 dev enp0s31f6.5 2>/dev/null || true
    post-up ip addr add 10.20.0.4/24 dev enp0s31f6.5 2>/dev/null || true
    post-up sysctl -qw net.ipv4.conf.enp0s31f6/5.rp_filter=2
    post-up iptables -t mangle -C PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5 2>/dev/null || iptables -t mangle -A PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5
    post-up iptables -t mangle -C PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || iptables -t mangle -A PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark
    post-up ip route replace default via 10.20.0.1 dev enp0s31f6.5 table 105
    post-up ip rule show | grep -q "fwmark 0x5 lookup 105" || ip rule add fwmark 0x5 lookup 105 priority 1050
    post-up ip -4 rule show | grep -Ewq 'to 10\.89\.0\.0/16 iif ocisaas0 ([[]detached[]] )?lookup main' || ip rule add to 10.89.0.0/16 iif ocisaas0 lookup main priority 1040
    post-up ip -4 rule show | grep -Ewq 'to 10\.20\.0\.0/24 iif ocisaas0 ([[]detached[]] )?lookup main' || ip rule add to 10.20.0.0/24 iif ocisaas0 lookup main priority 1041
    post-up ip -4 rule show | grep -Ewq 'iif ocisaas0 ([[]detached[]] )?lookup 105' || ip rule add iif ocisaas0 lookup 105 priority 1045
    pre-down ip rule del iif ocisaas0 lookup 105 2>/dev/null || true
    pre-down ip rule del to 10.20.0.0/24 iif ocisaas0 lookup main 2>/dev/null || true
    pre-down ip rule del to 10.89.0.0/16 iif ocisaas0 lookup main 2>/dev/null || true
    pre-down ip rule del fwmark 0x5 lookup 105 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5 2>/dev/null || true
    pre-down ip route flush table 105 2>/dev/null || true
    post-down ip link del enp0s31f6.5 2>/dev/null || true
`

// agentFile16 is agentFile once the network grew to /16 and the node has 10.20.255.2 (nuit-cloud/nuit-cloud#197): the ncs_vlan feature
// rewrites the address line, gives every tenant line the new prefix, and moves the pods' tenant VLAN rule to the /16 network.
var agentFile16 = strings.ReplaceAll(strings.ReplaceAll(strings.Replace(strings.ReplaceAll(agentFile, "/24 dev", "/16 dev"), "    address 10.20.0.2/24\n", "    address 10.20.255.2/16\n", 1),
	"to 10.20.0.0/24 iif", "to 10.20.0.0/16 iif"), `to 10\.20\.0\.0/24 iif`, `to 10\.20\.0\.0/16 iif`)

// handFile is what nuit-cloud-servers scripts/server-vlan5.sh writes: a comment before the tenant addresses and an extra pre-down line.
const handFile = `# Managed by deploy/server-vlan5.sh (nuit-cloud). Remove with: sh server-vlan5.sh --remove
auto enp0s31f6.5
iface enp0s31f6.5 inet static
    address 10.20.0.2/24
    pre-up modprobe 8021q
    pre-up ip link show enp0s31f6.5 >/dev/null 2>&1 || ip link add link enp0s31f6 name enp0s31f6.5 type vlan id 5
    # secondary addresses for tenants' DNS
    post-up ip addr add 10.20.0.3/24 dev enp0s31f6.5 2>/dev/null || true
    post-up ip addr add 10.20.0.30/24 dev enp0s31f6.5 2>/dev/null || true
    post-up ip addr add 10.20.0.4/24 dev enp0s31f6.5 2>/dev/null || true
    # replies for connections that came in on the VLAN leave by the VLAN gateway
    post-up sysctl -qw net.ipv4.conf.enp0s31f6/5.rp_filter=2
    post-up iptables -t mangle -C PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5 2>/dev/null || iptables -t mangle -A PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5
    post-up iptables -t mangle -C PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || iptables -t mangle -A PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark
    post-up ip route replace default via 10.20.0.1 dev enp0s31f6.5 table 105
    post-up ip rule show | grep -q "fwmark 0x5 lookup 105" || ip rule add fwmark 0x5 lookup 105 priority 1050
    pre-down ip rule del fwmark 0x5 lookup 105 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING ! -i enp0s31f6.5 -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || true
    # (the form before 20 Oct, which also marked the request itself and sent it back out the VLAN, if an old install is still loaded)
    pre-down iptables -t mangle -D PREROUTING -m connmark --mark 0x5 -j CONNMARK --restore-mark 2>/dev/null || true
    pre-down iptables -t mangle -D PREROUTING -i enp0s31f6.5 -m conntrack --ctstate NEW -j CONNMARK --set-mark 0x5 2>/dev/null || true
    pre-down ip route flush table 105 2>/dev/null || true
    post-down ip link del enp0s31f6.5 2>/dev/null || true
`

const (
	vifMAC   = "aa:bb:cc:dd:ee:01"
	otherMAC = "aa:bb:cc:dd:ee:99"
	line3    = "    post-up ip addr add 10.20.0.3/24 dev enp0s31f6.5 2>/dev/null || true\n"
)

// vlan is what `ip -d` reports for a VLAN interface with this 802.1Q id.
func vlan(id int) *linkInfo {
	l := &linkInfo{Kind: "vlan"}
	l.Data.Protocol, l.Data.ID = "802.1Q", id
	return l
}

// fakeMachine is a machine's interfaces as `ip` sees them, changed by the `ip addr add`/`del` it is asked to run the way the kernel
// changes them (checked on a real kernel, nuit-cloud/nuit-cloud#197): an address with another prefix is an entry of its own; the first
// entry of a network (address and prefix) is its primary, the rest secondaries; removing a primary removes its secondaries with it unless
// promote_secondaries (read from promote, a file, when set) is on, when the next one becomes the primary.
type fakeMachine struct {
	ifaces  []iface
	calls   []string
	addErr  error
	promote string
}

func sameNetwork(x ifaceAddr, p netip.Prefix) bool {
	a, err := netip.ParseAddr(x.Local)
	return err == nil && x.Family == "inet" && x.PrefixLen == p.Bits() && p.Masked().Contains(a)
}

func (f *fakeMachine) ip(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) == 4 && args[0] == "-d" && args[1] == "-j" {
		b, _ := json.Marshal(f.ifaces)
		return string(b), nil
	}
	if len(args) == 5 && args[0] == "addr" && args[3] == "dev" {
		pfx, err := netip.ParsePrefix(args[2])
		if err != nil {
			return "", err
		}
		for i := range f.ifaces {
			if f.ifaces[i].Name != args[4] {
				continue
			}
			switch args[1] {
			case "add":
				if f.addErr != nil {
					return "", f.addErr
				}
				secondary := false
				for _, x := range f.ifaces[i].Addrs {
					if x.Local == pfx.Addr().String() && x.PrefixLen == pfx.Bits() {
						return "", errors.New("RTNETLINK answers: File exists")
					}
					secondary = secondary || sameNetwork(x, pfx)
				}
				f.ifaces[i].Addrs = append(f.ifaces[i].Addrs, ifaceAddr{Family: "inet", Local: pfx.Addr().String(), PrefixLen: pfx.Bits(), Secondary: secondary})
				return "", nil
			case "del":
				for j, a := range f.ifaces[i].Addrs {
					if a.Local != pfx.Addr().String() || a.PrefixLen != pfx.Bits() || a.Family != "inet" {
						continue
					}
					f.ifaces[i].Addrs = append(f.ifaces[i].Addrs[:j:j], f.ifaces[i].Addrs[j+1:]...)
					if a.Secondary {
						return "", nil
					}
					b, _ := os.ReadFile(f.promote)
					promote := f.promote != "" && strings.TrimSpace(string(b)) == "1"
					var kept []ifaceAddr
					promoted := false
					for _, x := range f.ifaces[i].Addrs {
						if x.Secondary && sameNetwork(x, pfx) {
							if !promote {
								continue
							}
							if !promoted {
								x.Secondary, promoted = false, true
							}
						}
						kept = append(kept, x)
					}
					f.ifaces[i].Addrs = kept
					return "", nil
				}
				return "", errors.New("RTNETLINK answers: Cannot assign requested address")
			}
		}
		return "", errors.New("Cannot find device")
	}
	return "", errors.New("unexpected ip " + strings.Join(args, " "))
}

// changes are the ip commands that change something.
func (f *fakeMachine) changes() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "-d -j ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeMachine) on(name, addr string) bool {
	for _, i := range f.ifaces {
		if i.Name == name {
			_, ok := i.has(netip.MustParseAddr(addr))
			return ok
		}
	}
	return false
}

// newMachine has a loopback, the parent interface, a container bridge and the VLAN interface with the node's address and the given
// tenant addresses.
func newMachine(tenants ...string) *fakeMachine {
	vif := iface{Index: 4, Name: "enp0s31f6.5", MAC: vifMAC, Addrs: []ifaceAddr{{Family: "inet", Local: "10.20.0.2", PrefixLen: 24}}, LinkInfo: vlan(5)}
	for _, t := range tenants {
		vif.Addrs = append(vif.Addrs, ifaceAddr{Family: "inet", Local: t, PrefixLen: 24, Secondary: true})
	}
	return &fakeMachine{ifaces: []iface{
		{Index: 1, Name: "lo", MAC: "00:00:00:00:00:00", Addrs: []ifaceAddr{{Family: "inet", Local: "127.0.0.1", PrefixLen: 8}}},
		{Index: 2, Name: "enp0s31f6", MAC: vifMAC, Addrs: []ifaceAddr{{Family: "inet", Local: "192.168.1.20", PrefixLen: 24}, {Family: "inet6", Local: "fe80::1", PrefixLen: 64}}},
		{Index: 3, Name: "cni0", MAC: "aa:bb:cc:dd:ee:02", Addrs: []ifaceAddr{{Family: "inet", Local: "10.89.0.1", PrefixLen: 16}}},
		vif,
	}}
}

// newMachine16 is newMachine once the network grew to /16: the node's own 10.20.255.2/16 is the primary, the tenant addresses /16.
func newMachine16(tenants ...string) *fakeMachine {
	m := newMachine()
	m.ifaces[3].Addrs = []ifaceAddr{{Family: "inet", Local: "10.20.255.2", PrefixLen: 16}}
	for _, t := range tenants {
		m.ifaces[3].Addrs = append(m.ifaces[3].Addrs, ifaceAddr{Family: "inet", Local: t, PrefixLen: 16, Secondary: true})
	}
	return m
}

// onWith says whether the interface holds addr with this prefix.
func (f *fakeMachine) onWith(name, addr string, prefix int) bool {
	for _, i := range f.ifaces {
		if i.Name == name {
			return i.hasEntry(netip.MustParseAddr(addr), prefix)
		}
	}
	return false
}

// withPromote writes the VLAN interface's promote_secondaries below root as the kernel shows it, and has the fake read it.
func withPromote(t *testing.T, root string, m *fakeMachine, value string) string {
	t.Helper()
	f := promoteFile(root, "enp0s31f6.5")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(value+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.promote = f
	return f
}

// fakeARP records what was sent and answers probes with claim.
type fakeARP struct {
	claim     Claim
	probes    int
	announced []netip.Addr
	links     []Link
	err       error
}

func (a *fakeARP) Probe(_ context.Context, link Link, addr netip.Addr, wait time.Duration) (Claim, error) {
	a.probes++
	a.links = append(a.links, link)
	if a.err != nil {
		return Claim{}, a.err
	}
	return a.claim, nil
}

func (a *fakeARP) Announce(_ context.Context, link Link, addr netip.Addr) error {
	a.links = append(a.links, link)
	if a.err != nil {
		return a.err
	}
	a.announced = append(a.announced, addr)
	return nil
}

// scratch is a root with the VLAN file holding content (mode 0640, to see the mode is kept).
func scratch(t *testing.T, content string) (root, file string) {
	t.Helper()
	root = t.TempDir()
	file = File(root, 5)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return root, file
}

func mover(root string, m *fakeMachine, a *fakeARP) Mover {
	return Mover{Root: root, IP: m.ip, ARP: a, Sleep: func(context.Context, time.Duration) error { return nil }}
}

func params(action, addr string) Params {
	return Params{Action: action, Address: netip.MustParseAddr(addr), Prefix: 24, VLANID: 5}
}

func read(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseAcceptsExactlyTheFourKeys(t *testing.T) {
	good := map[string]any{"action": "take", "address": "10.20.0.3", "prefix": float64(24), "vlanId": float64(5)}
	p, err := Parse(good)
	if err != nil || p.Action != "take" || p.Address != netip.MustParseAddr("10.20.0.3") || p.Prefix != 24 || p.VLANID != 5 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, ok := range []map[string]any{
		{"action": "release", "address": "172.16.4.9", "prefix": float64(16), "vlanId": float64(4094)},
		{"action": "take", "address": "192.168.200.10", "prefix": float64(30), "vlanId": float64(1)},
	} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range good {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, bad := range map[string]map[string]any{
		"an extra key":         with("dev", "eth0"),
		"no action":            with("action", nil),
		"no address":           with("address", nil),
		"no prefix":            with("prefix", nil),
		"no vlanId":            with("vlanId", nil),
		"another action":       with("action", "steal"),
		"a public address":     with("address", "8.8.8.8"),
		"a shared address":     with("address", "100.64.0.3"),
		"a loopback address":   with("address", "127.0.0.3"),
		"an IPv6 address":      with("address", "fd00::3"),
		"a mapped address":     with("address", "::ffff:10.20.0.3"),
		"leading zeros":        with("address", "10.20.0.03"),
		"an address and mask":  with("address", "10.20.0.3/24"),
		"a shell word":         with("address", "10.20.0.3;reboot"),
		"a network address":    with("address", "10.20.0.0"),
		"a broadcast address":  with("address", "10.20.0.255"),
		"a prefix of 15":       with("prefix", float64(15)),
		"a prefix of 31":       with("prefix", float64(31)),
		"a fractional prefix":  with("prefix", 24.5),
		"a prefix as a string": with("prefix", "24"),
		"vlanId 0":             with("vlanId", float64(0)),
		"vlanId 4095":          with("vlanId", float64(4095)),
		"vlanId as a string":   with("vlanId", "5"),
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestReleaseRemovesExactlyTheAddressLineAndLeavesEveryOtherLineAsItWas(t *testing.T) {
	for name, content := range map[string]string{"agent-rendered": agentFile, "hand-made": handFile} {
		t.Run(name, func(t *testing.T) {
			root, file := scratch(t, content)
			m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4")
			a := &fakeARP{}
			res, err := mover(root, m, a).Release(context.Background(), params("release", "10.20.0.3"))
			if err != nil {
				t.Fatal(err)
			}
			if res != (ReleaseResult{Action: "release", Address: "10.20.0.3", WasPresent: true, FileChanged: true}) {
				t.Errorf("%+v", res)
			}
			if want := strings.Replace(content, line3, "", 1); read(t, file) != want {
				t.Errorf("the file is now\n%s\nwant\n%s", read(t, file), want)
			}
			if m.on("enp0s31f6.5", "10.20.0.3") || !m.on("enp0s31f6.5", "10.20.0.30") || !m.on("enp0s31f6.5", "10.20.0.2") {
				t.Errorf("live: %+v", m.ifaces[3].Addrs)
			}
			if got := m.changes(); len(got) != 1 || got[0] != "addr del 10.20.0.3/24 dev enp0s31f6.5" {
				t.Errorf("ran %v", got)
			}
			if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
				t.Errorf("mode %v", fi.Mode().Perm())
			}
			if a.probes != 0 || len(a.announced) != 0 {
				t.Error("a release sent ARP")
			}
			// again: nothing left to do, and nothing done
			before := read(t, file)
			m.calls = nil
			res, err = mover(root, m, a).Release(context.Background(), params("release", "10.20.0.3"))
			if err != nil || res.WasPresent || res.FileChanged {
				t.Errorf("%+v %v", res, err)
			}
			if read(t, file) != before || len(m.changes()) != 0 {
				t.Errorf("a second release changed something: %v", m.changes())
			}
			entries, _ := os.ReadDir(filepath.Dir(file))
			if len(entries) != 1 {
				t.Errorf("left behind: %v", entries)
			}
		})
	}
}

func TestReleaseNeverReleasesTheNodesOwnAddress(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3")
	_, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.2"))
	if err == nil || !strings.Contains(err.Error(), "this machine's own address") {
		t.Fatalf("%v", err)
	}
	if read(t, file) != agentFile || len(m.changes()) != 0 {
		t.Error("something was changed")
	}
}

func TestReleaseRefusesThePrimaryAddressOfANetworkWithSecondaries(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3")
	// the node's address is not there, so 10.20.0.3 became the primary one, with 10.20.0.4 secondary to it
	m.ifaces[3].Addrs = []ifaceAddr{{Family: "inet", Local: "10.20.0.3", PrefixLen: 24}, {Family: "inet", Local: "10.20.0.4", PrefixLen: 24, Secondary: true}}
	if _, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.3")); err == nil {
		t.Fatal("released a primary address")
	}
	if read(t, file) != agentFile || len(m.changes()) != 0 {
		t.Error("something was changed")
	}
}

func TestTakeAddsTheAddressAfterTheLastTenantLineAndAnnouncesIt(t *testing.T) {
	newLine := "    post-up ip addr add 10.20.0.7/24 dev enp0s31f6.5 2>/dev/null || true\n"
	after := "    post-up ip addr add 10.20.0.4/24 dev enp0s31f6.5 2>/dev/null || true\n"
	for name, content := range map[string]string{"agent-rendered": agentFile, "hand-made": handFile} {
		t.Run(name, func(t *testing.T) {
			root, file := scratch(t, content)
			m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4")
			a := &fakeARP{}
			res, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7"))
			if err != nil {
				t.Fatal(err)
			}
			if res != (TakeResult{Action: "take", Address: "10.20.0.7", FileChanged: true, Announcements: 3}) {
				t.Errorf("%+v", res)
			}
			if want := strings.Replace(content, after, after+newLine, 1); read(t, file) != want {
				t.Errorf("the file is now\n%s\nwant\n%s", read(t, file), want)
			}
			if got := m.changes(); len(got) != 1 || got[0] != "addr add 10.20.0.7/24 dev enp0s31f6.5" {
				t.Errorf("ran %v", got)
			}
			if a.probes != Probes || len(a.announced) != Announcements {
				t.Errorf("%d probes, %d announcements", a.probes, len(a.announced))
			}
			for _, l := range a.links {
				if l.Name != "enp0s31f6.5" || l.Index != 4 || l.MAC.String() != vifMAC {
					t.Errorf("ARP on %+v", l)
				}
			}
			if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
				t.Errorf("mode %v", fi.Mode().Perm())
			}
		})
	}
}

// With the management address on a VLAN of its own (nuit-cloud/nuit-cloud#197) the machine has enp0s31f6.31 beside the tenant VLAN
// enp0s31f6.5: the tenant VLAN is still the one interface for VLAN 5, and the management VLAN is never touched.
func TestAManagementVLANBesideTheTenantVLANIsLeftAlone(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4")
	m.ifaces = append(m.ifaces, iface{Index: 5, Name: "enp0s31f6.31", MAC: vifMAC, Addrs: []ifaceAddr{{Family: "inet", Local: "10.31.0.18", PrefixLen: 24}}, LinkInfo: vlan(31)})
	a := &fakeARP{}
	if _, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7")); err != nil {
		t.Fatal(err)
	}
	if got := m.changes(); len(got) != 1 || got[0] != "addr add 10.20.0.7/24 dev enp0s31f6.5" {
		t.Errorf("ran %v", got)
	}
	for _, l := range a.links {
		if l.Name != "enp0s31f6.5" {
			t.Errorf("ARP on %+v", l)
		}
	}
	if !strings.Contains(read(t, file), "ip addr add 10.20.0.7/24 dev enp0s31f6.5") || !m.on("enp0s31f6.31", "10.31.0.18") {
		t.Errorf("the file is\n%s", read(t, file))
	}
	// a management VLAN whose id ends in the tenant VLAN's digits (15, 105) is not a second interface for VLAN 5
	for name, id := range map[string]int{"enp0s31f6.15": 15, "enp0s31f6.105": 105} {
		root, _ := scratch(t, agentFile)
		m := newMachine()
		m.ifaces = append(m.ifaces, iface{Index: 5, Name: name, MAC: vifMAC, Addrs: []ifaceAddr{{Family: "inet", Local: "10.31.0.18", PrefixLen: 24}}, LinkInfo: vlan(id)})
		if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.7")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A parent whose name leaves no room for <parent>.5 (a Raspberry Pi's enx<mac> is 15 characters, the kernel's limit) carries the tenant
// VLAN as vlan5 (nuit-cloud/nuit-cloud#197). The VLAN interface is the one the kernel reports as VLAN 5, whichever name it has, and a
// VLAN 15 or 105 beside it is never taken for it, whatever its name.
func TestTheTenantVLANIsFoundByItsVLANIdWhateverItsName(t *testing.T) {
	for _, vif := range []string{"vlan5", "enp0s31f6.5"} {
		for _, others := range [][]string{nil, {"vlan15", "vlan105"}, {"enp0s31f6.15", "enp0s31f6.105"}, {"vlan15", "enp0s31f6.105"}} {
			root, file := scratch(t, strings.ReplaceAll(agentFile, "enp0s31f6.5", vif))
			m := newMachine("10.20.0.3")
			m.ifaces[3].Name = vif
			for i, o := range others {
				id := 15
				if strings.HasSuffix(o, "105") {
					id = 105
				}
				m.ifaces = append(m.ifaces, iface{Index: 10 + i, Name: o, MAC: vifMAC, Addrs: []ifaceAddr{{Family: "inet", Local: "10.31.0.18", PrefixLen: 24}}, LinkInfo: vlan(id)})
			}
			a := &fakeARP{}
			if _, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7")); err != nil {
				t.Fatalf("%s beside %v: %v", vif, others, err)
			}
			if got := m.changes(); len(got) != 1 || got[0] != "addr add 10.20.0.7/24 dev "+vif {
				t.Errorf("%s beside %v: ran %v", vif, others, got)
			}
			for _, l := range a.links {
				if l.Name != vif {
					t.Errorf("%s beside %v: ARP on %+v", vif, others, l)
				}
			}
			if !strings.Contains(read(t, file), TenantLine(netip.MustParseAddr("10.20.0.7"), 24, vif)) {
				t.Errorf("%s beside %v: the file is\n%s", vif, others, read(t, file))
			}
			if _, err := mover(root, m, a).Release(context.Background(), params("release", "10.20.0.3")); err != nil || m.on(vif, "10.20.0.3") || strings.Contains(read(t, file), "10.20.0.3/") {
				t.Errorf("%s beside %v: release: %v\n%s", vif, others, err, read(t, file))
			}
		}
	}
}

func TestTakeOnAMachineWithNoTenantAddressesAddsTheLineAfterTheAddressLine(t *testing.T) {
	var noTenants strings.Builder
	for _, l := range strings.SplitAfter(agentFile, "\n") {
		if !strings.Contains(l, "ip addr add") {
			noTenants.WriteString(l)
		}
	}
	root, file := scratch(t, noTenants.String())
	m := newMachine()
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.3")); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(noTenants.String(), "    address 10.20.0.2/24\n", "    address 10.20.0.2/24\n"+line3, 1)
	if read(t, file) != want {
		t.Errorf("the file is now\n%s", read(t, file))
	}
}

func TestTakingAnAddressAlreadyHereOnlyAnnouncesIt(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4")
	a := &fakeARP{}
	res, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.30"))
	if err != nil {
		t.Fatal(err)
	}
	if res != (TakeResult{Action: "take", Address: "10.20.0.30", AlreadyHere: true, Announcements: 3}) {
		t.Errorf("%+v", res)
	}
	if read(t, file) != agentFile || len(m.changes()) != 0 || a.probes != 0 || len(a.announced) != 3 {
		t.Errorf("changes %v, %d probes, %d announcements", m.changes(), a.probes, len(a.announced))
	}
	// live but missing from the file (taken before a failed write): the line is added, nothing else done
	m = newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4", "10.20.0.8")
	res, err = mover(root, m, a).Take(context.Background(), params("take", "10.20.0.8"))
	if err != nil || !res.AlreadyHere || !res.FileChanged || len(m.changes()) != 0 {
		t.Errorf("%+v %v %v", res, err, m.changes())
	}
	if !strings.Contains(read(t, file), "    post-up ip addr add 10.20.0.8/24 dev enp0s31f6.5 2>/dev/null || true\n") {
		t.Error("the line was not added")
	}
}

func TestTakeRefusesAnAddressAnotherMachineStillAnswersFor(t *testing.T) {
	for _, probing := range []bool{false, true} {
		root, file := scratch(t, agentFile)
		m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4")
		mac, _ := net.ParseMAC(otherMAC)
		a := &fakeARP{claim: Claim{MAC: mac, Probing: probing}}
		_, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7"))
		want := "10.20.0.7 is still answered on the VLAN by " + otherMAC + ": another machine holds it"
		if probing {
			want = "another machine is taking it"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v", err)
		}
		if read(t, file) != agentFile || len(m.changes()) != 0 || len(a.announced) != 0 {
			t.Error("something was changed")
		}
	}
}

func TestTakeRefusesAnAddressOnAnotherInterfaceOfThisMachine(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3")
	m.ifaces[2].Addrs = append(m.ifaces[2].Addrs, ifaceAddr{Family: "inet", Local: "10.20.0.7", PrefixLen: 32})
	a := &fakeARP{}
	_, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7"))
	if err == nil || !strings.Contains(err.Error(), "is on cni0") {
		t.Fatalf("%v", err)
	}
	if read(t, file) != agentFile || len(m.changes()) != 0 || a.probes != 0 || len(a.announced) != 0 {
		t.Error("something was changed")
	}
}

func TestTakeRefusesTheNodesAddressAndAnotherPrefix(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3")
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.2")); err == nil {
		t.Error("took the node's own address")
	}
	p := params("take", "10.20.0.7")
	p.Prefix = 16
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err == nil || !strings.Contains(err.Error(), "/16") {
		t.Errorf("%v", err)
	}
	if read(t, file) != agentFile || len(m.changes()) != 0 {
		t.Error("something was changed")
	}
}

func TestATakeWhoseFileCannotBeWrittenRemovesTheLiveAddressAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	root, file := scratch(t, agentFile)
	dir := filepath.Dir(file)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	m := newMachine("10.20.0.3")
	a := &fakeARP{}
	_, err := mover(root, m, a).Take(context.Background(), params("take", "10.20.0.7"))
	if err == nil || !strings.Contains(err.Error(), "removed from enp0s31f6.5 again") {
		t.Fatalf("%v", err)
	}
	if m.on("enp0s31f6.5", "10.20.0.7") || len(a.announced) != 0 {
		t.Error("the address was left live or announced")
	}
	if got := m.changes(); len(got) != 2 || got[1] != "addr del 10.20.0.7/24 dev enp0s31f6.5" {
		t.Errorf("ran %v", got)
	}
}

func TestWithoutOneVLANInterfaceAndItsFileNothingIsDone(t *testing.T) {
	cases := map[string]struct {
		machine func() *fakeMachine
		content string // "" leaves no file
		want    string
	}{
		"no VLAN interface": {func() *fakeMachine { m := newMachine(); m.ifaces = m.ifaces[:3]; return m }, agentFile, "no interface for VLAN 5"},
		"two VLAN interfaces": {func() *fakeMachine {
			m := newMachine()
			m.ifaces = append(m.ifaces, iface{Index: 9, Name: "eth1.5", MAC: otherMAC, LinkInfo: vlan(5)})
			return m
		}, agentFile, "2 interfaces for VLAN 5 (enp0s31f6.5, eth1.5)"},
		"no VLAN file":      {func() *fakeMachine { return newMachine() }, "", "no tenant VLAN set up by the platform"},
		"another interface": {func() *fakeMachine { return newMachine() }, strings.ReplaceAll(agentFile, "iface enp0s31f6.5 inet static", "iface eth0.5 inet static"), "no tenant VLAN set up by the platform"},
		"no address line":   {func() *fakeMachine { return newMachine() }, strings.ReplaceAll(agentFile, "    address 10.20.0.2/24\n", ""), "no tenant VLAN set up by the platform"},
		"VLAN 15 is not 5": {func() *fakeMachine {
			m := newMachine()
			m.ifaces[3].Name, m.ifaces[3].LinkInfo = "enp0s31f6.15", vlan(15)
			return m
		}, agentFile, "no interface for VLAN 5"},
		// the name is not what makes an interface VLAN 5: the kernel's VLAN id is
		"named .5, not a VLAN": {func() *fakeMachine { m := newMachine(); m.ifaces[3].LinkInfo = nil; return m }, agentFile, "no interface for VLAN 5"},
		"named .5, VLAN 15":    {func() *fakeMachine { m := newMachine(); m.ifaces[3].LinkInfo = vlan(15); return m }, agentFile, "no interface for VLAN 5"},
		"an 802.1ad VLAN 5": {func() *fakeMachine {
			m := newMachine()
			m.ifaces[3].LinkInfo.Data.Protocol = "802.1ad"
			return m
		}, agentFile, "no interface for VLAN 5"},
		"a hostile ifname":   {func() *fakeMachine { m := newMachine(); m.ifaces[3].Name = "a;reboot.5"; return m }, agentFile, "is not one the agent writes"},
		"a file, not a link": {func() *fakeMachine { return newMachine() }, "symlink", "not a regular file"},
	}
	for name, c := range cases {
		for _, action := range []string{"take", "release"} {
			root := t.TempDir()
			file := File(root, 5)
			_ = os.MkdirAll(filepath.Dir(file), 0o755)
			switch c.content {
			case "":
			case "symlink":
				target := filepath.Join(root, "elsewhere")
				_ = os.WriteFile(target, []byte(agentFile), 0o644)
				_ = os.Symlink(target, file)
			default:
				_ = os.WriteFile(file, []byte(c.content), 0o644)
			}
			m := c.machine()
			a := &fakeARP{}
			_, err := mover(root, m, a).Take(context.Background(), params(action, "10.20.0.3"))
			if action == "release" {
				_, err = mover(root, m, a).Release(context.Background(), params(action, "10.20.0.3"))
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s, %s: %v", name, action, err)
			}
			if len(m.changes()) != 0 || a.probes != 0 || len(a.announced) != 0 {
				t.Errorf("%s, %s: something was done", name, action)
			}
			if c.content != "" && c.content != "symlink" && read(t, file) != c.content {
				t.Errorf("%s, %s: the file changed", name, action)
			}
		}
	}
}

func TestTheProbeAndTheAnnouncementAreRFC5227Frames(t *testing.T) {
	mac, _ := net.ParseMAC(vifMAC)
	addr := netip.MustParseAddr("10.20.0.7")
	probe := probeFrame(mac, addr)
	want := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01, 0x08, 0x06, // broadcast, from us, ARP
		0, 1, 0x08, 0, 6, 4, 0, 1, // Ethernet, IPv4, request
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01, 0, 0, 0, 0, // sender: us, 0.0.0.0
		0, 0, 0, 0, 0, 0, 10, 20, 0, 7, // target: zero, the address
	}
	if len(probe) != 60 || string(probe[:42]) != string(want) {
		t.Errorf("probe % x", probe)
	}
	ann := announceFrame(mac, addr)
	copy(want[28:32], []byte{10, 20, 0, 7})
	if len(ann) != 60 || string(ann[:42]) != string(want) {
		t.Errorf("announcement % x", ann)
	}
	// what we sent ourselves is never a claim
	if _, ok := claimIn(probe, mac, addr); ok {
		t.Error("our own probe is a claim")
	}
	if _, ok := claimIn(ann, mac, addr); ok {
		t.Error("our own announcement is a claim")
	}
	other, _ := net.ParseMAC(otherMAC)
	reply := arpFrame(other, arpReply, other, addr, mac, netip.IPv4Unspecified())
	if c, ok := claimIn(reply, mac, addr); !ok || c.MAC.String() != otherMAC || c.Probing {
		t.Errorf("a reply from another machine: %+v %v", c, ok)
	}
	if c, ok := claimIn(announceFrame(other, addr), mac, addr); !ok || c.Probing {
		t.Errorf("another machine's announcement: %+v %v", c, ok)
	}
	if c, ok := claimIn(probeFrame(other, addr), mac, addr); !ok || !c.Probing {
		t.Errorf("another machine's probe: %+v %v", c, ok)
	}
	if _, ok := claimIn(arpFrame(other, arpReply, other, netip.MustParseAddr("10.20.0.70"), mac, addr), mac, addr); ok {
		t.Error("a reply for another address is a claim")
	}
	if _, ok := claimIn(reply[:41], mac, addr); ok {
		t.Error("a short frame is a claim")
	}
	notARP := append([]byte(nil), reply...)
	notARP[13] = 0x00
	if _, ok := claimIn(notARP, mac, addr); ok {
		t.Error("an IPv4 frame is a claim")
	}
}

// While the network changes (nuit-cloud/nuit-cloud#197) the stanza may still say /24 when the node's own address is already live with /16:
// a take with /16 is then the VLAN's prefix. With the node's address only at /24 it is a real mismatch, refused as before.
func TestTakeAcceptsTheNewPrefixOnceTheNodesOwnAddressHasIt(t *testing.T) {
	root, file := scratch(t, agentFile)
	m := newMachine("10.20.0.3")
	m.ifaces[3].Addrs = append(m.ifaces[3].Addrs, ifaceAddr{Family: "inet", Local: "10.20.0.2", PrefixLen: 16})
	p := params("take", "10.20.0.7")
	p.Prefix = 16
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !m.onWith("enp0s31f6.5", "10.20.0.7", 16) || !strings.Contains(read(t, file), "ip addr add 10.20.0.7/16 dev enp0s31f6.5") {
		t.Errorf("%v\n%s", m.changes(), read(t, file))
	}
	p.Address, p.Prefix = netip.MustParseAddr("10.20.0.8"), 20
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err == nil || !strings.Contains(err.Error(), "/20") {
		t.Errorf("a prefix neither the stanza's nor the node's: %v", err)
	}
}

// An address here already with the old prefix (/24) gets the task's (/16) beside it before the old entry goes, so it is never missing; its
// file line gets the new prefix; no probe is sent (the address is this machine's), and it is announced again.
func TestTakeOfAnAddressHereWithTheOldPrefixMovesItWithoutAGap(t *testing.T) {
	content := strings.Replace(agentFile16, "10.20.0.30/16", "10.20.0.30/24", 1)
	root, file := scratch(t, content)
	m := newMachine16("10.20.0.3", "10.20.0.4")
	m.ifaces[3].Addrs = append(m.ifaces[3].Addrs, ifaceAddr{Family: "inet", Local: "10.20.0.30", PrefixLen: 24})
	a := &fakeARP{}
	p := params("take", "10.20.0.30")
	p.Prefix = 16
	res, err := mover(root, m, a).Take(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if res != (TakeResult{Action: "take", Address: "10.20.0.30", AlreadyHere: true, FileChanged: true, Announcements: 3, PrefixChanged: true}) {
		t.Errorf("%+v", res)
	}
	if got := m.changes(); len(got) != 2 || got[0] != "addr add 10.20.0.30/16 dev enp0s31f6.5" || got[1] != "addr del 10.20.0.30/24 dev enp0s31f6.5" {
		t.Errorf("ran %v (the new prefix must come before the old one goes)", got)
	}
	if read(t, file) != agentFile16 || !m.onWith("enp0s31f6.5", "10.20.0.30", 16) || m.onWith("enp0s31f6.5", "10.20.0.30", 24) || a.probes != 0 || len(a.announced) != 3 {
		t.Errorf("file\n%s\nlive %+v, %d probes", read(t, file), m.ifaces[3].Addrs, a.probes)
	}
	// again: in place, so only announced
	m.calls = nil
	if res, err := mover(root, m, a).Take(context.Background(), p); err != nil || res.PrefixChanged || res.FileChanged || len(m.changes()) != 0 {
		t.Errorf("%+v %v %v", res, err, m.changes())
	}
}

// Moving an address that is the primary of its old network with secondaries in it needs promote_secondaries: without it (it cannot be
// read or turned on) nothing changes; with it off it is turned on first, and the other addresses stay.
func TestTakeMovesAFormerPrimaryOnlyWithPromoteSecondaries(t *testing.T) {
	content := strings.Replace(strings.Replace(agentFile16, "10.20.0.30/16", "10.20.0.30/24", 1), "10.20.0.4/16", "10.20.0.4/24", 1)
	setup := func() *fakeMachine {
		m := newMachine16("10.20.0.3")
		m.ifaces[3].Addrs = append(m.ifaces[3].Addrs, ifaceAddr{Family: "inet", Local: "10.20.0.30", PrefixLen: 24}, ifaceAddr{Family: "inet", Local: "10.20.0.4", PrefixLen: 24, Secondary: true})
		return m
	}
	p := params("take", "10.20.0.30")
	p.Prefix = 16
	root, file := scratch(t, content)
	m := setup()
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err == nil || !strings.Contains(err.Error(), "promote_secondaries") {
		t.Errorf("%v", err)
	}
	if read(t, file) != content || len(m.changes()) != 0 {
		t.Error("something was changed")
	}
	pf := withPromote(t, root, m, "0")
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(pf); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("promote_secondaries is %q", b)
	}
	if !m.onWith("enp0s31f6.5", "10.20.0.30", 16) || m.onWith("enp0s31f6.5", "10.20.0.30", 24) || !m.onWith("enp0s31f6.5", "10.20.0.4", 24) {
		t.Errorf("live %+v", m.ifaces[3].Addrs)
	}
}

// A former primary (the node's old own address kept as a tenant address, the first /16 entry before the node's new address) is released
// with promote_secondaries turned on, and every other address stays; an address there with two prefixes loses both.
func TestReleaseOfAFormerPrimaryKeepsTheOthers(t *testing.T) {
	content := strings.Replace(agentFile16, "10.20.0.30/16", "10.20.0.34/16", 1)
	root, file := scratch(t, content)
	m := newMachine()
	m.ifaces[3].Addrs = []ifaceAddr{{Family: "inet", Local: "10.20.0.34", PrefixLen: 16}, {Family: "inet", Local: "10.20.255.2", PrefixLen: 16, Secondary: true},
		{Family: "inet", Local: "10.20.0.3", PrefixLen: 16, Secondary: true}, {Family: "inet", Local: "10.20.0.34", PrefixLen: 24}}
	pf := withPromote(t, root, m, "0")
	res, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.34"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.WasPresent || !res.FileChanged || strings.Contains(read(t, file), "10.20.0.34") {
		t.Errorf("%+v\n%s", res, read(t, file))
	}
	if got := strings.Join(m.changes(), "|"); got != "addr del 10.20.0.34/16 dev enp0s31f6.5|addr del 10.20.0.34/24 dev enp0s31f6.5" {
		t.Errorf("ran %v", got)
	}
	if b, _ := os.ReadFile(pf); strings.TrimSpace(string(b)) != "1" || !m.on("enp0s31f6.5", "10.20.255.2") || !m.on("enp0s31f6.5", "10.20.0.3") || m.on("enp0s31f6.5", "10.20.0.34") {
		t.Errorf("promote %q, live %+v", b, m.ifaces[3].Addrs)
	}
}
