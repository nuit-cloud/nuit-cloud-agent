package tenantaddress

import (
	"context"
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
	cmd := exec.Command(sh, "-eu", "-c", `. "$1"; vlan_render enp0s31f6 "$2"`, "render", features, prev)
	cmd.Env = append(os.Environ(), "NC_USER=nuit", "NCS_VLAN_ID=5", "NCS_VLAN_PREFIX=24", "NCS_VLAN_ADDRESS=10.20.0.2", "NCS_VLAN_GATEWAY=10.20.0.1",
		"NCS_VLAN_TENANT_ADDRESSES="+tenants)
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
