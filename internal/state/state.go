// Package state is what the agent remembers between runs: who it is on the platform, the keys it trusts, and which tasks it
// has already run. It is one small file, written atomically, readable only by root.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

const fileName = "state.json"

// SeenFor is how long a run task's id is remembered. A task expires within an hour of being queued, so a replay after this
// is already refused as expired.
const SeenFor = 48 * time.Hour

// State is the agent's identity and memory.
type State struct {
	PlatformURL string           `json:"platformUrl"`
	MachineID   string           `json:"machineId"`
	Name        string           `json:"name"`
	Credential  string           `json:"credential"`
	TrustedKeys []taskverify.Key `json:"trustedKeys"`
	EnrolledAt  time.Time        `json:"enrolledAt"`
	// Seen maps a task id to when the agent started it.
	Seen map[string]time.Time `json:"seen"`
	// Fallbacks are fixed addresses (`host:port`) to connect to when the platform's name does not work; the platform names them.
	Fallbacks []string `json:"fallbacks,omitempty"`
	// PendingUpdate is set while a freshly installed version has not yet proved itself by checking in (see package update).
	PendingUpdate *PendingUpdate `json:"pendingUpdate,omitempty"`

	mu  sync.Mutex
	dir string
}

// PendingUpdate records an update that has been installed but not yet confirmed.
type PendingUpdate struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	// Attempts counts how many times the new version has started without being confirmed.
	Attempts int `json:"attempts"`
	// Previous is where the binary that was replaced is kept, to go back to.
	Previous string `json:"previous"`
}

// ErrNotEnrolled is returned by Load when there is no state yet.
var ErrNotEnrolled = errors.New("this machine is not enrolled: run `nuit-cloud-agent enroll` first")

// Load reads the state in dir.
func Load(dir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotEnrolled
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("the state file is damaged: %w", err)
	}
	if s.PlatformURL == "" || s.MachineID == "" || s.Credential == "" {
		return nil, errors.New("the state file is incomplete")
	}
	if s.Seen == nil {
		s.Seen = map[string]time.Time{}
	}
	s.dir = dir
	return &s, nil
}

// New makes a state for a machine that has just enrolled. It is not on disk until Save.
func New(dir string) *State {
	return &State{Seen: map[string]time.Time{}, dir: dir}
}

// Save writes the state to disk: to a temporary file in the same directory, flushed, then renamed over the old one, so a
// crash leaves either the old state or the new, never half of one. The directory and file are private to their owner.
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *State) saveLocked() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, fileName+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.dir, fileName))
}

// Dir is where the state is kept.
func (s *State) Dir() string { return s.dir }

// HasSeen reports whether a task id has been run before.
func (s *State) HasSeen(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.Seen[id]
	return ok
}

// MarkSeen records that a task is about to run, and forgets ids older than SeenFor. It is saved before the task starts, so a
// crash in the middle can never lead to the same task running twice.
func (s *State) MarkSeen(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Seen[id] = at
	for old, when := range s.Seen {
		if at.Sub(when) > SeenFor {
			delete(s.Seen, old)
		}
	}
	return s.saveLocked()
}

// SetCredential replaces the agent's credential and saves it at once.
func (s *State) SetCredential(credential string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Credential = credential
	return s.saveLocked()
}

// Pending returns a copy of the pending update, or nil.
// SetFallbacks remembers the fixed addresses, and writes the state only if they changed.
func (s *State) SetFallbacks(list []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(list) == len(s.Fallbacks) {
		same := true
		for i := range list {
			same = same && list[i] == s.Fallbacks[i]
		}
		if same {
			return nil
		}
	}
	s.Fallbacks = append([]string(nil), list...)
	return s.saveLocked()
}

func (s *State) Pending() *PendingUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PendingUpdate == nil {
		return nil
	}
	p := *s.PendingUpdate
	return &p
}

// SetPending records an installed, unconfirmed update and saves it at once.
func (s *State) SetPending(p PendingUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PendingUpdate = &p
	return s.saveLocked()
}

// ClearPending forgets the pending update (it was confirmed, rolled back, or is no longer relevant).
func (s *State) ClearPending() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PendingUpdate == nil {
		return nil
	}
	s.PendingUpdate = nil
	return s.saveLocked()
}

// BumpPendingAttempts counts one more start of the unconfirmed version and returns the new count (0 if nothing is pending).
func (s *State) BumpPendingAttempts() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.PendingUpdate == nil {
		return 0, nil
	}
	s.PendingUpdate.Attempts++
	return s.PendingUpdate.Attempts, s.saveLocked()
}
