// Package observe collects the small read-only facts the platform asks the agent to attach to its check-in (ADR 0001, amendment: observations).
//
// It is deliberately not a collector: nothing runs between check-ins, nothing is sent that the platform did not ask for, and the kinds are a closed list
// compiled in here. A read is made just before a call and is passive: the interface list and the neighbour table, never a probe. The one exception
// is `dns`, which asks the machine's own resolvers two questions (the platform's name and a public one) because whether a name resolves can only be
// found out by asking; it sends nothing but those queries, and only when the platform lists the kind.
package observe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Kind names.
const (
	Network = "network"
	// Clock is how far this machine's clock is ahead of the platform's.
	Clock = "clock"
	// Runtime is what the container runtime on this machine has left behind: supervisors that serve no container (and so hold disks locked).
	Runtime = "runtime"
	// DNS is which resolvers this machine uses and whether each one answers for the platform's own name and for a public one.
	DNS = "dns"
	// TaskKeys is which task-signing keys this agent trusts (their ids), so the platform can see an agent left trusting only a retired key.
	TaskKeys = "task-keys"
)

// maxTaskKeys bounds the task-keys observation (an agent trusts at most four; the platform enforces its own limit again).
const maxTaskKeys = 8

var taskKeyID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// publicTestName is a public name every Debian machine needs to reach today (its package mirror); a resolver that cannot answer it cannot install anything.
const publicTestName = "deb.debian.org"

// RuntimeBinary and RuntimeRoot are where the container runtime and its state are on a machine it is installed on.
const (
	RuntimeBinary = "/usr/local/bin/oci-saas"
	RuntimeRoot   = "/var/lib/oci-saas/state"
	// RuntimeBundleRecord holds the SHA-256 of the installed runtime bundle (or the word `unknown`), written by the oci_saas_runtime profile feature.
	RuntimeBundleRecord = "/etc/oci-saas/installed-bundle"
)

var bundleSum = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Limits (the platform enforces the same ones again).
const (
	maxInterfaces = 32
	maxAddresses  = 16
	maxNeighbours = 512
)

// Interface is one network interface of this machine.
type Interface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses"`
}

// Neighbour is an address this machine has recently heard from, as its neighbour (ARP) table says.
type Neighbour struct {
	Address   string `json:"address"`
	MAC       string `json:"mac"`
	Interface string `json:"interface,omitempty"`
	State     string `json:"state,omitempty"`
}

// ClockFacts is the `clock` observation: how many seconds this machine's clock is ahead of the platform's (negative: behind).
type ClockFacts struct {
	AheadSeconds int64 `json:"aheadSeconds"`
}

// RuntimeFacts is the `runtime` observation: the processes (supervisors) the runtime's own `leftovers` command finds serving no container.
type RuntimeFacts struct {
	LeftoverSupervisors []int `json:"leftoverSupervisors"`
	// BundleSHA256 is the installed runtime bundle's checksum; empty (and left out) when unrecorded, unreadable or not a checksum.
	BundleSHA256 string `json:"bundleSha256,omitempty"`
}

// DNSAnswer is one resolver's answer for one name: whether it returned an address.
type DNSAnswer struct {
	Resolver string `json:"resolver"`
	OK       bool   `json:"ok"`
}

// DNSTest is one name asked of every resolver.
type DNSTest struct {
	Name    string      `json:"name"`
	Answers []DNSAnswer `json:"answers"`
}

// DNSFacts is the `dns` observation: the resolvers /etc/resolv.conf lists (at most three are used) and what each answered.
type DNSFacts struct {
	Resolvers []string  `json:"resolvers"`
	Search    string    `json:"search,omitempty"`
	Tests     []DNSTest `json:"tests"`
}

// TaskKeysFacts is the `task-keys` observation: the ids of the task-signing keys this agent trusts, sorted.
type TaskKeysFacts struct {
	KeyIDs []string `json:"keyIds"`
}

// NetworkFacts is the `network` observation.
type NetworkFacts struct {
	Interfaces []Interface `json:"interfaces"`
	Neighbours []Neighbour `json:"neighbours"`
}

// Collector reads the facts; every part can be replaced in tests.
type Collector struct {
	// Run runs a command and returns what it printed; a non-nil error means it failed.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Interfaces lists this machine's interfaces.
	Interfaces func() []Interface
	// PlatformHost is the name the agent calls the platform by; asked of every resolver, since a machine that cannot resolve it cannot find the platform by name.
	PlatformHost string
	// ReadResolvConf returns /etc/resolv.conf's text.
	ReadResolvConf func() (string, error)
	// Lookup asks one resolver (an IPv4 address) for the addresses of a name.
	Lookup func(ctx context.Context, resolver, name string) bool
	// Clock is the platform's time minus this machine's, once measured (the client does that at each check-in).
	Clock func() (time.Duration, bool)
	// ReadBundleRecord returns the text of the installed-bundle record; nil means no record is read.
	ReadBundleRecord func() (string, error)
	// TrustedKeyIDs returns the ids of the task-signing keys the agent trusts (state.State.KeyIDs).
	TrustedKeyIDs func() []string
}

