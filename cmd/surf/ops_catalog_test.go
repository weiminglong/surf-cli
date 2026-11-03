package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asksurf-ai/surf-cli/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func catalogFixture() []cli.Operation {
	return []cli.Operation{
		{
			Name:   "news-feed",
			Method: "GET",
			Group:  "news",
			Short:  "Crypto news",
			QueryParams: []*cli.Param{
				{Name: "sort_by", Type: "string", Enum: []string{"recency", "trending"}, Description: "Sort order"},
				{
					Name:        "limit",
					Type:        "integer",
					Minimum:     fptr(1),
					Maximum:     fptr(50),
					Default:     float64(20),
					Description: "Results per page",
				},
			},
		},
		{
			Name:   "search-token",
			Method: "GET",
			Short:  "Search tokens by symbol",
			QueryParams: []*cli.Param{
				{Name: "q", Type: "string", Required: true, Description: "Query"},
			},
		},
		{
			Name:        "job-get",
			Method:      "GET",
			Short:       "Get a job",
			URITemplate: "https://example.com/jobs/{job_id}",
			PathParams: []*cli.Param{
				{Name: "job_id", Type: "string", Required: true, Description: "Job identifier"},
			},
		},
	}
}

func fptr(f float64) *float64 { return &f }

func decodeCatalog(t *testing.T, ops []cli.Operation, compact bool) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := catalogOut
	catalogOut = &buf
	t.Cleanup(func() { catalogOut = old })

	require.NoError(t, printOperationsJSON(ops, compact))
	var out []map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	return out
}

func TestOperationsJSONIncludesEnumsAndBounds(t *testing.T) {
	out := decodeCatalog(t, catalogFixture(), false)
	require.Len(t, out, 3)

	sortBy := flagByName(t, out[0]["optional_flags"], "sort-by")
	assert.Equal(t, []any{"recency", "trending"}, sortBy["enum"])

	limit := flagByName(t, out[0]["optional_flags"], "limit")
	assert.Equal(t, float64(1), limit["min"])
	assert.Equal(t, float64(50), limit["max"])
	assert.Equal(t, float64(20), limit["default"])

	required := out[1]["required_flags"].([]any)
	require.Len(t, required, 1)
	assert.Equal(t, "q", required[0].(map[string]any)["name"])
}

// TestOperationsJSONPathParamsArePositional guards against advertising path
// parameters as flags: `surf job-get --job-id 1` does not exist.
func TestOperationsJSONPathParamsArePositional(t *testing.T) {
	out := decodeCatalog(t, catalogFixture(), false)
	job := out[2]

	assert.NotContains(t, job, "path_flags")

	args, ok := job["positional_args"].([]any)
	require.True(t, ok, "positional args should be listed separately: %v", job)
	require.Len(t, args, 1)

	arg := args[0].(map[string]any)
	assert.Equal(t, "job_id", arg["name"])
	assert.Equal(t, float64(1), arg["position"])
	assert.Equal(t, true, arg["required"])
}

func TestOperationsJSONCompactDropsDescriptions(t *testing.T) {
	full := decodeCatalog(t, catalogFixture(), false)
	compact := decodeCatalog(t, catalogFixture(), true)

	assert.Contains(t, full[0], "short")
	assert.NotContains(t, compact[0], "short", "compact drops the operation summary")

	fullFlag := flagByName(t, full[0]["optional_flags"], "limit")
	assert.Contains(t, fullFlag, "description")
	assert.Equal(t, []any{"recency", "trending"},
		flagByName(t, full[0]["optional_flags"], "sort-by")["enum"],
		"enum survives in full mode")

	compactFlag := flagByName(t, compact[0]["optional_flags"], "limit")
	assert.NotContains(t, compactFlag, "description")
	assert.Equal(t, float64(50), compactFlag["max"], "bounds survive compaction")
	assert.Equal(t, []any{"recency", "trending"},
		flagByName(t, compact[0]["optional_flags"], "sort-by")["enum"],
		"enum survives compaction")
}

func TestOperationsJSONCompactIsSmaller(t *testing.T) {
	var full, compact bytes.Buffer
	old := catalogOut
	t.Cleanup(func() { catalogOut = old })

	catalogOut = &full
	require.NoError(t, printOperationsJSON(catalogFixture(), false))
	catalogOut = &compact
	require.NoError(t, printOperationsJSON(catalogFixture(), true))

	assert.Less(t, compact.Len(), full.Len())
	assert.Contains(t, compact.String(), `"enum"`)
	// Compact output is meant to be pasted into a prompt, so it carries no
	// pretty-printing whitespace.
	assert.NotContains(t, compact.String(), "\n  ")
	assert.Equal(t, 1, strings.Count(compact.String(), "\n"))
}

func flagByName(t *testing.T, flags any, name string) map[string]any {
	t.Helper()
	list, ok := flags.([]any)
	require.True(t, ok, "flags should be an array")
	for _, f := range list {
		m := f.(map[string]any)
		if m["name"] == name {
			return m
		}
	}
	t.Fatalf("flag %q not found", name)
	return nil
}
