package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/genai"

	"github.com/TangoEnSkai/gofer/internal/tools/shell"
)

// maxCallLine caps the length, in runes, of a one-line tool call.
const maxCallLine = 120

// callLine renders a tool call on one line, as name(key=value, ...), with
// values as JSON and the whole line cut to maxCallLine runes.
func callLine(fc *genai.FunctionCall) string {
	var b strings.Builder
	b.WriteString(fc.Name)
	b.WriteByte('(')
	for i, k := range slices.Sorted(maps.Keys(fc.Args)) {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", k, jsonText(fc.Args[k]))
	}
	b.WriteByte(')')
	s := b.String()
	if utf8.RuneCountInString(s) <= maxCallLine {
		return s
	}
	r := []rune(s)
	return string(r[:maxCallLine-1]) + "…"
}

// describeCall renders a tool call in full for a confirmation prompt: the
// command for bash, a diff preview for edit_file, the content for
// write_file, and every remaining argument as key: value.
func describeCall(fc *genai.FunctionCall) string {
	args := maps.Clone(fc.Args)
	take := func(k string) (string, bool) {
		v, ok := args[k].(string)
		if ok {
			delete(args, k)
		}
		return v, ok
	}

	var b strings.Builder
	b.WriteString(fc.Name)
	switch fc.Name {
	case shell.Name:
		if d, ok := take("description"); ok && d != "" {
			b.WriteString(": " + d)
		}
		b.WriteByte('\n')
		if c, ok := take("command"); ok {
			writeLines(&b, "  $ ", "    ", c)
		}
	case "edit_file":
		if p, ok := take("path"); ok {
			b.WriteString(" " + p)
		}
		b.WriteByte('\n')
		if o, ok := take("old_string"); ok {
			writeLines(&b, "  - ", "  - ", o)
		}
		if n, ok := take("new_string"); ok {
			writeLines(&b, "  + ", "  + ", n)
		}
	case "write_file":
		if p, ok := take("path"); ok {
			b.WriteString(" " + p)
		}
		b.WriteByte('\n')
		if c, ok := take("content"); ok {
			writeLines(&b, "  + ", "  + ", c)
		}
	default:
		b.WriteByte('\n')
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if s, ok := args[k].(string); ok && strings.Contains(s, "\n") {
			fmt.Fprintf(&b, "  %s:\n", k)
			writeLines(&b, "    ", "    ", s)
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", k, jsonText(args[k]))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// writeLines writes s line by line, the first line after first and the
// others after rest.
func writeLines(b *strings.Builder, first, rest, s string) {
	for i, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if i == 0 {
			b.WriteString(first)
		} else {
			b.WriteString(rest)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

// approvalKey identifies a tool call by name and arguments, for "always".
// encoding/json sorts map keys, so equal arguments give equal keys.
func approvalKey(fc *genai.FunctionCall) string {
	return fc.Name + " " + jsonText(fc.Args)
}

// jsonText returns v as compact JSON without HTML escaping.
func jsonText(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
