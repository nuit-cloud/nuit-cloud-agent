package runner

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The platform's registry of SSH public keys (nuit-cloud/nuit-cloud#92) reaches the login profile as
//
//	login: { format: 2, keys: [ { id, publicKey }, ... ] }
//
// and becomes NCS_LOGIN_KEYS, one `<id>=<base64>` per key separated by spaces: the type is always ssh-ed25519 and the key's comment
// is dropped, so nothing but the id alphabet and the base64 alphabet reaches the script. The older form `login: { publicKey }` (an
// older platform) still becomes NCS_PLATFORM_SSH_KEY, as before.
const (
	loginFormat  = 2
	loginMaxKeys = 64
)

var (
	loginKeyID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	// one ssh-ed25519 key: the type, the 68 base64 characters of its 51-byte blob, and an optional comment (printable ASCII, ignored)
	registryKey = regexp.MustCompile(`^ssh-ed25519 ([A-Za-z0-9+/]{68})( [\x21-\x7e][\x20-\x7e]{0,199})?$`)
)

// errLogin is what every refusal of the login parameter says first, so the task's output tells the platform where the problem is.
func errLogin(format string, a ...any) error {
	return fmt.Errorf("the task's login keys are not usable: "+format, a...)
}

// loginEnvironment checks the login parameter in either form and returns the script environment for it. Anything it does not accept
// fails the task before the machine is touched; in particular an empty key list, which would take every platform key away.
func loginEnvironment(raw any) ([]string, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, errLogin("expected an object")
	}
	_, hasFormat := m["format"]
	_, hasKeys := m["keys"]
	if !hasFormat && !hasKeys {
		key, _ := m["publicKey"].(string)
		if !loginKey.MatchString(key) {
			return nil, errors.New("the task carries no usable ssh-ed25519 public key")
		}
		return []string{"NCS_PLATFORM_SSH_KEY=" + key}, nil
	}
	for k := range m {
		if k != "format" && k != "keys" {
			return nil, errLogin("unexpected field %q beside format and keys", k)
		}
	}
	if f, ok := m["format"].(float64); !ok || f != loginFormat {
		return nil, errLogin("format must be %d", loginFormat)
	}
	list, ok := m["keys"].([]any)
	if !ok {
		return nil, errLogin("keys must be a list")
	}
	if len(list) == 0 {
		return nil, errLogin("the list is empty, which would leave no platform key on the machine")
	}
	if len(list) > loginMaxKeys {
		return nil, errLogin("%d keys, at most %d", len(list), loginMaxKeys)
	}
	ids, bodies := map[string]bool{}, map[string]bool{}
	out := make([]string, 0, len(list))
	for i, v := range list {
		k, ok := v.(map[string]any)
		if !ok {
			return nil, errLogin("key %d is not an object", i+1)
		}
		for f := range k {
			if f != "id" && f != "publicKey" {
				return nil, errLogin("key %d has an unexpected field %q", i+1, f)
			}
		}
		id, _ := k["id"].(string)
		if !loginKeyID.MatchString(id) {
			return nil, errLogin("key %d has no usable id (lower-case letters, digits and -, 1 to 40, starting with a letter or digit)", i+1)
		}
		pub, _ := k["publicKey"].(string)
		match := registryKey.FindStringSubmatch(pub)
		if match == nil || !ed25519Blob(match[1]) {
			return nil, errLogin("key %q is not a single ssh-ed25519 public key line", id)
		}
		if ids[id] {
			return nil, errLogin("the id %q appears twice", id)
		}
		if bodies[match[1]] {
			return nil, errLogin("key %q is listed twice (under another id too)", id)
		}
		ids[id], bodies[match[1]] = true, true
		out = append(out, id+"="+match[1])
	}
	return []string{"NCS_LOGIN_KEYS=" + strings.Join(out, " ")}, nil
}

// ed25519Blob reports whether b64 is the wire form of an ssh-ed25519 public key: the type string and a 32-byte key, nothing else.
func ed25519Blob(b64 string) bool {
	blob, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil || len(blob) != 4+11+4+32 {
		return false
	}
	if binary.BigEndian.Uint32(blob[0:4]) != 11 || !bytes.Equal(blob[4:15], []byte("ssh-ed25519")) {
		return false
	}
	return binary.BigEndian.Uint32(blob[15:19]) == 32
}
