package survey

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const shadowHash = "$6$Xw3nQm1pZr8tLs0a$lDwWrLKgJ072NNOWKl9n2RvpvOFl8zIGSPVAwF3AReFCzfxb.JQ8uGCDjZpOiKJ4Bu62Dij1HRReSDKFnXMDE0"

func rootProblems(r Report) []string {
	var out []string
	for _, p := range r.Problems {
		if strings.HasPrefix(p, "root-password:") {
			out = append(out, p)
		}
	}
	return out
}

func TestTheSurveySaysWhetherRootHasAPasswordAndNeverWhatItIs(t *testing.T) {
	root := t.TempDir()
	shadow := filepath.Join(root, "/etc/shadow")
	_ = os.MkdirAll(filepath.Dir(shadow), 0o755)
	for field, want := range map[string]string{
		"":                 "none",
		"!":                "locked",
		"*":                "locked",
		"!*":               "locked",
		"!" + shadowHash:   "locked", // a locked password keeps its hash behind the !
		shadowHash:         "set",
		"$y$j9T$salt$hash": "set",
	} {
		text := "daemon:*:20000:0:99999:7:::\nroot:" + field + ":20000:0:99999:7:::\nnuitcloud:" + shadowHash + ":20000::::::\n"
		if err := os.WriteFile(shadow, []byte(text), 0o640); err != nil {
			t.Fatal(err)
		}
		r := Collect(context.Background(), authSources(root))
		if r.RootPassword == nil || *r.RootPassword != want || len(rootProblems(r)) != 0 {
			t.Errorf("%q: %v %v", field, r.RootPassword, r.Problems)
			continue
		}
		raw, _ := json.Marshal(r)
		if !strings.Contains(string(raw), `"rootPassword":"`+want+`"`) {
			t.Errorf("the report's shape: %s", raw)
		}
		if strings.Contains(string(raw), "Xw3nQm1pZr8tLs0a") || strings.Contains(string(raw), "j9T$salt") {
			t.Errorf("a hash reached the report: %s", raw)
		}
	}

	// no line for root: unknown, and said so
	_ = os.WriteFile(shadow, []byte("daemon:*:20000:0:99999:7:::\n"), 0o640)
	r := Collect(context.Background(), authSources(root))
	if p := rootProblems(r); r.RootPassword != nil || len(p) != 1 || !strings.Contains(p[0], "no line for root") {
		t.Errorf("no root: %v %v", r.RootPassword, r.Problems)
	}

	// a file that cannot be read (a directory in its place stands in for a permission error): null, and said so
	_ = os.Remove(shadow)
	_ = os.MkdirAll(shadow, 0o700)
	r = Collect(context.Background(), authSources(root))
	if r.RootPassword != nil || len(rootProblems(r)) != 1 {
		t.Errorf("unreadable: %v %v", r.RootPassword, r.Problems)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"rootPassword":null`) {
		t.Errorf("an unreadable file must be null: %s", raw)
	}

	// no file at all is unknown too, not "none"
	_ = os.RemoveAll(shadow)
	r = Collect(context.Background(), authSources(root))
	if r.RootPassword != nil || len(rootProblems(r)) != 1 {
		t.Errorf("missing: %v %v", r.RootPassword, r.Problems)
	}
}
