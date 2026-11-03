package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestStripSchemaBlocks(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantOut string
		// Substrings that must NOT appear in the output.
		mustNotContain []string
		// Substrings that MUST appear in the output.
		mustContain []string
	}{
		{
			name:    "empty",
			in:      "",
			wantOut: "",
		},
		{
			name:    "prose only",
			in:      "Returns historical price data.\n\nUse --time-range or --from/--to.",
			wantOut: "Returns historical price data.\n\nUse --time-range or --from/--to.",
		},
		{
			name: "strips option schema",
			in: strings.Join([]string{
				"Returns historical price data.",
				"",
				"## Option Schema:",
				"```schema",
				"{",
				"  --symbol: (string) Token symbol",
				"}",
				"```",
			}, "\n"),
			mustNotContain: []string{"Option Schema", "--symbol: (string)"},
			mustContain:    []string{"Returns historical price data."},
		},
		{
			name: "keeps response blocks (unique return-shape info)",
			in: strings.Join([]string{
				"Token price history.",
				"",
				"## Response 200 (application/json)",
				"",
				"OK",
				"",
				"```schema",
				"{ data*: [...] }",
				"```",
				"",
				"## Response default (application/json)",
				"",
				"Error",
				"",
				"```schema",
				"{ error*: {...} }",
				"```",
			}, "\n"),
			mustContain: []string{"Token price history.", "Response 200", "Response default", "data*:", "error*:"},
		},
		{
			name: "strips argument schema",
			in: strings.Join([]string{
				"Get a single item.",
				"",
				"## Argument Schema:",
				"```schema",
				"{",
				"  item-id: (string) Item ID",
				"}",
				"```",
			}, "\n"),
			mustNotContain: []string{"Argument Schema", "item-id:"},
			mustContain:    []string{"Get a single item."},
		},
		{
			name: "keeps input example and response",
			in: strings.Join([]string{
				"Execute SQL.",
				"",
				"## Input Example",
				"",
				"```json",
				`{"sql": "SELECT 1"}`,
				"```",
				"",
				"## Response 200 (application/json)",
				"```schema",
				"{ data*: [...] }",
				"```",
			}, "\n"),
			mustContain: []string{"Execute SQL.", "Input Example", `"sql": "SELECT 1"`, "Response 200", "data*:"},
		},
		{
			name: "keeps request schema and response",
			in: strings.Join([]string{
				"Execute SQL.",
				"",
				"## Request Schema (application/json)",
				"",
				"```schema",
				"{",
				"  sql*: (string) SQL query",
				"}",
				"```",
				"",
				"## Response 200 (application/json)",
				"```schema",
				"{ data*: [...] }",
				"```",
			}, "\n"),
			mustContain: []string{"Request Schema", "sql*: (string) SQL query", "Response 200", "data*:"},
		},
		{
			name: "keeps user-authored sections",
			in: strings.Join([]string{
				"SQL executor.",
				"",
				"## Rules",
				"- Only SELECT allowed",
				"- Max 10,000 rows",
				"",
				"## Example",
				"",
				"```sql",
				"SELECT 1",
				"```",
				"",
				"## Option Schema:",
				"```schema",
				"{ --sql: string }",
				"```",
			}, "\n"),
			mustContain:    []string{"## Rules", "Only SELECT allowed", "## Example", "SELECT 1"},
			mustNotContain: []string{"Option Schema", "--sql: string"},
		},
		{
			name: "interleaved sections — strips Option Schema, keeps Response",
			in: strings.Join([]string{
				"Op description.",
				"",
				"## Option Schema:",
				"```schema",
				"{ --x: string }",
				"```",
				"",
				"## Rules",
				"Must use --x.",
				"",
				"## Response 200 (application/json)",
				"```schema",
				"{ data: [] }",
				"```",
			}, "\n"),
			mustContain:    []string{"Op description.", "## Rules", "Must use --x.", "Response 200", "{ data: [] }"},
			mustNotContain: []string{"Option Schema", "--x: string"},
		},
		{
			name: "code block containing a literal ## is not mistaken for a heading",
			in: strings.Join([]string{
				"Example.",
				"",
				"```bash",
				"## This is a bash comment, not a heading",
				"echo hi",
				"```",
				"",
				"## Option Schema:",
				"```schema",
				"{ --x: string }",
				"```",
			}, "\n"),
			mustContain:    []string{"## This is a bash comment", "echo hi"},
			mustNotContain: []string{"Option Schema", "--x: string"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			above, below := splitHelpBlocks(tt.in)
			got := above
			if below != "" {
				got = strings.TrimRight(above+"\n\n"+below, "\n")
			}
			if tt.wantOut != "" {
				assert.Equal(t, tt.wantOut, got)
			}
			for _, s := range tt.mustContain {
				assert.Contains(t, got, s, "expected output to contain %q\n\noutput:\n%s", s, got)
			}
			for _, s := range tt.mustNotContain {
				assert.NotContains(t, got, s, "expected output NOT to contain %q\n\noutput:\n%s", s, got)
			}
		})
	}
}

