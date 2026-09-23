package lint

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Reader walks a YAML node tree, collecting problems instead of stopping at
// the first one.
//
// It exists because the schema parsers need both node positions and unknown
// field detection. Decoding into structs gives neither: yaml.v3 cannot report
// unknown fields when decoding from a node, and struct fields carry no line
// numbers.
type Reader struct {
	file     string
	problems []Problem
}

func NewReader(file string) *Reader {
	return &Reader{file: file}
}

func (r *Reader) File() string { return r.file }

func (r *Reader) Problems() []Problem { return r.problems }

// Count is the number of problems so far, which lets a caller attribute the
// ones added while reading a particular item.
func (r *Reader) Count() int { return len(r.problems) }

// AttributeFrom names every problem added since index that has no subject yet.
// An item's name is usually only known once its whole mapping has been read.
func (r *Reader) AttributeFrom(index int, subject string) {
	for i := index; i < len(r.problems); i++ {
		if r.problems[i].Subject == "" {
			r.problems[i].Subject = subject
		}
	}
}

func (r *Reader) Add(line int, check string, sev Severity, format string, args ...any) {
	r.problems = append(r.problems, NewProblem(r.file, line, check, sev,
		fmt.Sprintf(format, args...)))
}

// Document parses the bytes and returns the document's root node. A syntax
// error is the one failure that stops everything, because nothing after it
// can be read.
func (r *Reader) Document(data []byte) (*yaml.Node, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		r.Add(0, CheckYAMLSyntax, SeverityError, "%s", err.Error())
		return nil, false
	}
	// An empty file parses cleanly and holds nothing.
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, false
	}
	return root.Content[0], true
}

// Entry is one key/value pair of a YAML mapping.
type Entry struct {
	Key   *yaml.Node
	Value *yaml.Node
}

func Entries(n *yaml.Node) []Entry {
	out := make([]Entry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, Entry{Key: n.Content[i], Value: n.Content[i+1]})
	}
	return out
}

func (r *Reader) UnknownField(key *yaml.Node, where string) {
	r.Add(key.Line, CheckYAMLUnknownField, SeverityError,
		"unknown field %q in %s", key.Value, where)
}

func (r *Reader) Mapping(n *yaml.Node, where string) bool {
	if n.Kind != yaml.MappingNode {
		r.Add(n.Line, CheckYAMLType, SeverityError, "%s must be a mapping", where)
		return false
	}
	return true
}

func (r *Reader) Sequence(n *yaml.Node, where string) bool {
	if n.Kind != yaml.SequenceNode {
		r.Add(n.Line, CheckYAMLType, SeverityError, "%s must be a list", where)
		return false
	}
	return true
}

func (r *Reader) Scalar(n *yaml.Node, where string) (string, bool) {
	if n.Kind != yaml.ScalarNode {
		r.Add(n.Line, CheckYAMLType, SeverityError, "%s must be a scalar", where)
		return "", false
	}
	return n.Value, true
}

func (r *Reader) Duration(n *yaml.Node, where string) (time.Duration, bool) {
	raw, ok := r.Scalar(n, where)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		r.Add(n.Line, CheckYAMLType, SeverityError, "%s is not a duration: %q", where, raw)
		return 0, false
	}
	return d, true
}

func (r *Reader) Int(n *yaml.Node, where string) (int, bool) {
	raw, ok := r.Scalar(n, where)
	if !ok {
		return 0, false
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		r.Add(n.Line, CheckYAMLType, SeverityError, "%s is not a whole number: %q", where, raw)
		return 0, false
	}
	return v, true
}

// StringMap reads a mapping of scalars, recording each key's line under
// prefix so a finding can point at the offending entry.
func (r *Reader) StringMap(n *yaml.Node, prefix, where string, lines map[string]int) map[string]string {
	if !r.Mapping(n, where) {
		return nil
	}
	out := make(map[string]string, len(n.Content)/2)
	for _, e := range Entries(n) {
		lines[prefix+"."+e.Key.Value] = e.Key.Line
		v, ok := r.Scalar(e.Value, fmt.Sprintf("%s.%s", where, e.Key.Value))
		if !ok {
			continue
		}
		out[e.Key.Value] = v
	}
	return out
}

// Lines records where each key of an item appeared. Nested map keys use a
// dotted path, for example "annotations.runbook_url".
type Lines struct {
	Start int
	keys  map[string]int
}

func NewLines(start int) Lines {
	return Lines{Start: start, keys: map[string]int{}}
}

func (l Lines) Set(key string, line int) { l.keys[key] = line }

func (l Lines) Has(key string) bool {
	_, ok := l.keys[key]
	return ok
}

// Of returns the line of the first key present, so a finding about a missing
// nested key can fall back to its parent block and then to the item itself.
func (l Lines) Of(keys ...string) int {
	for _, key := range keys {
		if line, ok := l.keys[key]; ok {
			return line
		}
	}
	return l.Start
}

func (l Lines) Keys() map[string]int { return l.keys }
