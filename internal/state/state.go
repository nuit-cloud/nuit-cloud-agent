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
	PlatformURL string `json:"platformUrl"`
	MachineID   string `json:"machineId"`
	Name        string `json:"name"`
	Credential  string `json:"credential"`
	// TrustedKeys are the keys a task may be signed by (a list from the first release; a rollover adds one and later retires the old).
	// Read and change it through Keys, TrustKey and RetireKey once the agent is running: a task may change it while the loop reads it.
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

// MaxTrustedKeys is how many task-signing keys an agent trusts at once. A rollover needs two; a third would mean an old key was never retired.
const MaxTrustedKeys = 4

// Refusals of a change to the trusted keys; their text is reported to the platform.
var (
	ErrKeyIDClash      = errors.New("a different key is already trusted under this key id")
	ErrTooManyKeys     = fmt.Errorf("this machine already trusts %d keys: retire one first", MaxTrustedKeys)
	ErrRetireSigner    = errors.New("refused: this task is signed by the key it would retire; retire a key with a task signed by another trusted key")
	ErrRetireLastKey   = errors.New("refused: this is the only key this machine trusts; retiring it would leave no way to send it a task")
	ErrNoKeyToTrustYet = errors.New("this machine trusts no key yet")
)

// Keys returns a copy of the trusted keys.
func (s *State) Keys() []taskverify.Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]taskverify.Key(nil), s.TrustedKeys...)
}

// KeyIDs returns the ids of the trusted keys, in the order they were trusted.
func (s *State) KeyIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.TrustedKeys))
	for _, k := range s.TrustedKeys {
		ids = append(ids, k.KeyID)
	}
	return ids
}

// TrustKey adds a key and saves at once. It reports whether anything changed: a key already trusted is not an error. The caller has
// checked the key (taskverify.KeyFromParams) and that the request came in a task signed by a key already trusted.
func (s *State) TrustKey(k taskverify.Key) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.TrustedKeys) == 0 {
		return false, ErrNoKeyToTrustYet // nothing could have signed the request
	}
	for _, have := range s.TrustedKeys {
		if have.KeyID == k.KeyID {
			if have.PublicKey == k.PublicKey {
				return false, nil
			}
			return false, ErrKeyIDClash
		}
	}
	if len(s.TrustedKeys) >= MaxTrustedKeys {
		return false, ErrTooManyKeys
	}
	before := s.TrustedKeys
	s.TrustedKeys = append(append([]taskverify.Key(nil), before...), k)
	if err := s.saveLocked(); err != nil {
		s.TrustedKeys = before // not trusted unless it is on disk
		return false, err
	}
	return true, nil
}

// RetireKey stops trusting a key and saves at once. It never removes the key that signed the request (signedBy) nor the last key, so a
// mistaken retire cannot lock the agent out. A key that is not trusted is not an error; it reports whether anything changed.
func (s *State) RetireKey(id, signedBy string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := -1
	for i, k := range s.TrustedKeys {
		if k.KeyID == id {
			at = i
		}
	}
	if at < 0 {
		return false, nil
	}
	if id == signedBy {
		return false, ErrRetireSigner
	}
	if len(s.TrustedKeys) <= 1 {
		return false, ErrRetireLastKey
	}
	before := s.TrustedKeys
	kept := make([]taskverify.Key, 0, len(before)-1)
	kept = append(append(kept, before[:at]...), before[at+1:]...)
	s.TrustedKeys = kept
	if err := s.saveLocked(); err != nil {
		s.TrustedKeys = before
		return false, err
	}
	return true, nil
}

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

// Pending returns a copy of the pending update, or nil.
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
