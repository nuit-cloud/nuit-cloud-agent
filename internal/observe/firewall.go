package observe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The firewall's pod rules, as the ncs_firewall profile feature writes them (internal/profiles/bootstrap/features.sh).
const (
	// FirewallRules is the rules file; its header says the pod rules' mode (`# pod-mode: observe`).
	FirewallRules = "/etc/nuit-cloud/firewall.nft"
	// FirewallTable is the nftables table (family inet) the rules load into.
	FirewallTable = "nuit_cloud"
	// maxFirewallRecent bounds the recently counted destinations reported.
	maxFirewallRecent = 20
	// maxFirewallLogLines bounds how much of the kernel log is read (the rules log at most 10 packets a minute each).
	maxFirewallLogLines = 5000
)

// FirewallCounters are the pod rules' named counters, in the order they are reported.
var FirewallCounters = []string{"pod_input", "pod_private", "admin_other"}

// FirewallCounter is one named counter's totals since the rules were last loaded.
type FirewallCounter struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// FirewallHit is one destination a pod rule counted in the last 24 hours (as logged; the log is rate-limited, so Count is a sample).
// Nothing about the source is kept.
type FirewallHit struct {
	Rule  string `json:"rule"`
	Dst   string `json:"dst"`
	Port  int    `json:"port"`
	Proto string `json:"proto"`
	Count int    `json:"count"`
}

// FirewallFacts is the `firewall` observation.
type FirewallFacts struct {
	Loaded   bool                       `json:"loaded"`
	Mode     string                     `json:"mode"`
	Counters map[string]FirewallCounter `json:"counters"`
	Recent   []FirewallHit              `json:"recent"`
}

// firewall reads the rules file's mode, the table's counters (`nft -j list counters`) and the last day's logged packets (`journalctl -k`).
// All of it is passive; a machine without nft, or without the table, reports it not loaded and off, and the check-in goes ahead.
func (c Collector) firewall(ctx context.Context) FirewallFacts {
	facts := FirewallFacts{Mode: "off", Counters: map[string]FirewallCounter{}, Recent: []FirewallHit{}}
	for _, name := range FirewallCounters {
		facts.Counters[name] = FirewallCounter{}
	}
	if c.Run == nil {
		return facts
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.Run(ctx, "nft", "-j", "list", "counters", "table", "inet", FirewallTable)
	if err != nil {
		return facts
	}
	facts.Loaded = true
	for name, v := range ParseFirewallCounters(out) {
		facts.Counters[name] = v
	}
	if c.ReadFirewallRules != nil {
		if text, err := c.ReadFirewallRules(); err == nil {
			facts.Mode = FirewallMode(text)
		}
	}
	if facts.Mode == "off" {
		return facts
	}
	// journalctl exits non-zero when nothing matched; what it printed is read either way
	log, _ := c.Run(ctx, "journalctl", "-k", "--since=-24h", "-g", "^nc-", "-o", "cat", "--no-pager", "-q", "-n", strconv.Itoa(maxFirewallLogLines))
	facts.Recent = ParseFirewallLog(log)
	return facts
}

// FirewallMode reads the pod rules' mode from the rules file's header (its first two lines, where fw_render writes it): observe or
// enforce, anything else (no pod rules) off.
func FirewallMode(rules string) string {
	for i, line := range strings.SplitN(rules, "\n", 3) {
		if i > 1 {
			break
		}
		switch strings.TrimSpace(line) {
		case "# pod-mode: observe":
			return "observe"
		case "# pod-mode: enforce":
			return "enforce"
		}
	}
	return "off"
}

// ParseFirewallCounters reads `nft -j list counters table inet nuit_cloud`, keeping only the pod rules' counters of that table.
func ParseFirewallCounters(out string) map[string]FirewallCounter {
	var parsed struct {
		Nftables []struct {
			Counter *struct {
				Family  string `json:"family"`
				Table   string `json:"table"`
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
				Bytes   uint64 `json:"bytes"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	got := map[string]FirewallCounter{}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return got
	}
	for _, e := range parsed.Nftables {
		if e.Counter == nil || e.Counter.Family != "inet" || e.Counter.Table != FirewallTable || !knownCounter(e.Counter.Name) {
			continue
		}
		got[e.Counter.Name] = FirewallCounter{Packets: e.Counter.Packets, Bytes: e.Counter.Bytes}
	}
	return got
}

func knownCounter(name string) bool {
	for _, n := range FirewallCounters {
		if n == name {
			return true
		}
	}
	return false
}

// ParseFirewallLog reads kernel log lines the pod rules wrote (`nc-pod_private IN=cni0 ... SRC=… DST=10.10.10.140 ... PROTO=TCP SPT=… DPT=5432`)
// and returns the most frequent destinations, at most 20. A line from another prefix, or whose destination is not an IPv4 address, is skipped.
func ParseFirewallLog(log string) []FirewallHit {
	counts := map[FirewallHit]int{}
	for _, line := range strings.Split(log, "\n") {
		line = strings.TrimSpace(line)
		rule, rest, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(rule, "nc-") || !knownCounter(rule[3:]) {
			continue
		}
		hit := FirewallHit{Rule: rule[3:], Proto: "other"}
		for _, f := range strings.Fields(rest) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "DST":
				hit.Dst = v
			case "DPT":
				if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 65535 {
					hit.Port = n
				}
			case "PROTO":
				switch v {
				case "TCP":
					hit.Proto = "tcp"
				case "UDP":
					hit.Proto = "udp"
				}
			}
		}
		if a, err := netip.ParseAddr(hit.Dst); err != nil || !a.Is4() || a.String() != hit.Dst {
			continue
		}
		if hit.Proto == "other" {
			hit.Port = 0
		}
		counts[hit]++
	}
	out := make([]FirewallHit, 0, len(counts))
	for h, n := range counts {
		h.Count = n
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Proto < b.Proto
	})
	if len(out) > maxFirewallRecent {
		out = out[:maxFirewallRecent]
	}
	return out
}

// version is a digest of everything reported: a firewall whose counters do not move sends nothing new.
func (f FirewallFacts) version() string {
	h := sha256.New()
	fmt.Fprintf(h, "%t|%s", f.Loaded, f.Mode)
	for _, name := range FirewallCounters {
		v := f.Counters[name]
		fmt.Fprintf(h, "|%s=%d/%d", name, v.Packets, v.Bytes)
	}
	for _, r := range f.Recent {
		fmt.Fprintf(h, "|%s,%s,%d,%s,%d", r.Rule, r.Dst, r.Port, r.Proto, r.Count)
	}
	return "f" + hex.EncodeToString(h.Sum(nil))[:12]
}
