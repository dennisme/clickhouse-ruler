package alert

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
	"strings"
)

// sameLabels reports whether two final label sets are the same alert.
//
// The fingerprint alone cannot answer that: it is 64 bits, so two different
// label sets can hash alike. Everything deciding identity compares the labels
// themselves and treats the hash as the bucket it is.
func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// labelKey renders a label set for a human to read and for a tie-break to
// order on. Not an identity: sameLabels decides that.
func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(labels[k])
	}
	return b.String()
}

// fingerprint identifies an alert instance by its label set.
//
// Keys are sorted so that Go's randomized map iteration cannot change the
// result, and each key and value is length-prefixed rather than joined with a
// separator. A separator of any kind can appear inside a ClickHouse column
// value, and a collision there would silently merge two different alerts into
// one instance.
func fingerprint(labels map[string]string) uint64 {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := fnv.New64a()
	var length [8]byte

	// hash.Hash documents that Write never returns an error.
	write := func(s string) {
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(s))
	}

	for _, k := range keys {
		write(k)
		write(labels[k])
	}
	return h.Sum64()
}
