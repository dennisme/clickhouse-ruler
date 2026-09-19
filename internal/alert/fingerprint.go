package alert

import (
	"encoding/binary"
	"hash/fnv"
	"sort"
)

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
