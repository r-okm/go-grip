// Package frontmatter detects and extracts YAML frontmatter from markdown
// sources and renders it as an HTML table.
package frontmatter

import (
	"bytes"
	"fmt"
	"html"
	"strings"

	"gopkg.in/yaml.v3"
)

// Extract splits src into (frontmatterBytes, bodyBytes, found).
//
// found is true only when:
//   - src starts with exactly "---\n" or "---\r\n" at byte offset 0
//   - a closing "---" appears on its own line before EOF
//   - the bytes between the delimiters parse as valid YAML
//
// When found is false, bodyBytes == src (full content unchanged, no copy).
// When found is true, bodyBytes is the content after the closing delimiter line.
func Extract(src []byte) (fm []byte, body []byte, found bool) {
	// Must start with --- at byte offset 0
	if !bytes.HasPrefix(src, []byte("---\n")) && !bytes.HasPrefix(src, []byte("---\r\n")) {
		return nil, src, false
	}

	// Find the newline that ends the opening delimiter
	openEnd := bytes.IndexByte(src, '\n')
	if openEnd < 0 {
		return nil, src, false
	}
	openEnd++ // advance past the \n

	// Scan lines after the opener for a closing --- or ...
	rest := src[openEnd:]
	closeStart := -1
	closeEnd := -1

	for i := 0; i < len(rest); {
		// Find end of current line
		nl := bytes.IndexByte(rest[i:], '\n')
		var lineEnd int
		if nl < 0 {
			lineEnd = len(rest)
		} else {
			lineEnd = i + nl + 1
		}
		line := rest[i:lineEnd]
		stripped := bytes.TrimRight(line, "\r\n")
		if bytes.Equal(stripped, []byte("---")) {
			closeStart = i
			closeEnd = lineEnd
			break
		}
		if nl < 0 {
			break
		}
		i = lineEnd
	}

	if closeStart < 0 {
		return nil, src, false
	}

	fmBytes := rest[:closeStart]

	// Validate that fmBytes is parseable YAML
	var v interface{}
	if err := yaml.Unmarshal(fmBytes, &v); err != nil {
		return nil, src, false
	}

	return fmBytes, rest[closeEnd:], true
}

// RenderTable renders parsed YAML frontmatter bytes as an HTML table laid out
// the way GitHub shows frontmatter: one header row holding the top-level keys
// and one body row holding their values. Keys appear in document order.
// Nested values are rendered as their string representation. All values are
// HTML-escaped. Frontmatter without top-level keys renders as nothing.
func RenderTable(fm []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, err
	}

	var keys, vals []string

	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		mapping := doc.Content[0]
		if mapping.Kind == yaml.MappingNode {
			// MappingNode Content is [key, value, key, value, ...]
			for i := 0; i+1 < len(mapping.Content); i += 2 {
				keys = append(keys, mapping.Content[i].Value)
				vals = append(vals, nodeToString(mapping.Content[i+1]))
			}
		}
	}

	if len(keys) == 0 {
		return nil, nil
	}

	var buf strings.Builder
	buf.WriteString(`<table class="frontmatter-table">` + "\n")
	buf.WriteString("<thead>\n<tr>")
	for _, k := range keys {
		buf.WriteString("<th>")
		buf.WriteString(html.EscapeString(k))
		buf.WriteString("</th>")
	}
	buf.WriteString("</tr>\n</thead>\n")
	buf.WriteString("<tbody>\n<tr>")
	for _, v := range vals {
		buf.WriteString("<td>")
		buf.WriteString(html.EscapeString(v))
		buf.WriteString("</td>")
	}
	buf.WriteString("</tr>\n</tbody>\n")
	buf.WriteString("</table>\n")

	return []byte(buf.String()), nil
}

// nodeToString converts a yaml.Node value to a human-readable string.
// Scalars return their value directly; sequences and mappings use fmt.Sprintf.
func nodeToString(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.SequenceNode:
		parts := make([]string, len(n.Content))
		for i, child := range n.Content {
			parts[i] = nodeToString(child)
		}
		return fmt.Sprintf("[%s]", strings.Join(parts, ", "))
	case yaml.MappingNode:
		parts := make([]string, 0, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			parts = append(parts, nodeToString(n.Content[i])+": "+nodeToString(n.Content[i+1]))
		}
		return fmt.Sprintf("{%s}", strings.Join(parts, ", "))
	default:
		return n.Value
	}
}
