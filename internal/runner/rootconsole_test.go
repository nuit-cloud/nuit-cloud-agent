package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/rootconsole"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

const consoleHash = "$y$j9T$uZLQoZM5bHxvkYTzn31GU1$sbYUCQiGjE2Z4m9jioAFrkmM3vytg9onLM81u8t4xo4"

// consoleRunner has a scratch /etc/shadow and a stand-in for chpasswd and usermod that records every call and writes the file as they would.
func consoleRunner(t *testing.T) (Runner, *[]string, string) {
	t.Helper()
	root := t.TempDir()
	shadow := rootconsole.ShadowPath(root)
	if err := os.MkdirAll(filepath.Dir(shadow), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shadow, []byte("root:!:20000:0:99999:7:::\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var calls []string
	exec := func(_ context.Context, stdin []byte, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " ")+" <"+string(stdin))
		field := "!"
		switch name {
		case "chpasswd":
			field = strings.TrimSuffix(strings.TrimPrefix(string(stdin), "root:"), "\n")
		case "usermod":
			field = "!" + consoleHash
		}
		return "", os.WriteFile(shadow, []byte("root:"+field+":20000:0:99999:7:::\n"), 0o640)
	}
	r := newRunner()
	r.RootConsole = rootconsole.Console{Root: root, Exec: exec}
	return r, &calls, shadow
}

func consoleTask(params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: "root-console", TimeoutSeconds: 30, Params: params}
}

func TestRootConsoleSetsAndLocksAndNeverReportsTheHash(t *testing.T) {
	r, calls, _ := consoleRunner(t)
	rep := r.Run(context.Background(), consoleTask(map[string]any{"action": "set", "hash": consoleHash}))
	if rep.Status != "done" || *rep.ExitCode != 0 || rep.Output != "root's console password is set (yescrypt)" {
		t.Errorf("%+v", rep)
	}
	if len(*calls) != 1 || (*calls)[0] != "chpasswd -e <root:"+consoleHash+"\n" {
		t.Errorf("calls %q", *calls)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "uZLQoZM5bHxvkYTzn31GU1") || strings.Contains(string(raw), "j9T") {
		t.Errorf("the hash reached the report: %s", raw)
	}

	rep = r.Run(context.Background(), consoleTask(map[string]any{"action": "lock"}))
	if rep.Status != "done" || rep.Output != "root's console password is locked" {
		t.Errorf("%+v", rep)
	}
	if len(*calls) != 2 || (*calls)[1] != "usermod -L root <" {
		t.Errorf("calls %q", *calls)
	}
	raw, _ = json.Marshal(rep)
	if strings.Contains(string(raw), "uZLQoZM5bHxvkYTzn31GU1") {
		t.Errorf("the hash reached the report: %s", raw)
	}
}

func TestAHostileRootConsoleTaskChangesNothing(t *testing.T) {
	r, calls, shadow := consoleRunner(t)
	for _, p := range []map[string]any{
		{"action": "set", "hash": "correct horse battery staple"},
		{"action": "set", "hash": consoleHash + "\nnuitcloud:" + consoleHash},
		{"action": "set", "hash": "$1$abc$OGyl6dDvZCDiGmIVbeuCq/"},
		{"action": "set", "hash": "$5$abc$sCOGUciM.GdmYMCUvmUcgu03oRH87O0clF96.9mwWGB"},
		{"action": "set", "hash": ""},
		{"action": "set", "hash": consoleHash, "user": "nuitcloud"},
		{"action": "lock", "user": "nuitcloud"},
		{"action": "unlock"},
		{},
	} {
		rep := r.Run(context.Background(), consoleTask(p))
		if rep.Status != "failed" {
			t.Errorf("%q was accepted: %+v", p, rep)
		}
		if strings.Contains(rep.Output, "horse") || strings.Contains(rep.Output, "uZLQoZM5") || strings.Contains(rep.Output, "OGyl6d") {
			t.Errorf("the refusal repeats the value: %s", rep.Output)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("the machine was touched: %q", *calls)
	}
	if raw, _ := os.ReadFile(shadow); string(raw) != "root:!:20000:0:99999:7:::\n" {
		t.Errorf("shadow changed: %q", raw)
	}
	found := false
	for _, n := range r.Names() {
		found = found || n == "root-console"
	}
	if !found {
		t.Error("root-console is not in the catalog")
	}
}