func realRun(ctx context.Context, name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// Real is a Collector on this machine.
func Real() Collector {
	return Collector{Run: realRun, Interfaces: realInterfaces, ReadBundleRecord: func() (string, error) {
		b, err := os.ReadFile(RuntimeBundleRecord)
		return string(b), err
	}}
}

func realInterfaces() []Interface {
	var out []Interface
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		var addrs []string
		list, _ := ifc.Addrs()
		for _, a := range list {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				addrs = append(addrs, ipn.IP.String())
			}
		}
		out = append(out, Interface{Name: ifc.Name, MAC: strings.ToLower(ifc.HardwareAddr.String()), Addresses: addrs})
	}
	return out
}

// Known says whether this agent can collect a kind. A kind the platform asks for that is not here is simply not sent.
func (c Collector) Known(kind string) bool {
	return kind == Network || kind == Clock || kind == Runtime || kind == DNS || kind == TaskKeys
}

// Collect reads one kind: what is sent as the observation's `data`, and its version. The version is a digest of what the fact *is*, so a fact that has
// not changed has the same version and the agent can leave it unsent; it leaves out what changes without the fact changing (a neighbour's state flips between
// REACHABLE and STALE all the time).
func (c Collector) Collect(ctx context.Context, kind string) (any, string, error) {
	if kind == Clock {
		if c.Clock == nil {
			return nil, "", nil
		}
		d, ok := c.Clock()
		if !ok {
			return nil, "", nil
		}
		ahead := int64(math.Round(-d.Seconds()))
		// The version moves in steps of ten seconds, so ordinary drift costs nothing to report.
		return ClockFacts{AheadSeconds: ahead}, "c" + strconv.FormatInt(int64(math.Round(float64(ahead)/10)), 10), nil
	}
	if kind == Runtime {
		return c.runtime(ctx)
	}
	if kind == DNS {
		return c.dns(ctx)
	}
	if kind == TaskKeys {
		return c.taskKeys()
	}
	if kind != Network {
		return nil, "", nil
	}
	facts := c.network(ctx)
	return facts, facts.Version(), nil
}

