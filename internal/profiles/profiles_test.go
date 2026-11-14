package profiles

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// Output as bootstrap.sh printed it on a real Debian 13 image, and as the sandbox tests show it.
const checkOutput = `DRIFT   user
ok      updates
DRIFT   ncs_packages
ok      ncs_wired
DRIFT   ncs_ssh

`

const applyOutput = `ok      user
applying journal ...
done    journal
applying ncs_packages ...
FAILED  ncs_packages
ok      ncs_wired
applying ncs_ssh ...
  set PermitRootLogin no and PasswordAuthentication no in /etc/ssh/sshd_config.d/, AFTER you have shown that key login works
MANUAL  ncs_ssh
applying kvm ...
  no /dev/kvm: turn on Intel VT-x / AMD-V in the BIOS/UEFI
MANUAL  kvm

A reboot is needed for the changes above to take effect.
Clock synchronised: yes
`

func TestParseReadsACheck(t *testing.T) {
	r := Parse(checkOutput)
	want := []Feature{{Name: "user", Status: "drift"}, {Name: "updates", Status: "ok"}, {Name: "ncs_packages", Status: "drift"}, {Name: "ncs_wired", Status: "ok"}, {Name: "ncs_ssh", Status: "drift"}}
	if !reflect.DeepEqual(r.Features, want) || r.RebootNeeded {
		t.Errorf("%+v", r)
	}
}

func TestParseReadsAnApplyWithItsNotesAndTheRebootFlag(t *testing.T) {
	r := Parse(applyOutput)
	byName := map[string]Feature{}
	for _, f := range r.Features {
		byName[f.Name] = f
	}
	if byName["journal"].Status != "applied" || byName["ncs_packages"].Status != "failed" || byName["user"].Status != "ok" {
		t.Errorf("%+v", r.Features)
	}
	ssh := byName["ncs_ssh"]
	if ssh.Status != "manual" || len(ssh.Notes) != 1 || !strings.Contains(ssh.Notes[0], "PermitRootLogin") {
		t.Errorf("a feature's note must be attached to it: %+v", ssh)
	}
	kvm := byName["kvm"]
	if len(kvm.Notes) != 1 || !strings.Contains(kvm.Notes[0], "BIOS") {
		t.Errorf("notes must not leak between features: %+v", kvm)
	}
	if !r.RebootNeeded {
		t.Error("the reboot line was not seen")
	}
	if len(r.Features) != 6 {
		t.Errorf("only status lines are features: %d", len(r.Features))
	}
}

func TestParseIgnoresEverythingElse(t *testing.T) {
	r := Parse("Clock synchronised: yes\nok      a b\nsh: 1: systemctl: not found\nUNKNOWN  x\n\n")
	if len(r.Features) != 0 {
		t.Errorf("%+v", r.Features)
	}
	if r.Features == nil {
		t.Error("the list must be an empty list, not null, in the JSON")
	}
}

func fakeScripts() fstest.MapFS {
	return fstest.MapFS{
		"bootstrap.sh":                  {Data: []byte("#!/bin/sh\n")},
		"features.sh":                   {Data: []byte("# features\n")},
		"profiles/nuit-cloud-server.sh": {Data: []byte("PROFILE_FEATURES=user\n")},
		"profiles/other.sh":             {Data: []byte("PROFILE_FEATURES=user\n")},
		"profiles/README":               {Data: []byte("not a profile\n")},
	}
}

func TestNamesListsTheProfilesCarried(t *testing.T) {
	if got := (Runner{Scripts: fakeScripts()}).Names(); !reflect.DeepEqual(got, []string{"nuit-cloud-server", "other"}) {
		t.Errorf("%v", got)
	}
}

func TestTheRealEmbeddedProfilesAreTheOnesThePlatformAsksFor(t *testing.T) {
	got := Runner{}.Names()
	if !reflect.DeepEqual(got, []string{"nuit-cloud-address", "nuit-cloud-control-plane", "nuit-cloud-firewall", "nuit-cloud-ingress", "nuit-cloud-login", "nuit-cloud-resolvers", "nuit-cloud-server", "nuit-cloud-ssh", "nuit-cloud-vlan", "nuit-cloud-wired", "nuit-container-service", "nuit-container-service-record"}) {
		t.Errorf("embedded profiles: %v (internal/profiles/bootstrap/profiles holds exactly the profiles the platform asks for)", got)
	}
}

