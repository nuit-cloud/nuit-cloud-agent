package observe

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

const neigh = `10.10.10.162 dev br0 lladdr 8E:FF:E6:55:1B:1E REACHABLE
10.10.10.250 dev br0  FAILED
10.10.10.1 dev br0 lladdr 74:ac:b9:11:22:33 PERMANENT
10.10.10.7 dev br0 lladdr zz:ac:b9:11:22:33 STALE
10.10.10.8 dev br0 INCOMPLETE
fe80::1 dev br0 lladdr 74:ac:b9:11:22:33 router STALE
junk
`

func TestParseNeighboursKeepsAnsweringIPv4EntriesWithAMACAndLowercasesIt(t *testing.T) {
	got := ParseNeighbours(neigh)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if got[0] != (Neighbour{Address: "10.10.10.1", MAC: "74:ac:b9:11:22:33", Interface: "br0", State: "PERMANENT"}) {
		t.Errorf("%+v", got[0])
	}
	if got[1] != (Neighbour{Address: "10.10.10.162", MAC: "8e:ff:e6:55:1b:1e", Interface: "br0", State: "REACHABLE"}) {
		t.Errorf("%+v", got[1])
	}
}

func fake(out string, err error) Collector {
	return Collector{
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			if name != "ip" || strings.Join(args, " ") != "-4 neigh show" {
				panic("an observation may only run `ip -4 neigh show`")
			}
			return out, err
		},
		Interfaces: func() []Interface {
			return []Interface{{Name: "eth0", MAC: "aa:aa:aa:aa:aa:aa", Addresses: []string{"10.10.10.161"}}, {Name: "veth1"}}
		},
	}
}

func TestTheNetworkObservationHasTheInterfacesAndTheNeighbours(t *testing.T) {
	data, _, err := fake(neigh, nil).Collect(context.Background(), Network)
	if err != nil {
		t.Fatal(err)
	}
	f := data.(NetworkFacts)
	if len(f.Interfaces) != 2 || f.Interfaces[0].MAC != "aa:aa:aa:aa:aa:aa" || f.Interfaces[1].Addresses == nil {
		t.Errorf("interfaces %+v", f.Interfaces)
	}
	if len(f.Neighbours) != 2 {
		t.Errorf("neighbours %+v", f.Neighbours)
	}
}

func TestAMachineWithoutIPStillReportsItsInterfaces(t *testing.T) {
	data, _, _ := fake("", errors.New("not found")).Collect(context.Background(), Network)
	f := data.(NetworkFacts)
	if len(f.Interfaces) != 2 || f.Neighbours == nil || len(f.Neighbours) != 0 {
		t.Errorf("%+v", f)
	}
}

func TestOnlyTheNetworkKindIsKnown(t *testing.T) {
	c := fake("", nil)
	if !c.Known("network") || c.Known("cpu") || c.Known("") {
		t.Error("the catalog is closed")
	}
	if data, v, _ := c.Collect(context.Background(), "cpu"); data != nil || v != "" {
		t.Error("an unknown kind collects nothing")
	}
}

func TestTheReportIsBounded(t *testing.T) {
	var ifs []Interface
	for i := 0; i < 50; i++ {
		ifs = append(ifs, Interface{Name: "if", Addresses: make([]string, 40)})
	}
	c := Collector{Interfaces: func() []Interface { return ifs }}
	f := c.network(context.Background())
	if len(f.Interfaces) != maxInterfaces || len(f.Interfaces[0].Addresses) != maxAddresses {
		t.Errorf("%d interfaces, %d addresses", len(f.Interfaces), len(f.Interfaces[0].Addresses))
	}
	var b strings.Builder
	for i := 0; i < 700; i++ {
		b.WriteString("10.1." + string(rune('0'+i%10)) + ".1 dev e lladdr aa:aa:aa:aa:aa:aa REACHABLE\n")
	}
	if n := len(ParseNeighbours(b.String())); n > maxNeighbours {
		t.Errorf("%d neighbours", n)
	}
}

