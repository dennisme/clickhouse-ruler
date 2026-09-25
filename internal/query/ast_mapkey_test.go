package query

import "testing"

// Every subscript the fixture performs, tagged by how its key was written. A
// numeric key is carried rather than dropped: `arr[1]` and a `Map(UInt64, …)`
// lookup are written identically, so the tree cannot settle which it is and
// resolveKeys decides from the column's type.
func TestMapKeysReadsEverySubscript(t *testing.T) {
	keys, _ := mapKeys(readAST(t, "ast_map_keys.txt"))

	want := []MapKey{
		{Column: "Durations", Key: "1", Numeric: true},
		{Column: "LogAttributes", Key: "checkout"},
		{Column: "LogAttributes", Key: "payment_id"},
		{Column: "Nested", Key: "outer"},
		{Column: "t.SpanAttributes", Key: "tenant"},
	}

	if len(keys) != len(want) {
		t.Fatalf("mapKeys = %v, want %v", keys, want)
	}
	for i, w := range want {
		if keys[i] != w {
			t.Errorf("key %d = %v, want %v", i, keys[i], w)
		}
	}
}

// A tuple has positions rather than keys, so nothing about it can be missing
// and it is not a subscript this check has anything to say about.
func TestMapKeysIgnoresTupleAccess(t *testing.T) {
	keys, skipped := mapKeys(readAST(t, "ast_map_keys.txt"))

	for _, k := range keys {
		if k.Column == "Bounds" {
			t.Errorf("read %v from tuple access", k)
		}
	}
	for _, s := range skipped {
		if s.Column == "Bounds" {
			t.Errorf("skipped %v, but tuple access was never a key to read", s)
		}
	}
}

// A key built at evaluation time cannot be looked for now. Reported rather than
// dropped: the author wrote a lookup and it went unchecked (spec 7.3).
func TestMapKeysReportsAKeyItCannotRead(t *testing.T) {
	_, skipped := mapKeys(readAST(t, "ast_map_keys.txt"))

	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want the expression key alone", skipped)
	}
	if skipped[0].Column != "LogAttributes" || skipped[0].Reason != skipDynamicKey {
		t.Errorf("skipped = %v, want LogAttributes as %q", skipped[0], skipDynamicKey)
	}
}

// `LogAttributes.keys` names no key, so it is neither checked nor reported as
// something that could not be read. Counting it as a skip would tell an author
// a key was ignored when they never wrote one.
func TestMapKeysIgnoresTheKeysColumn(t *testing.T) {
	keys, skipped := mapKeys(readAST(t, "ast_map_keys.txt"))

	for _, k := range keys {
		if k.Column == "LogAttributes.keys" || k.Key == "keys" {
			t.Errorf("read %v from LogAttributes.keys, which names no key", k)
		}
	}
	for _, s := range skipped {
		if s.Column == "LogAttributes.keys" {
			t.Errorf("skipped %v, but nothing about it was a subscript", s)
		}
	}
}

// The identifier is carried as written, qualifier and all. A dot is a table
// alias in `t.SpanAttributes` and part of the name in `Events.Attributes`, which
// a Nested block flattened into, and only the table can say which. resolveKeys
// does that; stripping here would make the second one unfindable.
func TestMapKeysKeepsTheIdentifierAsWritten(t *testing.T) {
	keys, _ := mapKeys(readAST(t, "ast_map_keys.txt"))

	var found bool
	for _, k := range keys {
		if k.Column == "t.SpanAttributes" {
			found = true
		}
	}
	if !found {
		t.Errorf("mapKeys = %v, want the qualifier kept for the table to resolve", keys)
	}
}

// `EXPLAIN AST` escapes the SQL literal it prints, so what arrives is the
// literal text escaped once more. Probing for the escaped form looks for a key
// nobody wrote.
func TestMapKeysDecodesTheLiteral(t *testing.T) {
	keys, _ := mapKeys(readAST(t, "ast_map_keys_escaped.txt"))

	want := []string{"", "back\\slash", "quote'd", "utf-8 ☃"}

	if len(keys) != len(want) {
		t.Fatalf("mapKeys = %v, want %d keys", keys, len(want))
	}
	for i, w := range want {
		if keys[i].Key != w {
			t.Errorf("key %d = %q, want %q", i, keys[i].Key, w)
		}
	}
}

// Sorted and deduplicated, because a rule reading the same key twice is one
// question, and a finding that names a key twice reads as two problems.
func TestMapKeysAreSortedAndDeduplicated(t *testing.T) {
	root, err := parseAST(`SelectWithUnionQuery (children 1)
 ExpressionList (children 3)
  Function arrayElement (children 1)
   ExpressionList (children 2)
    Identifier LogAttributes
    Literal \'zebra\'
  Function arrayElement (children 1)
   ExpressionList (children 2)
    Identifier LogAttributes
    Literal \'apple\'
  Function mapContains (children 1)
   ExpressionList (children 2)
    Identifier LogAttributes
    Literal \'zebra\'
`)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	keys, _ := mapKeys(root)

	if len(keys) != 2 {
		t.Fatalf("mapKeys = %v, want apple and zebra once each", keys)
	}
	if keys[0].Key != "apple" || keys[1].Key != "zebra" {
		t.Errorf("mapKeys = %v, want sorted", keys)
	}
}
