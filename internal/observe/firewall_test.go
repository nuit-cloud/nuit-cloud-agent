package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// nft 1.1.3's output for the table the observe-mode rules load (Debian 13), with a counter of another table that must be ignored.
const nftCounters = `{"nftables": [{"metainfo": {"version": "1.1.3", "release_name": "Commodore Bullmoose #4", "json_schema_version": 1}}, ` +
	`{"counter": {"family": "inet", "name": "pod_input", "table": "nuit_cloud", "handle": 3, "packets": 12, "bytes": 960}}, ` +
	`{"counter": {"family": "inet", "name": "pod_private", "table": "nuit_cloud", "handle": 4, "packets": 3400, "bytes": 512000}}, ` +
	`{"counter": {"family": "ip", "name": "admin_other", "table": "other", "handle": 9, "packets": 99, "bytes": 99}}]}`

const kernelLog = `nc-pod_private IN=cni0 OUT=eth0 MAC=aa:bb:cc:dd:ee:ff:11:22:33:44:55:66:08:00 SRC=10.89.0.5 DST=10.10.10.140 LEN=60 TOS=0x00 PREC=0x00 TTL=63 ID=4242 DF PROTO=TCP SPT=40312 DPT=5432 WINDOW=64240 RES=0x00 SYN URGP=0
nc-pod_private IN=cni0 OUT=eth0 SRC=10.89.0.6 DST=10.10.10.140 LEN=60 PROTO=TCP SPT=40313 DPT=5432 SYN URGP=0
nc-pod_private IN=cni0 OUT=eth0 SRC=10.89.0.5 DST=10.10.10.140 LEN=60 PROTO=TCP SPT=40314 DPT=5432 SYN URGP=0
nc-pod_input IN=ocisaas0 OUT= SRC=10.89.0.7 DST=10.10.10.161 LEN=70 PROTO=UDP SPT=5353 DPT=5335 LEN=50
nc-pod_input IN=ocisaas0 OUT= SRC=fd00::7 DST=fd00::1 LEN=70 PROTO=UDP SPT=5353 DPT=5335 LEN=50
nc-admin_other IN=eth0 OUT=cni0 SRC=192.0.2.9 DST=10.89.0.9 LEN=60 PROTO=TCP SPT=5000 DPT=5380 SYN
nc-pod_private IN=cni0 OUT=eth0 SRC=10.89.0.5 DST=10.20.0.1 LEN=84 PROTO=ICMP TYPE=8 CODE=0 ID=1 SEQ=1
nc-other_rule IN=cni0 SRC=10.89.0.5 DST=10.20.0.9 PROTO=TCP DPT=22
nc-pod_private IN=cni0 SRC=10.89.0.5 DST=10.20.0.300 PROTO=TCP DPT=22
nc-pod_private IN=cni0 SRC=10.89.0.5 DST=10.20.0.8;x PROTO=TCP DPT=22
some other kernel line DST=10.0.0.1 DPT=1
`

func firewallCollector(nft string, nftErr error, rules string, log string) (Collector, *[]string) {
	var ran []string
	return Collector{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			switch name {
			case "nft":
				return nft, nftErr
			case "journalctl":
				return log, nil
			}
			panic("the firewall observation may only run nft and journalctl, not " + name)
		},
		ReadFirewallRules: func() (string, error) {
			if rules == "" {
				return "", errors.New("no such file")
			}
			return rules, nil
		},
	}, &ran
}

