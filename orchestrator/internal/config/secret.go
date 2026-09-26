package config

import (
	"encoding/json"
	"strings"
)

// Secret wraps a credential so that printing it does not leak it.
//
// Credentials escape into logs by accident, not by intent: a %v on a struct, a
// JSON dump of config at boot, an error wrapping a connection string. Secret
// closes those paths by implementing Stringer, GoStringer and json.Marshaler to
// return a placeholder. Reading the value requires calling Reveal, which is
// greppable and therefore reviewable.
type Secret string

const redacted = "[REDACTED]"

// String satisfies fmt.Stringer, covering %s, %v and print-family calls.
func (s Secret) String() string { return redacted }

// GoString satisfies fmt.GoStringer, covering %#v.
func (s Secret) GoString() string { return redacted }

// MarshalJSON prevents a struct dump from carrying the value.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// MarshalText covers encoders that prefer TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal returns the underlying value. Every call site is a deliberate decision
// to handle plaintext, and `grep -rn '\.Reveal()'` enumerates them.
func (s Secret) Reveal() string { return string(s) }

// IsZero reports whether the secret is unset.
func (s Secret) IsZero() bool { return len(s) == 0 }

// Fingerprint returns a short, non-reversible label for a secret, suitable for
// logs and for confirming which credential is loaded without disclosing it.
//
// It exposes only the length and the last two characters. Two characters is
// enough for an operator to tell one key from another and far too little to
// reconstruct one.
func (s Secret) Fingerprint() string {
	if len(s) == 0 {
		return "unset"
	}
	if len(s) <= 4 {
		return "len=" + itoa(len(s)) + " ...**"
	}
	return "len=" + itoa(len(s)) + " ..." + string(s[len(s)-2:])
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// looksLikePlaceholder reports whether a value is one of the obvious
// not-yet-filled-in markers, so the loader can reject it rather than fail later
// with a confusing authentication error from a third party.
func looksLikePlaceholder(v string) bool {
	t := strings.ToLower(strings.TrimSpace(v))
	switch t {
	case "", "changeme", "todo", "xxx", "your-key-here", "<your-key>", "replace-me", "placeholder":
		return true
	}
	return strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">")
}
