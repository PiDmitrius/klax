// Package ids generates the opaque identifiers klax hands out: session klax_id and the live
// channel's process epoch. An id is a string of [A-Za-z0-9] compared only for equality; its length
// is not part of any contract, so ids of different lengths coexist after Length changes.
package ids

import "crypto/rand"

const (
	alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// Length is the size of newly generated ids.
	Length = 8
)

// New returns a fresh random id of Length characters.
func New() string {
	b := make([]byte, Length)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