func TestIsDroppedSchemaHeading(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"## Option Schema:", true},
		{"## Option Schema", true},
		{"## Argument Schema:", true},
		{"## Response 200 (application/json)", false},      // kept — unique return-shape info
		{"## Response default (application/json)", false},  // kept
		{"## Responses 200/201 (application/json)", false}, // kept
		{"## Request Schema (application/json)", false},    // kept
		{"## Input Example", false},                        // kept
		{"## Rules", false},
		{"## Example", false},
		{"## Time range options", false},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			assert.Equal(t, tt.want, isDroppedSchemaHeading(tt.line))
		})
	}
}

func TestSplitHelpBlocks(t *testing.T) {
	in := strings.Join([]string{
		"Returns crypto news.",
		"",
		"## Input Example",
		"",
		"```json",
		`{"q": "x"}`,
		"```",
		"",
		"## Response 200 (application/json)",
		"",
		"```schema",
		"{",
		"  $schema: (string format:uri) A URL to the JSON Schema for this object.",
		"  data*: [",
		"    {",
		"      id*: (string) Article ID. Use with the detail endpoint",
		"      title*: (string) Article headline",
		"    }",
		"  ]",
		"  meta*: {",
		"    cached*: (boolean) Whether cached",
		"    credits_used*: (integer) Credits deducted",
		"    empty_reason: (string) Hint explaining why empty",
		"    total: (integer) Total matching items",
		"    watermark: (integer) Warehouse watermark",
		"  }",
		"}",
		"```",
		"",
		"## Response default (application/json)",
		"",
		"```schema",
		"{",
		"  error*: {",
		"    code*: (string)",
		"  }",
		"}",
		"```",
	}, "\n")

	above, below := splitHelpBlocks(in)

	assert.Contains(t, above, "Returns crypto news.")
	assert.Contains(t, above, "## Input Example")
	assert.Contains(t, above, "## Response default", "error envelope stays with the prose")
	assert.NotContains(t, above, "## Response 200")

	assert.Contains(t, below, "## Response 200")
	assert.NotContains(t, below, "## Response default", "error response must not move")

	// Compacted: decision fields survive, noise goes.
	assert.Contains(t, below, "empty_reason:")
	assert.Contains(t, below, "total:")
	assert.NotContains(t, below, "cached*:")
	assert.NotContains(t, below, "credits_used*:")
	assert.NotContains(t, below, "watermark:")
	assert.NotContains(t, below, "$schema")

	// Chaining hints and first-sentence docs survive; the long tail does not.
	assert.Contains(t, below, "id*: (string) Article ID.")
	assert.Contains(t, below, "title*: (string) Article headline")
}

func TestSplitHelpBlocks_Empty(t *testing.T) {
	above, below := splitHelpBlocks("")
	assert.Empty(t, above)
	assert.Empty(t, below)
}

func TestIsBelowSchemaHeading(t *testing.T) {
	assert.True(t, isBelowSchemaHeading("## Response 200 (application/json)"))
	assert.True(t, isBelowSchemaHeading("## Responses 200/201 (application/json)"))
	assert.False(t, isBelowSchemaHeading("## Response default (application/json)"))
	assert.False(t, isBelowSchemaHeading("## Response 408 (application/json)"),
		"error responses keep their recovery guidance above the flags")
	assert.False(t, isBelowSchemaHeading("## Response 429 (application/json)"))
	assert.False(t, isBelowSchemaHeading("## Responses 200/409 (application/json)"),
		"a merged heading with any non-2xx code stays above")
	assert.True(t, isBelowSchemaHeading("## Responses 200/201 (application/json)"))
	assert.False(t, isBelowSchemaHeading("## Input Example"))
	assert.False(t, isBelowSchemaHeading("## Request Schema (application/json)"))
}