func TestTheFirewallObservationReportsTheModeTheCountersAndWhatWasCountedWithoutSources(t *testing.T) {
	c, ran := firewallCollector(nftCounters, nil, "# Written by the nuit-cloud agent (profile nuit-cloud-firewall). Edit the task, not this file.\n# pod-mode: observe\ntable inet nuit_cloud\n", kernelLog)
	data, v, err := c.Collect(context.Background(), Firewall)
	if err != nil || !versionOK.MatchString(v) {
		t.Fatalf("%v %q", err, v)
	}
	f := data.(FirewallFacts)
	if !f.Loaded || f.Mode != "observe" {
		t.Errorf("%+v", f)
	}
	if f.Counters["pod_input"] != (FirewallCounter{12, 960}) || f.Counters["pod_private"] != (FirewallCounter{3400, 512000}) || f.Counters["admin_other"] != (FirewallCounter{}) || len(f.Counters) != 3 {
		t.Errorf("counters %+v", f.Counters)
	}
	want := []FirewallHit{
		{Rule: "pod_private", Dst: "10.10.10.140", Port: 5432, Proto: "tcp", Count: 3},
		{Rule: "admin_other", Dst: "10.89.0.9", Port: 5380, Proto: "tcp", Count: 1},
		{Rule: "pod_input", Dst: "10.10.10.161", Port: 5335, Proto: "udp", Count: 1},
		{Rule: "pod_private", Dst: "10.20.0.1", Port: 0, Proto: "other", Count: 1},
	}
	if fmt.Sprint(f.Recent) != fmt.Sprint(want) {
		t.Errorf("recent\n got %+v\nwant %+v", f.Recent, want)
	}
	b, _ := json.Marshal(f)
	if strings.Contains(string(b), "10.89.0.5") || strings.Contains(string(b), "SRC") || strings.Contains(string(b), "192.0.2.9") {
		t.Errorf("a source address was reported: %s", b)
	}
	if !strings.Contains(string(b), `"counters":{"admin_other":{"packets":0,"bytes":0}`) || !strings.Contains(string(b), `"recent":[{"rule":"pod_private","dst":"10.10.10.140","port":5432,"proto":"tcp","count":3}`) {
		t.Errorf("shape: %s", b)
	}
	if len(*ran) != 2 || (*ran)[0] != "nft -j list counters table inet nuit_cloud" || !strings.HasPrefix((*ran)[1], "journalctl -k --since=-24h -g ^nc- -o cat") {
		t.Errorf("ran %q", *ran)
	}
	// the same facts are the same version; a counter that moved is a new one
	_, again, _ := c.Collect(context.Background(), Firewall)
	c2, _ := firewallCollector(strings.Replace(nftCounters, `"packets": 12`, `"packets": 13`, 1), nil, "# x\n# pod-mode: observe\n", kernelLog)
	_, moved, _ := c2.Collect(context.Background(), Firewall)
	if again != v || moved == v {
		t.Errorf("%q %q %q", v, again, moved)
	}
}

func TestTheFirewallObservationIsOffWithoutNftOrTheTableAndNeverFails(t *testing.T) {
	for name, c := range map[string]Collector{
		"no nft or no table": func() Collector {
			c, _ := firewallCollector("", errors.New("No such file or directory"), "# pod-mode: enforce\n", kernelLog)
			return c
		}(),
		"no way to run anything": {},
	} {
		data, v, err := c.Collect(context.Background(), Firewall)
		f := data.(FirewallFacts)
		if err != nil || !versionOK.MatchString(v) || f.Loaded || f.Mode != "off" || len(f.Recent) != 0 || f.Recent == nil || len(f.Counters) != 3 || f.Counters["pod_private"] != (FirewallCounter{}) {
			t.Errorf("%s: %+v %q %v", name, f, v, err)
		}
	}
	// the table without pod rules (or no rules file): loaded, off, and the kernel log is not read
	for _, rules := range []string{"# Written by the nuit-cloud agent\ntable inet nuit_cloud\n", ""} {
		c, ran := firewallCollector(`{"nftables": [{"metainfo": {"version": "1.1.3"}}]}`, nil, rules, kernelLog)
		data, _, _ := c.Collect(context.Background(), Firewall)
		if f := data.(FirewallFacts); !f.Loaded || f.Mode != "off" || len(f.Recent) != 0 || len(*ran) != 1 {
			t.Errorf("%q: %+v %q", rules, f, *ran)
		}
	}
	// output nft could not have meant is zeros, not a failure
	c, _ := firewallCollector("not json", nil, "# pod-mode: observe\n", "")
	data, _, _ := c.Collect(context.Background(), Firewall)
	if f := data.(FirewallFacts); !f.Loaded || f.Counters["pod_input"] != (FirewallCounter{}) || f.Recent == nil {
		t.Errorf("%+v", f)
	}
}

func TestTheFirewallModeComesOnlyFromTheHeader(t *testing.T) {
	for rules, want := range map[string]string{
		"# Written\n# pod-mode: observe\ntable inet nuit_cloud\n": "observe",
		"# Written\n# pod-mode: enforce\n":                        "enforce",
		"# Written\n# pod-mode: permissive\n":                     "off",
		"# Written\ntable inet nuit_cloud\n# pod-mode: enforce\n": "off",
		"": "off",
	} {
		if got := FirewallMode(rules); got != want {
			t.Errorf("%q: %q, want %q", rules, got, want)
		}
	}
}

func TestTheRecentListKeepsTheTwentyMostFrequent(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		for n := 0; n < i; n++ {
			fmt.Fprintf(&b, "nc-pod_private IN=cni0 SRC=10.89.0.2 DST=10.10.%d.1 PROTO=UDP DPT=53\n", i)
		}
	}
	got := ParseFirewallLog(b.String())
	if len(got) != maxFirewallRecent || got[0].Dst != "10.10.30.1" || got[0].Count != 30 || got[19].Dst != "10.10.11.1" {
		t.Errorf("%+v", got)
	}
}
