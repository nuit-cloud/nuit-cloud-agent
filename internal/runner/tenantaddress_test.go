package runner

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/tenantaddress"
)

const vlanFile = `auto eth0.5
iface eth0.5 inet static
    address 10.20.0.2/24
    post-up ip addr add 10.20.0.3/24 dev eth0.5 2>/dev/null || true
    pre-down ip route flush table 105 2>/dev/null || true
`

const ipShow = `[{"ifindex":1,"ifname":"lo","address":"00:00:00:00:00:00","addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8}]},` +
	`{"ifindex":3,"ifname":"eth0.5","link":"eth0","address":"aa:bb:cc:dd:ee:01","addr_info":[{"family":"inet","local":"10.20.0.2","prefixlen":24},{"family":"inet","local":"10.20.0.3","prefixlen":24,"secondary":true}]}]`

// recordingIP answers `ip -j addr show` with ipShow and records every other command.
type recordingIP struct{ changes []string }

func (r *recordingIP) run(_ context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "-j" {
		return ipShow, nil
	}
	r.changes = append(r.changes, strings.Join(args, " "))
	return "", nil
}

// silentARP hears nobody and counts what it sends.
type silentARP struct{ probes, announcements int }

func (s *silentARP) Probe(context.Context, tenantaddress.Link, netip.Addr, time.Duration) (tenantaddress.Claim, error) {
	s.probes++
	return tenantaddress.Claim{}, nil
}

func (s *silentARP) Announce(context.Context, tenantaddress.Link, netip.Addr) error {
	s.announcements++
	return nil
}

func tenantAddressRunner(t *testing.T) (Runner, *recordingIP, *silentARP, string) {
	t.Helper()
	root := t.TempDir()
	file := tenantaddress.File(root, 5)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(vlanFile), 0o644); err != nil {
		t.Fatal(err)
	}
	ip, arp := &recordingIP{}, &silentARP{}
	r := newRunner()
	r.TenantAddresses = tenantaddress.Mover{Root: root, IP: ip.run, ARP: arp, Sleep: func(context.Context, time.Duration) error { return nil }}
	return r, ip, arp, file
}

func addrTask(params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: "tenant-address", TimeoutSeconds: 120, Params: params}
}

func TestTenantAddressIsInTheCatalog(t *testing.T) {
	for _, n := range newRunner().Names() {
		if n == "tenant-address" {
			return
		}
	}
	t.Error("tenant-address is not in the catalog")
}

func TestAHostileTenantAddressParameterChangesNothing(t *testing.T) {
	r, ip, arp, file := tenantAddressRunner(t)
	good := func() map[string]any {
		return map[string]any{"action": "take", "address": "10.20.0.7", "prefix": float64(24), "vlanId": float64(5)}
	}
	hostile := []map[string]any{}
	for k, v := range map[string]any{
		"address": "10.20.0.7 dev eth0; reboot", "prefix": "24 && reboot", "vlanId": "5/../../../etc/passwd", "action": "take; reboot",
	} {
		p := good()
		p[k] = v
		hostile = append(hostile, p)
	}
	extra := good()
	extra["interface"] = "eth0"
	public := good()
	public["address"] = "1.1.1.1"
	hostile = append(hostile, extra, public, map[string]any{})
	for _, p := range hostile {
		if rep := r.Run(context.Background(), addrTask(p)); rep.Status != "failed" {
			t.Errorf("%v ran: %+v", p, rep)
		}
	}
	if len(ip.changes) != 0 || arp.probes != 0 || arp.announcements != 0 {
		t.Errorf("ip %v, %d probes, %d announcements", ip.changes, arp.probes, arp.announcements)
	}
	if b, _ := os.ReadFile(file); string(b) != vlanFile {
		t.Error("the VLAN file was changed")
	}
}

func TestTenantAddressReportsWhatItDid(t *testing.T) {
	r, ip, arp, file := tenantAddressRunner(t)
	rep := r.Run(context.Background(), addrTask(map[string]any{"action": "take", "address": "10.20.0.7", "prefix": float64(24), "vlanId": float64(5)}))
	res, ok := rep.Result.(tenantaddress.TakeResult)
	if rep.Status != "done" || !ok || res != (tenantaddress.TakeResult{Action: "take", Address: "10.20.0.7", FileChanged: true, Announcements: 3}) {
		t.Fatalf("%+v", rep)
	}
	if rep.Output != "took 10.20.0.7; added to the VLAN configuration; announced 3 times" {
		t.Errorf("%q", rep.Output)
	}
	if len(ip.changes) != 1 || arp.probes != 3 || arp.announcements != 3 {
		t.Errorf("ip %v, %d probes, %d announcements", ip.changes, arp.probes, arp.announcements)
	}
	rep = r.Run(context.Background(), addrTask(map[string]any{"action": "release", "address": "10.20.0.3", "prefix": float64(24), "vlanId": float64(5)}))
	rel, ok := rep.Result.(tenantaddress.ReleaseResult)
	if rep.Status != "done" || !ok || rel != (tenantaddress.ReleaseResult{Action: "release", Address: "10.20.0.3", WasPresent: true, FileChanged: true}) {
		t.Fatalf("%+v", rep)
	}
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "10.20.0.3/") || !strings.Contains(string(b), "10.20.0.7/24") {
		t.Errorf("the file is\n%s", b)
	}
	rep = r.Run(context.Background(), addrTask(map[string]any{"action": "release", "address": "10.20.0.2", "prefix": float64(24), "vlanId": float64(5)}))
	if rep.Status != "failed" || !strings.Contains(rep.Output, "own address") {
		t.Errorf("%+v", rep)
	}
}
