package survey

import (
	"errors"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/rootconsole"
)

// collectRootPassword reports whether root has a password, from /etc/shadow's second field: "none", "locked" or "set" (see
// rootconsole.State). It never reports the field. A file that cannot be read, or that has no line for root, is nil (unknown) and a
// collection problem, so a gap is not mistaken for a locked account.
func collectRootPassword(s Sources, problem func(string, error)) *string {
	text, err := s.read("/etc/shadow")
	if err != nil {
		problem("root-password", err)
		return nil
	}
	field, ok := rootconsole.ShadowField(text, "root")
	if !ok {
		problem("root-password", errors.New("/etc/shadow has no line for root"))
		return nil
	}
	state := rootconsole.State(field)
	return &state
}