// TestCompactResponseSchemas_DropsNestedMetaSubtree verifies that dropping a
// meta field also drops the block it opens, so its children and closing brace
// do not leak into the parent scope.
func TestCompactResponseSchemas_DropsNestedMetaSubtree(t *testing.T) {
	block := strings.Join([]string{
		"## Response 200 (application/json)",
		"",
		"```schema",
		"{",
		"  data*: [",
		"    {",
		"      id*: (string) Article ID",
		"    }",
		"  ]",
		"  meta*: {",
		"    pagination: {",
		"      total: (integer) nested total",
		"      limit: (integer) nested limit",
		"    }",
		"    total: (integer) Total matching items",
		"    cached*: (boolean) Served from cache",
		"  }",
		"}",
		"```",
	}, "\n")

	got := compactResponseSchemas(block)
	assert.NotContains(t, got, "pagination")
	assert.NotContains(t, got, "nested total")
	assert.NotContains(t, got, "nested limit")
	assert.Contains(t, got, "total: (integer) Total matching items")
	assert.NotContains(t, got, "Served from cache")

	// Braces must still balance after the subtree is removed.
	assert.Equal(t, strings.Count(got, "{"), strings.Count(got, "}"), got)
}

// TestCompactResponseSchemas_CapsLongDocs verifies that a long doc keeps its
// beginning (where units live) and drops the tail, without splitting on
// sentence periods.
func TestCompactResponseSchemas_CapsLongDocs(t *testing.T) {
	long := "Decimal-adjusted transfer amount as a string. " + strings.Repeat("extra rationale ", 20)
	block := strings.Join([]string{
		"```schema",
		"{",
		"  data*: [",
		"    {",
		"      amount*: (string) " + long,
		"    }",
		"  ]",
		"}",
		"```",
	}, "\n")

	got := compactResponseSchemas(block)
	assert.Contains(t, got, "Decimal-adjusted transfer amount as a string.")
	assert.NotContains(t, got, strings.Repeat("extra rationale", 10))
	assert.Contains(t, got, "…")
}

// TestCompactResponseSchemas_KeepsAbbreviations guards against splitting prose
// on periods inside abbreviations: "e.g." used to leave a dangling fragment and
// drop the unit or example that followed.
func TestCompactResponseSchemas_KeepsAbbreviations(t *testing.T) {
	block := strings.Join([]string{
		"```schema",
		"{",
		"  data*: [",
		"    {",
		"      name*: (string) Token name, e.g. Tesla xStock",
		"      distance: (number) Fraction the mark must move (e.g. 0.05 = 5%), always positive",
		"    }",
		"  ]",
		"}",
		"```",
	}, "\n")

	got := compactResponseSchemas(block)
	assert.Contains(t, got, "Token name, e.g. Tesla xStock")
	assert.Contains(t, got, "(e.g. 0.05 = 5%)")
	assert.NotContains(t, got, "e.g.\n")
}

// TestCompactResponseSchemas_DropsEmptyMeta verifies that a meta object whose
// fields were all pruned disappears instead of rendering `meta*: {}`.
func TestCompactResponseSchemas_DropsEmptyMeta(t *testing.T) {
	block := strings.Join([]string{
		"```schema",
		"{",
		"  data*: [",
		"    {",
		"      id*: (string) Identifier",
		"    }",
		"  ]",
		"  meta*: {",
		"    cached*: (boolean) Served from cache",
		"  }",
		"}",
		"```",
	}, "\n")

	got := compactResponseSchemas(block)
	assert.NotContains(t, got, "meta")
	assert.Equal(t, strings.Count(got, "{"), strings.Count(got, "}"), got)
}

// TestCapDocUTF8 ensures truncation never splits a rune.
func TestCapDocUTF8(t *testing.T) {
	doc := strings.Repeat("区", 300)
	got := capDoc(doc, 160)
	assert.True(t, utf8.ValidString(got), "truncated doc must remain valid UTF-8")
	assert.LessOrEqual(t, len([]rune(got)), 161) // 160 runes + ellipsis
}

func TestIsSchemaOpenLine(t *testing.T) {
	assert.True(t, isSchemaOpenLine("{"))
	assert.True(t, isSchemaOpenLine("["))
	assert.True(t, isSchemaOpenLine("data*: ["))
	assert.True(t, isSchemaOpenLine("pricing: {"))
	assert.True(t, isSchemaOpenLine("<any>: ["))
	assert.False(t, isSchemaOpenLine("foo: (string) see docs {"),
		"a brace inside a description is prose, not structure")
	assert.False(t, isSchemaOpenLine("total: (integer) 3"))
}
