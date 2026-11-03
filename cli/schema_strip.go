package cli

import (
	"strconv"
	"strings"
	"unicode"
)

// HelpAfterUsageAnnotation is the command annotation that carries the
// response-format block rendered below Usage/Flags. Set by Operation.Command
// and inlined by the help template in cli.go.
const HelpAfterUsageAnnotation = "surf-help-after-usage"

// splitHelpBlocks splits an operation's Long description into the prose that
// should render above Cobra's Usage/Flags and the auto-generated response
// sections that should render below them.
//
// Why split instead of keep-in-place: cobra renders `.Long` before
// `.UsageString()`, so anything left in Long pushes Usage/Flags down. On the
// Surf spec the response schemas are 50–62% of --help, which buried the
// flags an agent needs to construct a call. The HelpTemplate in cli.go emits
// the returned `below` after Usage/Flags instead.
//
// Stripped from Long entirely (redundant with Cobra's rendering):
//   - `## Argument Schema:`   — duplicates positional args in Cobra Usage line
//   - `## Option Schema:`     — duplicates Cobra's `Flags:` section exactly
//
// Returned separately in `below` (rendered after Usage/Flags):
//   - `## Response 2xx`       — success response field names/types
//   - `## Responses 2xx`      — merged success schemas
//
// Kept in `above`:
//   - `## Input Example`      — concrete example body
//   - `## Request Schema`     — body field shapes not in Cobra Flags
//   - Every error response (`default`, 4xx, 5xx) — recovery guidance such as
//     `recommended_action: use_async_job` belongs next to the call, not after
//     the flags
//   - User-authored prose (`## Rules`, `## Example`, ...)
//
// See docs/CLI_DESIGN_PRINCIPLES.md §3.5 and
// docs/CLI_V1_ALPHA_26_REVIEW.md item #5.
func splitHelpBlocks(long string) (above, below string) {
	if long == "" {
		return "", ""
	}
	lines := strings.Split(long, "\n")
	var aboveOut, belowOut []string
	inFence := false
	mode := 0 // 0 = above, 1 = dropping, 2 = below

	for _, line := range lines {
		// Track fenced code blocks so a literal `## Response` inside a
		// user-authored code example is never mistaken for a section
		// boundary. (Not strictly needed for today's spec, but cheap
		// insurance against future prose.)
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			appendTo(mode, line, &aboveOut, &belowOut)
			continue
		}

		if !inFence && strings.HasPrefix(line, "## ") {
			switch {
			case isDroppedSchemaHeading(line):
				mode = 1
				continue
			case isBelowSchemaHeading(line):
				mode = 2
			default:
				mode = 0
			}
		}

		appendTo(mode, line, &aboveOut, &belowOut)
	}

	return strings.TrimRight(strings.Join(aboveOut, "\n"), "\n"),
		compactResponseSchemas(strings.TrimRight(strings.Join(belowOut, "\n"), "\n"))
}

// metaFieldsKept are the response meta fields that change what an agent does
// next: whether an empty result is genuine, whether there is more to page
// through, and how many rows matched. Cached/credits/limit/offset/watermark
// are diagnostics, not decisions, and cost tokens on every --help.
var metaFieldsKept = map[string]bool{
	"empty_reason": true,
	"has_more":     true,
	"next_cursor":  true,
	"total":        true,
}

// fieldDescriptionsKept keep their doc comment in the compact view because
// they explain how to chain this endpoint to another one.
var fieldDescriptionsKept = map[string]bool{
	"id":         true,
	"project_id": true,
}

