// Package tomledit makes small, targeted edits to a TOML file's text without
// disturbing anything else in it — comments, formatting, key order, unrelated
// sections all survive untouched.
//
// This exists because canaveral.toml is a file users hand-edit and comment
// extensively (as every manifest in this project demonstrates), and
// re-marshalling the whole file from a parsed struct would silently discard
// all of that. Only one thing here needs to be updated programmatically —
// [layout.current], written back automatically after the user resizes their
// windows — so only that one table is ever touched.
package tomledit

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// tableHeaderRe matches both "[table]" and "[[array.of.tables]]" header
// lines. Array-of-tables headers ([[agent]], [[window]], ...) must count as
// section boundaries too, even though findTable never searches for one by
// name, otherwise scanning for "where does this table's body end" would run
// straight through them and swallow unrelated content.
var tableHeaderRe = regexp.MustCompile(`^\s*(\[\[?)([A-Za-z0-9_.-]+)(\]\]?)\s*(#.*)?$`)

// isTableHeader reports whether a line is any kind of table header, and its
// name when it is a plain (non-array) "[table]" header.
func isTableHeader(line string) (name string, isPlain, isHeader bool) {
	m := tableHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return "", false, false
	}
	plain := m[1] == "[" && m[3] == "]"
	return m[2], plain, true
}

// ReplaceTable rewrites the body of the named TOML table (for example
// "layout.current") in the file at path so it contains exactly the given
// key/value pairs, in the given key order. If the table does not exist yet,
// it is appended immediately after afterTable (or at end of file if
// afterTable is also absent). Every other line in the file is left exactly
// as it was.
func ReplaceTable(path, table string, keys []string, values map[string]float64, afterTable string) error {
	orig, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Trim a trailing newline (and any trailing blank lines) before slicing
	// so it can never leak into the middle of the output; exactly one is
	// added back unconditionally at the end, which is the normal, expected
	// shape for a text file regardless of what the original happened to have.
	lines := strings.Split(strings.TrimRight(string(orig), "\n"), "\n")
	body := renderBody(keys, values)

	start, end, found := findTable(lines, table)
	if found {
		out := make([]string, 0, len(lines))
		out = append(out, lines[:start+1]...) // keep the [table] header line itself
		out = append(out, body...)
		out = append(out, lines[end:]...)
		return writeIfChanged(path, orig, out)
	}

	insertAt := len(lines)
	if afterTable != "" {
		if _, aEnd, ok := findTable(lines, afterTable); ok {
			insertAt = aEnd
		}
	}
	out := make([]string, 0, len(lines)+len(body)+2)
	out = append(out, lines[:insertAt]...)
	if insertAt > 0 && strings.TrimSpace(lines[insertAt-1]) != "" {
		out = append(out, "")
	}
	out = append(out, "["+table+"]")
	out = append(out, body...)
	if insertAt < len(lines) && strings.TrimSpace(lines[insertAt]) != "" {
		out = append(out, "")
	}
	out = append(out, lines[insertAt:]...)
	return writeIfChanged(path, orig, out)
}

// findTable locates a plain "[table]" header's line and the end of its body
// (the index of the next table header of any kind, or len(lines)).
func findTable(lines []string, table string) (headerIdx, bodyEnd int, found bool) {
	for i, l := range lines {
		name, plain, isHeader := isTableHeader(l)
		if !isHeader || !plain || name != table {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if _, _, isHeader := isTableHeader(lines[j]); isHeader {
				end = j
				break
			}
		}
		return i, end, true
	}
	return 0, 0, false
}

// renderBody formats float values as minimal, readable TOML key/value lines.
func renderBody(keys []string, values map[string]float64) []string {
	ordered := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if _, ok := values[k]; ok && !seen[k] {
			ordered = append(ordered, k)
			seen[k] = true
		}
	}
	// Any key not covered by the caller's ordering still gets written,
	// deterministically, rather than silently dropped.
	var extra []string
	for k := range values {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	ordered = append(ordered, extra...)

	out := make([]string, 0, len(ordered))
	for _, k := range ordered {
		out = append(out, fmt.Sprintf("%s = %s", k, formatFloat(values[k])))
	}
	return out
}

func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 4, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-0" {
		s = "0"
	}
	return s
}

func writeIfChanged(path string, orig []byte, lines []string) error {
	newContent := []byte(strings.Join(lines, "\n") + "\n")
	if bytes.Equal(orig, newContent) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, newContent, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