// echoing stands in for the shell: it prints a canned output and exits with the given code, so no real system is touched.
func echoing(output string, code int, sawArgs *[]string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*sawArgs = append([]string{name}, args...)
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' \"$1\"; exit "+string(rune('0'+code)), "sh", output)
	}
}

func TestACheckNeedsNoRootRunsTheShellWithTheCheckFlagAndReportsDriftWithoutFailing(t *testing.T) {
	var args []string
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 1000 }, Command: echoing(checkOutput, 1, &args)}
	res, exit, err := r.Run(context.Background(), "nuit-cloud-server", false)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 1 || res.Clean || res.Mode != "check" || res.Profile != "nuit-cloud-server" || len(res.Features) != 5 {
		t.Errorf("exit=%d %+v", exit, res)
	}
	if args[0] != "sh" || args[2] != "nuit-cloud-server" || args[3] != "--check" || !strings.HasSuffix(args[1], "bootstrap.sh") {
		t.Errorf("command: %v", args)
	}
	if res.Output != checkOutput {
		t.Error("the raw output must be kept")
	}
}

func TestApplyingNeedsRootAndSaysSo(t *testing.T) {
	var args []string
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 1000 }, Command: echoing("", 0, &args)}
	_, _, err := r.Run(context.Background(), "nuit-cloud-server", true)
	if !errors.Is(err, ErrNeedsRoot) {
		t.Errorf("got %v", err)
	}
	if args != nil {
		t.Error("nothing may run when it cannot be applied")
	}
}

func TestAnApplyAsRootRunsWithoutTheCheckFlagAndAGoodRunIsClean(t *testing.T) {
	var args []string
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 0 }, Command: echoing("ok      user\n", 0, &args)}
	res, exit, err := r.Run(context.Background(), "nuit-cloud-server", true)
	if err != nil || exit != 0 || !res.Clean || res.Mode != "apply" {
		t.Fatalf("%v %d %+v", err, exit, res)
	}
	for _, a := range args {
		if a == "--check" {
			t.Errorf("an apply must not pass --check: %v", args)
		}
	}
}

func TestAProfileTheAgentDoesNotCarryIsRefusedBeforeAnythingRuns(t *testing.T) {
	var args []string
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 0 }, Command: echoing("", 0, &args)}
	for _, bad := range []string{"", "nope", "../etc/passwd", "nuit-cloud-server; rm -rf /", "nuit-cloud-server --with docker"} {
		if _, _, err := r.Run(context.Background(), bad, true); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if args != nil {
		t.Error("nothing may run for an unknown profile")
	}
}

func TestTheScriptsAreExtractedPrivatelyAndRemoved(t *testing.T) {
	var seen string
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 0 }, Command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
		seen = args[0]
		return exec.CommandContext(ctx, "sh", "-c", "test -f \"$1\" && test -f \"$(dirname \"$1\")/features.sh\" && test -f \"$(dirname \"$1\")/profiles/nuit-cloud-server.sh\"", "sh", args[0])
	}}
	if _, exit, err := r.Run(context.Background(), "nuit-cloud-server", true); err != nil || exit != 0 {
		t.Fatalf("the scripts were not all on disk when it ran: exit=%d err=%v", exit, err)
	}
	if out, _ := exec.Command("ls", seen).CombinedOutput(); !strings.Contains(string(out), "No such file") {
		t.Error("the temporary copy must be removed afterwards")
	}
}

func TestTruncateKeepsTheHeadAndTailAndSaysItCut(t *testing.T) {
	if Truncate("short", 100) != "short" {
		t.Error("short text must be untouched")
	}
	long := "HEAD" + strings.Repeat("x", 100000) + "TAIL"
	cut := Truncate(long, 1000)
	if len(cut) > 1000 || !strings.HasPrefix(cut, "HEAD") || !strings.HasSuffix(cut, "TAIL") || !strings.Contains(cut, "output cut") {
		t.Errorf("len=%d", len(cut))
	}
}

func TestARunKeepsAtMostMaxRawOutputOfTheShellsOutput(t *testing.T) {
	var args []string
	big := strings.Repeat("noise\n", 20000) + "ok      user\n"
	r := Runner{Scripts: fakeScripts(), Euid: func() int { return 0 }, Command: echoing(big, 0, &args)}
	res, _, err := r.Run(context.Background(), "nuit-cloud-server", true)
	if err != nil || len(res.Output) > MaxRawOutput || len(res.Features) != 1 {
		t.Errorf("err=%v len=%d features=%d", err, len(res.Output), len(res.Features))
	}
}
