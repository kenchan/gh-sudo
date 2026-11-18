package sudo

import (
	"crypto/subtle"
	"fmt"
)

type secret struct {
	value string
}

const redacted = "[REDACTED]"

func (s secret) String() string   { return redacted }
func (s secret) GoString() string { return redacted }

func (s secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

func (s secret) reveal() string { return s.value }

func (s secret) isZero() bool { return s.value == "" }

func (s secret) equal(o secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.value), []byte(o.value)) == 1
}