// runtime asks the runtime what it has left behind. A machine without the runtime (or one whose answer cannot be read) reports nothing at all, which is
// not the same as reporting none.
func (c Collector) runtime(ctx context.Context) (any, string, error) {
	out, err := c.Run(ctx, RuntimeBinary, "--root", RuntimeRoot, "leftovers")
	if err != nil {
		return nil, "", nil
	}
	var parsed struct {
		Leftovers []struct {
			PID int `json:"pid"`
		} `json:"leftovers"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return nil, "", nil
	}
	pids := []int{}
	for _, l := range parsed.Leftovers {
		pids = append(pids, l.PID)
	}
	sort.Ints(pids)
	if len(pids) > 16 {
		pids = pids[:16]
	}
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.Itoa(p)
	}
	bundle := ""
	if c.ReadBundleRecord != nil {
		if text, err := c.ReadBundleRecord(); err == nil && bundleSum.MatchString(strings.TrimSpace(text)) {
			bundle = strings.TrimSpace(text)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ",") + "|" + bundle))
	version := "r" + hex.EncodeToString(sum[:])[:12]
	return RuntimeFacts{LeftoverSupervisors: pids, BundleSHA256: bundle}, version, nil
}

// taskKeys reads the ids of the keys the agent trusts from its own state: nothing on the machine is asked. An agent that trusts no key
// (it cannot be sent a task) reports nothing.
func (c Collector) taskKeys() (any, string, error) {
	if c.TrustedKeyIDs == nil {
		return nil, "", nil
	}
	ids := []string{}
	for _, id := range c.TrustedKeyIDs() {
		if taskKeyID.MatchString(id) && len(ids) < maxTaskKeys {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	return TaskKeysFacts{KeyIDs: ids}, "k" + hex.EncodeToString(sum[:])[:12], nil
}

var resolverLine = regexp.MustCompile(`^nameserver\s+(\d{1,3}(?:\.\d{1,3}){3})\s*$`)
var ipv4Name = regexp.MustCompile(`^\d{1,3}(?:\.\d{1,3}){3}$`)

// dns reads the resolvers the machine uses and asks each of them for a platform name and a public one, two seconds apiece. Nothing is
// changed; what is learned is whether the machine can find the platform and its package source by name, and through which resolver.
func (c Collector) dns(ctx context.Context) (any, string, error) {
	read := c.ReadResolvConf
	if read == nil {
		read = func() (string, error) { b, err := os.ReadFile("/etc/resolv.conf"); return string(b), err }
	}
	text, err := read()
	if err != nil {
		return nil, "", nil
	}
	facts := DNSFacts{Resolvers: []string{}, Tests: []DNSTest{}}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if m := resolverLine.FindStringSubmatch(line); m != nil && len(facts.Resolvers) < 3 {
			facts.Resolvers = append(facts.Resolvers, m[1])
		} else if (strings.HasPrefix(line, "search ") || strings.HasPrefix(line, "domain ")) && facts.Search == "" {
			facts.Search = strings.TrimSpace(line[strings.Index(line, " ")+1:])
		}
	}
	names := []string{}
	if c.PlatformHost != "" && !ipv4Name.MatchString(c.PlatformHost) {
		names = append(names, c.PlatformHost)
	}
	names = append(names, publicTestName)
	lookup := c.Lookup
	if lookup == nil {
		lookup = realLookup
	}
	for _, name := range names {
		test := DNSTest{Name: name, Answers: []DNSAnswer{}}
		for _, r := range facts.Resolvers {
			test.Answers = append(test.Answers, DNSAnswer{Resolver: r, OK: lookup(ctx, r, name)})
		}
		facts.Tests = append(facts.Tests, test)
	}
	return facts, facts.version(), nil
}

// version is a digest of what the resolvers are and whether each answered, so a machine whose resolution does not change sends nothing.
func (f DNSFacts) version() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s", strings.Join(f.Resolvers, ","), f.Search)
	for _, t := range f.Tests {
		fmt.Fprintf(h, "|%s", t.Name)
		for _, a := range t.Answers {
			fmt.Fprintf(h, ",%s=%t", a.Resolver, a.OK)
		}
	}
	return "d" + hex.EncodeToString(h.Sum(nil))[:12]
}

// realLookup asks one resolver directly (not the machine's own configuration) for a name's addresses.
func realLookup(ctx context.Context, resolver, name string) bool {
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "udp", net.JoinHostPort(resolver, "53"))
	}}
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	addrs, err := r.LookupHost(ctx, name)
	return err == nil && len(addrs) > 0
}

// Version is a short digest of the interfaces and the neighbours (address, hardware address, interface), in a fixed order and without the neighbours' state.
func (f NetworkFacts) Version() string {
	var b strings.Builder
	ifs := append([]Interface(nil), f.Interfaces...)
	sort.SliceStable(ifs, func(i, j int) bool { return ifs[i].Name < ifs[j].Name })
	for _, i := range ifs {
		addrs := append([]string(nil), i.Addresses...)
		sort.Strings(addrs)
		b.WriteString("i|" + i.Name + "|" + i.MAC + "|" + strings.Join(addrs, ",") + "\n")
	}
	ns := append([]Neighbour(nil), f.Neighbours...)
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].Address < ns[j].Address })
	for _, n := range ns {
		b.WriteString("n|" + n.Address + "|" + n.MAC + "|" + n.Interface + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

func (c Collector) network(ctx context.Context) NetworkFacts {
	facts := NetworkFacts{Interfaces: []Interface{}, Neighbours: []Neighbour{}}
	if c.Interfaces != nil {
		for _, ifc := range c.Interfaces() {
			if len(facts.Interfaces) >= maxInterfaces {
				break
			}
			if len(ifc.Addresses) > maxAddresses {
				ifc.Addresses = ifc.Addresses[:maxAddresses]
			}
			if ifc.Addresses == nil {
				ifc.Addresses = []string{}
			}
			facts.Interfaces = append(facts.Interfaces, ifc)
		}
	}
	if c.Run != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		// A machine without `ip` still reports its interfaces; its neighbours are simply not known.
		if out, err := c.Run(ctx, "ip", "-4", "neigh", "show"); err == nil {
			facts.Neighbours = ParseNeighbours(out)
		}
	}
	return facts
}

var macRe = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)

// ParseNeighbours reads `ip -4 neigh show`: `10.10.10.1 dev eth0 lladdr aa:bb:cc:dd:ee:ff REACHABLE`. An entry with no hardware address, or one that
// failed or is incomplete, is a neighbour that did not answer and is left out.
func ParseNeighbours(output string) []Neighbour {
	out := []Neighbour{}
	for _, line := range strings.Split(output, "\n") {
		w := strings.Fields(line)
		if len(w) < 1 {
			continue
		}
		ip := net.ParseIP(w[0])
		if ip == nil || ip.To4() == nil {
			continue
		}
		n := Neighbour{Address: ip.To4().String()}
		for i := 0; i+1 < len(w); i++ {
			switch w[i] {
			case "dev":
				n.Interface = w[i+1]
			case "lladdr":
				n.MAC = strings.ToLower(w[i+1])
			}
		}
		if !macRe.MatchString(n.MAC) {
			continue
		}
		if last := w[len(w)-1]; last == strings.ToUpper(last) && last != "" && !strings.ContainsAny(last, ":.0123456789") {
			n.State = last
		}
		if n.State == "FAILED" || n.State == "INCOMPLETE" {
			continue
		}
		out = append(out, n)
		if len(out) >= maxNeighbours {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}
