package survey

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"regexp"
	"strings"
)

// LoginAccount is the account the platform logs in as (bootstrap's NC_USER).
const LoginAccount = "nuitcloud"

// maxAuthorizedKeys bounds what the survey reports from one file, so a huge file cannot make the report large.
const maxAuthorizedKeys = 256

// AuthorizedKey is one key in the login account's authorized_keys: public facts only (never the key itself, never a comment
// other than the platform's own marker).
type AuthorizedKey struct {
	Fingerprint string `json:"fingerprint"` // SHA256:<unpadded base64>, as ssh-keygen -l prints it
	Type        string `json:"type"`        // ssh-ed25519, ssh-rsa, ...
	// ManagedID is the registry id from a line the platform marked `nuit-cloud:<id>`; null for a line the platform does not manage.
	ManagedID *string `json:"managedId"`
}

// keyTypes are the key types sshd accepts in authorized_keys.
var keyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true, "ssh-dss": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// managedMarker is the comment the agent writes on a registry line. It matches what the login feature (features.sh, LOGIN_SYNC_AWK)
// treats as a platform line, which it removes unless the registry lists it, so the survey and the feature agree on what is managed.
var managedMarker = regexp.MustCompile(`^nuit-cloud:([a-z0-9][a-z0-9-]*)$`)

// ParseAuthorizedKeys reads an authorized_keys file. A line with options before the key is found by its key type; a line that holds
// no recognisable key is skipped. A line is the platform's only when it is exactly `<type> <base64> nuit-cloud:<id>`.
func ParseAuthorizedKeys(text string) []AuthorizedKey {
	out := []AuthorizedKey{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if !keyTypes[fields[i]] {
				continue
			}
			blob, err := base64.StdEncoding.DecodeString(fields[i+1])
			if err != nil || len(blob) == 0 {
				break
			}
			sum := sha256.Sum256(blob)
			k := AuthorizedKey{Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), Type: fields[i]}
			if i == 0 && len(fields) == 3 {
				if m := managedMarker.FindStringSubmatch(fields[2]); m != nil {
					id := m[1]
					k.ManagedID = &id
				}
			}
			out = append(out, k)
			break
		}
		if len(out) == maxAuthorizedKeys {
			break
		}
	}
	return out
}

// loginHome finds the login account's home in /etc/passwd (the account is a local one, made when the machine was installed).
func loginHome(passwd string) string {
	for _, line := range strings.Split(passwd, "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 7 && f[0] == LoginAccount {
			return f[5]
		}
	}
	return ""
}

// collectAuthorizedKeys reports the keys that can log in as the platform's login account. A missing file is no keys ([]); a file that
// cannot be read is nil (unknown) and a collection problem, so a gap is not mistaken for an empty file.
func collectAuthorizedKeys(s Sources, problem func(string, error)) []AuthorizedKey {
	passwd, err := s.read("/etc/passwd")
	if err != nil {
		problem("authorized_keys", err)
		return nil
	}
	home := loginHome(passwd)
	if home == "" || !strings.HasPrefix(home, "/") {
		problem("authorized_keys", errors.New("there is no account "+LoginAccount))
		return nil
	}
	text, err := s.read(home + "/.ssh/authorized_keys")
	if errors.Is(err, fs.ErrNotExist) {
		return []AuthorizedKey{}
	}
	if err != nil {
		problem("authorized_keys", err)
		return nil
	}
	return ParseAuthorizedKeys(text)
}
