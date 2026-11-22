package tenantaddress

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderByFeature is the VLAN file the real ncs_vlan feature (internal/profiles/bootstrap/features.sh, vlan_render) writes for node
// 10.20.0.2/24 on enp0s31f6.5 with the given tenant addresses, keeping the tenant lines of existing (when not empty), as its apply does.
func renderByFeature(t *testing.T, tenants, existing string) string {
	t.Helper()
	return renderByFeatureOn(t, "enp0s31f6", tenants, existing)
}

// renderByFeatureOn is renderByFeature with the VLAN on another parent interface, and env overriding the slice.
func renderByFeatureOn(t *testing.T, parent, tenants, existing string, env ...string) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the feature with")
	}
	features, err := filepath.Abs(filepath.Join("..", "profiles", "bootstrap", "features.sh"))
	if err != nil {
		t.Fatal(err)
	}
	prev := ""
	if existing != "" {
		prev = filepath.Join(t.TempDir(), "previous")
		if err := os.WriteFile(prev, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(sh, "-eu", "-c", `. "$1"; vlan_render "$3" "$2"`, "render", features, prev, parent)
	cmd.Env = append(os.Environ(), "NC_USER=nuit", "NCS_VLAN_ID=5", "NCS_VLAN_PREFIX=24", "NCS_VLAN_ADDRESS=10.20.0.2", "NCS_VLAN_GATEWAY=10.20.0.1",
		"NCS_VLAN_TENANT_ADDRESSES="+tenants)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the feature did not render: %v", err)
	}
	return string(out)
}

func tenantLines(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		if strings.Contains(l, "ip addr add ") {
			out = append(out, l)
		}
	}
	return out
}

// The file the feature renders today, with its tenant list given, is the one this package was written against.
func TestTheFeatureStillRendersTheFileTakeAndReleaseWereWrittenFor(t *testing.T) {
	if got := renderByFeature(t, "10.20.0.3 10.20.0.30 10.20.0.4", ""); got != agentFile {
		t.Errorf("the feature renders\n%s\nwant\n%s", got, agentFile)
	}
}

// Tenant addresses are leases the platform moves with tenant-address (nuit-cloud/nuit-cloud#189); the VLAN profile may be given none.
// Take, the feature again, and Release then each keep the others' work.
func TestTakeAndReleaseWorkOnAFileTheFeatureRenderedWithNoTenantAddresses(t *testing.T) {
	content := renderByFeature(t, "", "")
	if len(tenantLines(content)) != 0 {
		t.Fatalf("an empty list rendered tenant lines:\n%s", content)
	}
	root, file := scratch(t, content)
	m := newMachine()
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.3")); err != nil {
		t.Fatal(err)
	}
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.7")); err != nil {
		t.Fatal(err)
	}
	line7 := "    post-up ip addr add 10.20.0.7/24 dev enp0s31f6.5 2>/dev/null || true\n"
	if want := strings.Replace(content, "    address 10.20.0.2/24\n", "    address 10.20.0.2/24\n"+line3+line7, 1); read(t, file) != want {
		t.Fatalf("after two takes the file is\n%s", read(t, file))
	}

	// the profile applied again, given one address of its own: the task's lines stay, the given one is added after them
	again := renderByFeature(t, "10.20.0.4", read(t, file))
	want := []string{strings.TrimSuffix(line3, "\n"), strings.TrimSuffix(line7, "\n"), "    post-up ip addr add 10.20.0.4/24 dev enp0s31f6.5 2>/dev/null || true"}
	if got := tenantLines(again); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the feature rendered the tenant lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// given an address already there, it is not written twice; given none, nothing is dropped
	if got := tenantLines(renderByFeature(t, "10.20.0.7", again)); len(got) != 3 {
		t.Errorf("a given address already in the file was written again: %v", got)
	}
	if got := tenantLines(renderByFeature(t, "", again)); len(got) != 3 {
		t.Errorf("an empty list dropped tenant lines: %v", got)
	}
	if err := os.WriteFile(file, []byte(again), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.7")); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(again, line7, "", 1); read(t, file) != want {
		t.Errorf("after the release the file is\n%s", read(t, file))
	}
}