// compactResponseSchemas rewrites each ```schema fence in the response block:
// $schema is dropped, meta is pruned to the decision fields, and field doc
// comments are capped. The full, unabridged descriptions remain in the
// published OpenAPI spec.
func compactResponseSchemas(block string) string {
	if block == "" {
		return ""
	}
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines))
	inSchema := false
	type frame struct {
		key     string
		keep    bool
		opener  string
		emitted bool
	}
	var stack []frame

	// A kept meta child emits the meta opener lazily, so a meta object whose
	// fields were all pruned disappears instead of rendering `meta*: {}`.
	flushOpeners := func() {
		for i := range stack {
			if stack[i].keep && !stack[i].emitted {
				out = append(out, stack[i].opener)
				stack[i].emitted = true
			}
		}
	}
	keepCurrent := func() bool {
		if len(stack) == 0 {
			return true
		}
		return stack[len(stack)-1].keep
	}
	currentKey := func() string {
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1].key
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inSchema = !inSchema
			stack = nil
			out = append(out, line)
			continue
		}
		if !inSchema {
			out = append(out, line)
			continue
		}

		if isSchemaClose(trimmed) {
			if len(stack) > 0 {
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if f.emitted {
					out = append(out, line)
				}
			} else {
				out = append(out, line)
			}
			continue
		}

		key, _, isProperty := parseSchemaLine(trimmed)
		opens := isSchemaOpenLine(trimmed)

		keep := keepCurrent()
		if isProperty {
			switch {
			case currentKey() == "meta" && (len(stack) == 1 || stack[len(stack)-2].key == ""):
				// Direct children of the envelope's meta only; a business field
				// named "meta" inside data is ordinary data.
				keep = keep && metaFieldsKept[key]
			case currentKey() == "" && key == "$schema":
				keep = false
			}
		}

		if opens {
			fk := key
			if !isProperty {
				fk = currentKey()
			}
			f := frame{key: fk, keep: keep, opener: line}
			if keep && fk != "meta" {
				flushOpeners()
				out = append(out, line)
				f.emitted = true
			}
			stack = append(stack, f)
			continue
		}

		if !keep {
			continue
		}
		flushOpeners()
		if isProperty && !fieldDescriptionsKept[key] {
			out = append(out, truncateSchemaDescription(line))
		} else {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// isSchemaOpenLine reports whether a line opens a block. The brace or bracket
// must follow the `key: ` separator: a doc comment that happens to end in `{`
// is prose, not structure.
func isSchemaOpenLine(trimmed string) bool {
	if trimmed == "{" || trimmed == "[" || strings.HasSuffix(trimmed, "Of{") {
		return true
	}
	i := strings.Index(trimmed, ": ")
	if i < 0 {
		return false
	}
	rest := strings.TrimSpace(trimmed[i+2:])
	// Exactly "{" or "[" opens a block; "[<any>]" and "[<recursive ref>]" are
	// complete one-line values, not openers.
	return rest == "{" || rest == "[" || strings.HasSuffix(rest, "Of{")
}

// parseSchemaLine splits "key*: (type) desc" into its key, value and whether
// the line looks like a schema property at all.
func parseSchemaLine(trimmed string) (key, value string, ok bool) {
	colon := strings.Index(trimmed, ": ")
	if colon <= 0 {
		return "", "", false
	}
	key = strings.TrimSuffix(trimmed[:colon], "*")
	for _, r := range key {
		if !(r == '_' || r == '-' || r == '$' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return "", "", false
		}
	}
	return key, trimmed[colon+2:], true
}

func isSchemaClose(trimmed string) bool {
	return trimmed == "}" || trimmed == "]" || trimmed == "},"
}

// maxDocRunes caps a response field's doc comment in --help. Units and
// precision live in the first few words, so a hard cap keeps them while
// dropping long rationale.
const maxDocRunes = 160

// truncateSchemaDescription keeps the key, required marker, type and a capped
// doc comment. It cuts at a word boundary, never splits a UTF-8 rune, and
// extends to close a parenthesis rather than leaving it dangling.
func truncateSchemaDescription(line string) string {
	trimmed := strings.TrimSpace(line)
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	colon := strings.Index(trimmed, ": ")
	if colon <= 0 {
		return line
	}
	prefix := trimmed[:colon+2]
	rest := trimmed[colon+2:]
	doc, typeEnd, ok := splitTypeAnnotation(rest)
	if !ok {
		return line
	}
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return indent + prefix + rest[:typeEnd]
	}
	return indent + prefix + rest[:typeEnd] + " " + capDoc(doc, maxDocRunes)
}

// splitTypeAnnotation returns the doc text following a leading "(type)" and
// the byte offset just past that type annotation.
func splitTypeAnnotation(rest string) (doc string, typeEnd int, ok bool) {
	if !strings.HasPrefix(rest, "(") {
		return "", 0, false
	}
	depth := 0
	for i, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return rest[i+1:], i + 1, true
			}
		}
	}
	return "", 0, false
}

// capDoc shortens prose at a word boundary. It deliberately does not split on
// sentence periods: abbreviations such as "e.g." would leave dangling
// fragments and drop the unit that follows.
func capDoc(doc string, limit int) string {
	runes := []rune(doc)
	if len(runes) <= limit {
		return doc
	}
	cut := limit

	// A cut inside parentheses leaves unbalanced output; extend to the close.
	// Once extended, leave the cut alone: backing off to a word boundary would
	// move it back inside the parentheses and undo the balance.
	extended := false
	if depth := parenDepth(runes[:cut]); depth > 0 {
		for i := cut; i < len(runes); i++ {
			switch runes[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				cut = i + 1
				extended = true
				break
			}
		}
	}

	if !extended {
		for i := cut - 1; i > limit/2; i-- {
			if unicode.IsSpace(runes[i]) {
				if parenDepth(runes[:i]) == 0 {
					cut = i
				}
				break
			}
		}
	}
	return strings.TrimRight(string(runes[:cut]), " ,;:") + "…"
}

func parenDepth(runes []rune) int {
	depth := 0
	for _, r := range runes {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	return depth
}

func appendTo(mode int, line string, above, below *[]string) {
	switch mode {
	case 0:
		*above = append(*above, line)
	case 2:
		*below = append(*below, line)
	}
}

// isDroppedSchemaHeading reports whether a `## …` heading introduces one
// of the auto-generated schema dumps we want to suppress from --help.
//
// Keep in sync with the explicit `desc += "\n## …"` statements in
// openapi/openapi.go that emit these headings.
func isDroppedSchemaHeading(line string) bool {
	h := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	switch {
	case strings.HasPrefix(h, "Argument Schema"),
		strings.HasPrefix(h, "Option Schema"):
		return true
	}
	return false
}

// isBelowSchemaHeading reports whether a `## …` heading should be rendered
// after Usage/Flags. Only 2xx responses move: error responses carry recovery
// guidance and stay next to the prose that explains how to call the command.
func isBelowSchemaHeading(line string) bool {
	h := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	var codes string
	switch {
	case strings.HasPrefix(h, "Responses "):
		codes = strings.TrimPrefix(h, "Responses ")
	case strings.HasPrefix(h, "Response "):
		codes = strings.TrimPrefix(h, "Response ")
	default:
		return false
	}
	if i := strings.Index(codes, " "); i >= 0 {
		codes = codes[:i]
	}
	for _, code := range strings.Split(codes, "/") {
		n, err := strconv.Atoi(code)
		if err != nil || n < 200 || n >= 300 {
			return false
		}
	}
	return true
}