func TestTheVersionChangesWithTheFactNotWithTheNoise(t *testing.T) {
	_, v1, _ := fake(neigh, nil).Collect(context.Background(), Network)
	// the same neighbours in another order and in another state are the same fact
	shuffled := "10.10.10.1 dev br0 lladdr 74:ac:b9:11:22:33 STALE\n10.10.10.162 dev br0 lladdr 8e:ff:e6:55:1b:1e DELAY\n"
	_, v2, _ := fake(shuffled, nil).Collect(context.Background(), Network)
	if v1 != v2 || len(v1) != 16 {
		t.Errorf("%q vs %q", v1, v2)
	}
	// a new neighbour, or a changed MAC, is a different one
	_, v3, _ := fake(shuffled+"10.10.10.9 dev br0 lladdr aa:aa:aa:aa:aa:01 REACHABLE\n", nil).Collect(context.Background(), Network)
	_, v4, _ := fake(strings.Replace(shuffled, "74:ac:b9:11:22:33", "74:ac:b9:11:22:34", 1), nil).Collect(context.Background(), Network)
	if v3 == v2 || v4 == v2 || v3 == v4 {
		t.Errorf("%q %q %q", v2, v3, v4)
	}
}

func TestTheClockObservationSaysHowFarAheadThisMachineIsAndMovesInTenSecondSteps(t *testing.T) {
	c := Collector{Clock: func() (time.Duration, bool) { return -2 * time.Hour, true }}
	data, v, _ := c.Collect(context.Background(), Clock)
	if f := data.(ClockFacts); f.AheadSeconds != 7200 || v != "c720" {
		t.Errorf("%+v %q", data, v)
	}
	c.Clock = func() (time.Duration, bool) { return -7203 * time.Second, true }
	if _, v2, _ := c.Collect(context.Background(), Clock); v2 != "c720" {
		t.Errorf("a few seconds of drift changed the version: %q", v2)
	}
	c.Clock = func() (time.Duration, bool) { return 0, false }
	if data, v3, _ := c.Collect(context.Background(), Clock); data != nil || v3 != "" {
		t.Error("nothing is reported before the offset is known")
	}
	if !c.Known(Clock) {
		t.Error("clock is in the catalog")
	}
}

func TestRuntimeObservationListsLeftoversAndStaysSilentWithoutTheRuntime(t *testing.T) {
	c := Collector{Run: func(_ context.Context, name string, args ...string) (string, error) {
		if name != RuntimeBinary || strings.Join(args, " ") != "--root "+RuntimeRoot+" leftovers" {
			t.Errorf("asked %s %v", name, args)
		}
		return `{"leftovers":[{"pid":30},{"pid":4}]}`, nil
	}}
	data, version, err := c.Collect(context.Background(), Runtime)
	if err != nil || !versionOK.MatchString(version) || len(data.(RuntimeFacts).LeftoverSupervisors) != 2 {
		t.Errorf("%v %q %v", data, version, err)
	}
	c.Run = func(context.Context, string, ...string) (string, error) { return `{"leftovers":[]}`, nil }
	if data, version, _ := c.Collect(context.Background(), Runtime); !versionOK.MatchString(version) || len(data.(RuntimeFacts).LeftoverSupervisors) != 0 {
		t.Errorf("none: %v %q", data, version)
	}
	c.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("no such file") }
	if data, _, _ := c.Collect(context.Background(), Runtime); data != nil {
		t.Errorf("a machine without the runtime reported %v", data)
	}
}

