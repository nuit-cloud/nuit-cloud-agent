package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/volume"
)

const (
	volTenant   = "t-6f1c2a3b-4d5e-4f60-8a71-92b3c4d5e6f7"
	volName     = volTenant + "/data.img"
	volTransfer = "0b9a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
)

// countingPlatform counts the platform calls a task makes (none, for a task whose params do not pass).
type countingPlatform struct{ calls int }

func (c *countingPlatform) PostJSON(context.Context, string, any, int64, any) error {
	c.calls++
	return nil
}

func volTask(name string, params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: name, TimeoutSeconds: 60, Params: params}
}

func TestTheVolumeTasksAreInTheCatalog(t *testing.T) {
	found := map[string]bool{}
	for _, n := range newRunner().Names() {
		found[n] = true
	}
	for _, n := range []string{"volume-export", "volume-import", "volume-remove"} {
		if !found[n] {
			t.Errorf("%s is not in the catalog", n)
		}
	}
}

func TestAHostileVolumeParameterChangesNothing(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.img")
	if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	plat := &countingPlatform{}
	r := newRunner()
	r.Volumes = volume.Mover{Root: filepath.Join(root, "volumes"), Platform: plat}
	sum := strings.Repeat("a", 64)
	hostile := []*taskverify.Task{
		volTask("volume-export", map[string]any{"transferId": volTransfer, "volume": "../outside.img"}),
		volTask("volume-export", map[string]any{"transferId": volTransfer, "volume": volName, "path": "/etc/shadow"}),
		volTask("volume-export", map[string]any{"transferId": "$(reboot)", "volume": volName}),
		volTask("volume-import", map[string]any{"transferId": volTransfer, "volume": volTenant + "/../../outside.img", "size": float64(7), "sha256": sum, "chunks": float64(1)}),
		volTask("volume-import", map[string]any{"transferId": volTransfer, "volume": volName, "size": float64(7), "sha256": sum, "chunks": float64(1), "overwrite": true}),
		volTask("volume-import", map[string]any{"transferId": volTransfer, "volume": volName, "size": float64(2 << 40), "sha256": sum, "chunks": float64(1)}),
		volTask("volume-remove", map[string]any{"volume": "/" + filepath.Join(root, "outside.img")}),
		volTask("volume-remove", map[string]any{"volume": volName, "force": true}),
		volTask("volume-remove", map[string]any{"volume": volName, "sha256": "not-a-hash"}),
	}
	for _, task := range hostile {
		if rep := r.Run(context.Background(), task); rep.Status != "failed" {
			t.Errorf("%s %v ran: %+v", task.Name, task.Params, rep)
		}
	}
	if plat.calls != 0 {
		t.Errorf("the platform was called %d times for tasks that should never have started", plat.calls)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep me" {
		t.Error("the file outside the volumes root was touched")
	}
	if _, err := os.Stat(filepath.Join(root, "volumes")); !os.IsNotExist(err) {
		t.Error("the volumes root was created for a task that should never have started")
	}
}

func TestVolumeRemoveReportsWhatItDidAndAMissingVolumeIsDone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, volTenant)
	_ = os.MkdirAll(dir, 0o700)
	content := []byte("an image")
	if err := os.WriteFile(filepath.Join(dir, "data.img"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	r := newRunner()
	r.Volumes = volume.Mover{Root: root}
	rep := r.Run(context.Background(), volTask("volume-remove", map[string]any{"volume": volName, "sha256": strings.Repeat("0", 64)}))
	if rep.Status != "failed" || !strings.Contains(rep.Output, "not the copy the platform expects") {
		t.Errorf("%+v", rep)
	}
	rep = r.Run(context.Background(), volTask("volume-remove", map[string]any{"volume": volName, "sha256": hex.EncodeToString(sum[:])}))
	if res, ok := rep.Result.(volume.RemoveResult); rep.Status != "done" || !ok || !res.Removed {
		t.Errorf("%+v", rep)
	}
	rep = r.Run(context.Background(), volTask("volume-remove", map[string]any{"volume": volName}))
	if res, ok := rep.Result.(volume.RemoveResult); rep.Status != "done" || !ok || res.Removed {
		t.Errorf("%+v", rep)
	}
	rep = r.Run(context.Background(), volTask("volume-export", map[string]any{"transferId": volTransfer, "volume": volName}))
	if rep.Status != "failed" || rep.Output != "no volume "+volName+" on this machine" {
		t.Errorf("%+v", rep)
	}
}
