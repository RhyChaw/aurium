// Package ids generates and validates Aurium identifiers.
//
// Every entity id is a type prefix, an underscore, and a 26-character
// Crockford base32 ULID: 48 bits of big-endian millisecond timestamp
// followed by 80 bits of cryptographic randomness. Encoding the timestamp
// first is what makes ids sort lexicographically by creation time, which the
// event log, the snapshot sequence and every "most recent N" query rely on.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

// Type prefixes. These appear in the ERD data model (§7) and in every
// user-facing id, so they are part of the public contract: never renumber or
// reuse one.
const (
	Project     = "p"
	Task        = "t"
	Container   = "c"
	Snapshot    = "s"
	Agent       = "a"
	Message     = "m"
	Integration = "i"
	Event       = "e"
	Approval    = "ap"
	Grant       = "g"
	ContextItem = "ci"
	Proposal    = "cp"
	Token       = "tok"
)

// crockford is the base32 alphabet from Crockford's spec: the digits plus the
// uppercase letters with I, L, O and U removed so ids cannot be misread.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodedLen is the number of base32 characters needed for 128 bits.
const encodedLen = 26

var decodeTable = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i, c := range []byte(crockford) {
		t[c] = int8(i)
	}
	return t
}()

// monotonic guards against two ids colliding inside one millisecond. Within a
// millisecond we increment the previous random component instead of drawing a
// fresh one, which keeps ids strictly increasing even under a tight loop.
var monotonic struct {
	sync.Mutex
	lastMS   uint64
	lastRand [10]byte
}

// New returns a fresh id with the given type prefix, for example
// New(Container) == "c_01J9Z3KQ7V8XG2M4P6R8T0W2YB".
func New(prefix string) string {
	ms := uint64(time.Now().UTC().UnixMilli())

	monotonic.Lock()
	var entropy [10]byte
	if ms == monotonic.lastMS {
		// Same millisecond: increment the previous entropy so the id still
		// sorts after its predecessor.
		entropy = monotonic.lastRand
		for i := len(entropy) - 1; i >= 0; i-- {
			entropy[i]++
			if entropy[i] != 0 {
				break // no carry out of this byte
			}
		}
	} else {
		if _, err := rand.Read(entropy[:]); err != nil {
			// crypto/rand on a supported platform does not fail; if it ever
			// does, fall back to the timestamp so we still produce a unique,
			// ordered id rather than panicking in library code.
			binary.BigEndian.PutUint64(entropy[:8], ms)
		}
		monotonic.lastMS = ms
	}
	monotonic.lastRand = entropy
	monotonic.Unlock()

	var raw [16]byte
	// 48-bit timestamp, big-endian, in the leading 6 bytes.
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], entropy[:])

	var sb strings.Builder
	sb.Grow(len(prefix) + 1 + encodedLen)
	sb.WriteString(prefix)
	sb.WriteByte('_')
	sb.Write(encode(raw))
	return sb.String()
}

// encode renders 128 bits as 26 Crockford base32 characters. 26*5 = 130 bits,
// so the first character carries only the top 2 bits.
func encode(raw [16]byte) []byte {
	out := make([]byte, encodedLen)
	// Treat the 16 bytes as a 128-bit big-endian integer and peel off 5 bits
	// at a time from the least significant end.
	hi := binary.BigEndian.Uint64(raw[0:8])
	lo := binary.BigEndian.Uint64(raw[8:16])
	for i := encodedLen - 1; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return out
}

// Prefix returns the type prefix of id, or "" if id is not well formed.
func Prefix(id string) string {
	i := strings.IndexByte(id, '_')
	if i <= 0 || len(id)-i-1 != encodedLen {
		return ""
	}
	if !validBody(id[i+1:]) {
		return ""
	}
	return id[:i]
}

// Valid reports whether id is well formed and carries the given type prefix.
// Callers use it to reject, say, a snapshot id passed where a container id
// belongs — a class of mistake the type prefixes exist to catch.
func Valid(id, prefix string) bool {
	return Prefix(id) == prefix
}

func validBody(body string) bool {
	if len(body) != encodedLen {
		return false
	}
	for i := 0; i < len(body); i++ {
		if decodeTable[body[i]] < 0 {
			return false
		}
	}
	return true
}

// Time recovers the creation timestamp encoded in id. The zero time is
// returned for a malformed id.
func Time(id string) time.Time {
	if Prefix(id) == "" {
		return time.Time{}
	}
	body := id[strings.IndexByte(id, '_')+1:]
	var ms uint64
	// 26 characters encode 130 bits: 2 zero pad bits followed by the 128-bit
	// value. The first 10 characters therefore carry 50 bits — the 2 pad bits
	// plus exactly the 48-bit timestamp — so the accumulator needs no shift.
	for i := 0; i < 10; i++ {
		ms = ms<<5 | uint64(decodeTable[body[i]])
	}
	return time.UnixMilli(int64(ms)).UTC()
}

// Now returns the current time in the RFC 3339 UTC form used by every
// timestamp column in the database (§7). Nothing else should format times.
func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