func TestDNSObservationReadsTheResolversAndAsksEachOfThem(t *testing.T) {
	asked := map[string]bool{}
	c := Collector{
		PlatformHost: "agent.ops.example.com",
		ReadResolvConf: func() (string, error) {
			return "# x\ndomain internal.example.com\nnameserver 10.10.10.151\nnameserver 10.20.0.22\nnameserver 1.2.3.4\nnameserver 9.9.9.9\n", nil
		},
		Lookup: func(_ context.Context, resolver, name string) bool {
			asked[resolver+" "+name] = true
			return resolver == "10.20.0.22" && name != "agent.ops.example.com"
		},
	}
	data, version, err := c.Collect(context.Background(), DNS)
	f, ok := data.(DNSFacts)
	if err != nil || !ok || len(f.Resolvers) != 3 || f.Search != "internal.example.com" || len(f.Tests) != 2 || f.Tests[0].Name != "agent.ops.example.com" || f.Tests[1].Name != "deb.debian.org" {
		t.Fatalf("%+v %q %v", data, version, err)
	}
	if f.Tests[0].Answers[1].OK || !f.Tests[1].Answers[1].OK || f.Tests[1].Answers[0].OK || len(asked) != 6 {
		t.Errorf("answers %+v asked %v", f.Tests, asked)
	}
	_, same, _ := c.Collect(context.Background(), DNS)
	c.Lookup = func(context.Context, string, string) bool { return true }
	_, changed, _ := c.Collect(context.Background(), DNS)
	if same != version || changed == version {
		t.Errorf("version %q %q %q", version, same, changed)
	}
	c.PlatformHost = "10.10.10.140"
	if d, _, _ := c.Collect(context.Background(), DNS); len(d.(DNSFacts).Tests) != 1 {
		t.Errorf("an address is not a name to ask for: %+v", d)
	}
	c.ReadResolvConf = func() (string, error) { return "", errors.New("no file") }
	if d, _, _ := c.Collect(context.Background(), DNS); d != nil {
		t.Errorf("an unreadable resolv.conf reported %v", d)
	}
}

var versionOK = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func TestRuntimeVersionIsAShortStableDigestOfWhatItReports(t *testing.T) {
	good := strings.Repeat("ab12", 16)
	run := `{"leftovers":[]}`
	record := ""
	c := Collector{
		Run:              func(context.Context, string, ...string) (string, error) { return run, nil },
		ReadBundleRecord: func() (string, error) { return record, nil },
	}
	ver := func() string { _, v, _ := c.Collect(context.Background(), Runtime); return v }
	seen := map[string]bool{}
	for _, r := range []string{`{"leftovers":[]}`, `{"leftovers":[{"pid":4}]}`, `{"leftovers":[{"pid":30},{"pid":4},{"pid":7}]}`} {
		for _, rec := range []string{"", good} {
			run, record = r, rec
			v := ver()
			if !versionOK.MatchString(v) || v != ver() || seen[v] {
				t.Errorf("%s %q: version %q", r, rec, v)
			}
			seen[v] = true
		}
	}
}

func TestEveryKindsVersionIsOneThePlatformKeeps(t *testing.T) {
	c := Collector{
		Run: func(_ context.Context, name string, _ ...string) (string, error) {
			if name == RuntimeBinary {
				return `{"leftovers":[{"pid":30},{"pid":4}]}`, nil
			}
			return "", nil
		},
		Interfaces: func() []Interface {
			return []Interface{{Name: "eth0", MAC: "aa:aa:aa:aa:aa:aa", Addresses: []string{"10.0.0.2"}}}
		},
		Clock:            func() (time.Duration, bool) { return -2 * time.Hour, true },
		ReadResolvConf:   func() (string, error) { return "nameserver 10.0.0.1\n", nil },
		Lookup:           func(context.Context, string, string) bool { return true },
		ReadBundleRecord: func() (string, error) { return strings.Repeat("ab12", 16), nil },
		TrustedKeyIDs:    func() []string { return []string{"fe812c12f3ab4ce6"} },
	}
	for _, kind := range []string{Network, Clock, Runtime, DNS, TaskKeys} {
		_, v, err := c.Collect(context.Background(), kind)
		if err != nil || !versionOK.MatchString(v) {
			t.Errorf("%s: version %q (%v)", kind, v, err)
		}
	}
}