// A hand-made file (scripts/server-vlan5.sh) the feature rewrites keeps every tenant line, and Take and Release find them after.
func TestAFileTheFeatureRewroteFromAHandMadeOneKeepsItsTenantAddresses(t *testing.T) {
	content := renderByFeature(t, "10.20.0.5", handFile)
	want := []string{
		"    post-up ip addr add 10.20.0.3/24 dev enp0s31f6.5 2>/dev/null || true",
		"    post-up ip addr add 10.20.0.30/24 dev enp0s31f6.5 2>/dev/null || true",
		"    post-up ip addr add 10.20.0.4/24 dev enp0s31f6.5 2>/dev/null || true",
		"    post-up ip addr add 10.20.0.5/24 dev enp0s31f6.5 2>/dev/null || true",
	}
	if got := tenantLines(content); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the tenant lines are\n%s", strings.Join(got, "\n"))
	}
	root, file := scratch(t, content)
	m := newMachine("10.20.0.3", "10.20.0.30", "10.20.0.4", "10.20.0.5")
	if _, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.30")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, file), "10.20.0.30/") || !strings.Contains(read(t, file), "ip addr add 10.20.0.3/24") {
		t.Errorf("the release took the wrong line:\n%s", read(t, file))
	}
}

// On a parent whose name leaves no room for <parent>.5 (a Raspberry Pi's enx<mac>, 15 characters; nuit-cloud/nuit-cloud#197) the feature
// writes vlan5, tied to its parent by vlan-raw-device, and Take and Release work on that file and that interface.
func TestTakeAndReleaseWorkOnTheVLAN5FileTheFeatureWritesForAFifteenCharacterParent(t *testing.T) {
	const parent = "enxb827ebdbcb96"
	content := renderByFeatureOn(t, parent, "10.20.0.3", "")
	for _, want := range []string{"auto vlan5\n", "iface vlan5 inet static\n", "    vlan-raw-device " + parent + "\n",
		"ip link add link " + parent + " name vlan5 type vlan id 5", "net.ipv4.conf.vlan5.rp_filter=2", TenantLine(netip.MustParseAddr("10.20.0.3"), 24, "vlan5")} {
		if !strings.Contains(content, want) {
			t.Fatalf("the feature rendered no %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, parent+".5") {
		t.Fatalf("the feature rendered an interface name longer than 15 characters:\n%s", content)
	}
	root, file := scratch(t, content)
	m := newMachine("10.20.0.3")
	m.ifaces[1].Name, m.ifaces[3].Name = parent, "vlan5"
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.7")); err != nil {
		t.Fatal(err)
	}
	if got := m.changes(); len(got) != 1 || got[0] != "addr add 10.20.0.7/24 dev vlan5" || !strings.Contains(read(t, file), TenantLine(netip.MustParseAddr("10.20.0.7"), 24, "vlan5")) {
		t.Fatalf("ran %v; the file is\n%s", got, read(t, file))
	}
	if _, err := mover(root, m, &fakeARP{}).Release(context.Background(), params("release", "10.20.0.3")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, file), "10.20.0.3/") || m.on("vlan5", "10.20.0.3") {
		t.Errorf("after the release the file is\n%s", read(t, file))
	}
}

// The tenant VLAN's network grows from /24 to /16 and the node gets an address of its own in 10.20.255.0/24 (nuit-cloud/nuit-cloud#197): the
// feature rewrites the stanza with the new address and prefix and gives every tenant line kept the new prefix, the rest of each line as it
// was. That is the file a take with prefix 16 then works on, and a take with the old prefix is refused.
func TestTheFeatureMovesTheFileTo16AndTakeThenUses16(t *testing.T) {
	got := renderByFeatureOn(t, "enp0s31f6", "10.20.0.3 10.20.0.30 10.20.0.4", agentFile, "NCS_VLAN_PREFIX=16", "NCS_VLAN_ADDRESS=10.20.255.2")
	if got != agentFile16 {
		t.Fatalf("the feature renders\n%s\nwant\n%s", got, agentFile16)
	}
	root, file := scratch(t, got)
	m := newMachine16("10.20.0.3", "10.20.0.30", "10.20.0.4")
	p := params("take", "10.20.0.7")
	p.Prefix = 16
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, file), "    post-up ip addr add 10.20.0.7/16 dev enp0s31f6.5 2>/dev/null || true\n") || !m.onWith("enp0s31f6.5", "10.20.0.7", 16) {
		t.Errorf("not taken with /16: %v\n%s", m.changes(), read(t, file))
	}
	before := read(t, file)
	m.calls = nil
	if _, err := mover(root, m, &fakeARP{}).Take(context.Background(), params("take", "10.20.0.8")); err == nil || !strings.Contains(err.Error(), "/24") {
		t.Errorf("a take with the old prefix: %v", err)
	}
	if read(t, file) != before || len(m.changes()) != 0 {
		t.Error("a refused take changed something")
	}
}