func TestRuntimeObservationReportsTheInstalledBundleOnlyWhenItIsAChecksum(t *testing.T) {
	good := strings.Repeat("ab12", 16)
	record := ""
	var rerr error
	c := Collector{
		Run:              func(context.Context, string, ...string) (string, error) { return `{"leftovers":[{"pid":4}]}`, nil },
		ReadBundleRecord: func() (string, error) { return record, rerr },
	}
	collect := func() (RuntimeFacts, string) {
		d, v, _ := c.Collect(context.Background(), Runtime)
		return d.(RuntimeFacts), v
	}
	record = good + "\n"
	f, v1 := collect()
	if f.BundleSHA256 != good || !versionOK.MatchString(v1) {
		t.Errorf("good: %+v %q", f, v1)
	}
	if _, again := collect(); again != v1 {
		t.Errorf("version changed with nothing changed: %q %q", v1, again)
	}
	record = strings.Repeat("cd34", 16)
	if _, v2 := collect(); v2 == v1 {
		t.Error("a changed bundle kept the same version")
	}
	for _, bad := range []string{"unknown\n", "abc123", strings.ToUpper(good), good + "0", ""} {
		record = bad
		f, v := collect()
		if f.BundleSHA256 != "" || len(f.LeftoverSupervisors) != 1 || !versionOK.MatchString(v) {
			t.Errorf("%q: %+v %q", bad, f, v)
		}
	}
	record, rerr = good, errors.New("permission denied")
	if f, _ := collect(); f.BundleSHA256 != "" || len(f.LeftoverSupervisors) != 1 {
		t.Errorf("unreadable: %+v", f)
	}
	rerr = nil
	c.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("no runtime") }
	if d, _, _ := c.Collect(context.Background(), Runtime); d != nil {
		t.Errorf("no runtime but the record exists reported %v", d)
	}
}

func TestEveryKindTheAgentCanCollectIsOneItSaysItKnows(t *testing.T) {
	// A kind that is collected but not known is never asked for: it is silently never sent (the dns kind was missed this way in v0.3.28).
	c := Collector{
		Run:            func(context.Context, string, ...string) (string, error) { return `{"leftovers":[]}`, nil },
		Interfaces:     func() []Interface { return nil },
		Clock:          func() (time.Duration, bool) { return 0, true },
		ReadResolvConf: func() (string, error) { return "nameserver 10.0.0.1\n", nil },
		Lookup:         func(context.Context, string, string) bool { return true },

		ReadBundleRecord: func() (string, error) { return "", errors.New("none") },
		TrustedKeyIDs:    func() []string { return []string{"fe812c12f3ab4ce6"} },
	}
	for _, kind := range []string{Network, Clock, Runtime, DNS, TaskKeys} {
		if !c.Known(kind) {
			t.Errorf("%s is not a known kind", kind)
		}
		if data, version, err := c.Collect(context.Background(), kind); err != nil || data == nil || version == "" {
			t.Errorf("%s collected nothing: %v %q %v", kind, data, version, err)
		}
	}
	if c.Known("invented") {
		t.Error("an invented kind is known")
	}
}

// The platform learns which task-signing keys an agent trusts, so it can see one left trusting only a retired key. The version is the same
// whatever order the keys were trusted in, and changes when the set does.
func TestTheTaskKeysObservationNamesTheTrustedKeyIdsAndChangesOnlyWithThem(t *testing.T) {
	ids := []string{"fe812c12f3ab4ce6", "0123456789abcdef"}
	c := Collector{TrustedKeyIDs: func() []string { return ids }}
	data, v1, err := c.Collect(context.Background(), TaskKeys)
	if err != nil || !versionOK.MatchString(v1) {
		t.Fatalf("version %q (%v)", v1, err)
	}
	if got := data.(TaskKeysFacts).KeyIDs; len(got) != 2 || got[0] != "0123456789abcdef" || got[1] != "fe812c12f3ab4ce6" {
		t.Errorf("key ids: %v", got)
	}
	ids = []string{"0123456789abcdef", "fe812c12f3ab4ce6"}
	if _, v2, _ := c.Collect(context.Background(), TaskKeys); v2 != v1 {
		t.Errorf("the same keys in another order changed the version: %q, %q", v1, v2)
	}
	ids = []string{"0123456789abcdef"}
	if _, v3, _ := c.Collect(context.Background(), TaskKeys); v3 == v1 {
		t.Error("retiring a key did not change the version")
	}
	// Anything that is not a key id is not sent; an agent with no key to report sends nothing.
	ids = []string{"not-a-key-id", "FE812C12F3AB4CE6"}
	if data, v, _ := c.Collect(context.Background(), TaskKeys); data != nil || v != "" {
		t.Errorf("sent %v %q", data, v)
	}
	if data, _, _ := (Collector{}).Collect(context.Background(), TaskKeys); data != nil {
		t.Errorf("a collector without the agent's keys sent %v", data)
	}
}
