package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestHelpPutsFlagsBeforeResponseSchema is the integration counterpart to
// TestSplitHelpBlocks: it asserts the ordering users and agents actually see
// in the final --help output, not just the helper's return values.
func TestHelpPutsFlagsBeforeResponseSchema(t *testing.T) {
	bin := buildSurfBin(t)

	tests := []struct {
		op        string
		wantFlag  string
		wantAbove string // section that must stay above Usage for body-type commands
	}{
		{op: "news-feed", wantFlag: "--sort-by"},
		{op: "market-price", wantFlag: "--symbol"},
		{op: "onchain-sql", wantFlag: "--idempotency-key", wantAbove: "## Input Example"},
	}

	for _, tc := range tests {
		t.Run(tc.op, func(t *testing.T) {
			out, err := runHelp(t, bin, tc.op)
			if err != nil {
				t.Skipf("%s not in the cached spec: %v", tc.op, err)
			}

			usage := strings.Index(out, "\nUsage:")
			flags := strings.Index(out, "\nFlags:")
			responses := strings.Index(out, "Format of the response:")
			responseSchema := strings.Index(out, "## Response 200")

			if usage < 0 || flags < 0 {
				t.Fatalf("%s --help has no Usage/Flags section:\n%s", tc.op, out)
			}
			if responseSchema < 0 {
				t.Fatalf("%s --help has no success-response block:\n%s", tc.op, out)
			}

			// Flags must come before the response schema, and the response
			// block must come after the flags.
			if flags > responseSchema {
				t.Errorf("%s --help puts Flags after the response schema (flags@%d, response@%d)", tc.op, flags, responseSchema)
			}
			if responses < 0 || responses < flags {
				t.Errorf("%s --help is missing the response block after Flags:\n%s", tc.op, out)
			}

			// The flag an agent needs must be visible before the schema dump.
			flagPos := strings.Index(out, tc.wantFlag)
			if flagPos < 0 {
				t.Fatalf("%s --help is missing %s", tc.op, tc.wantFlag)
			}
			if flagPos > responses {
				t.Errorf("%s: %s appears at %d, after the response block at %d", tc.op, tc.wantFlag, flagPos, responses)
			}

			if tc.wantAbove != "" {
				above := strings.Index(out, tc.wantAbove)
				if above < 0 || above > usage {
					t.Errorf("%s: %q must stay above Usage (found at %d, usage at %d)", tc.op, tc.wantAbove, above, usage)
				}
			}
		})
	}
}

func runHelp(t *testing.T, bin, op string) (string, error) {
	t.Helper()
	// Inherit the real HOME: the cached spec lives in ~/.surf and --help makes
	// no network calls, so no request server is needed here.
	out, err := exec.Command(bin, op, "--help").CombinedOutput()
	return string(out), err
}

// runSurfCapture runs surf with an isolated HOME and returns combined output
// plus the process error, so tests can assert on failures too.
func runSurfCapture(t *testing.T, bin, home string, args ...string) (string, error) {
	t.Helper()
	out, err := surfCmd(t, bin, home, nil, args...).CombinedOutput()
	return string(out), err
}

// TestEmptyResultHintIntegration drives a real empty response end to end and
// checks each output format: readable explains itself, structured formats are
// left byte-for-byte intact, and table renders instead of panicking.
func TestEmptyResultHintIntegration(t *testing.T) {
	bin := buildBareSurfBin(t)

	serve := func(t *testing.T, body string) (home string, base string) {
		t.Helper()
		srv, _ := newSpecAndOperationServer(t, "emptyResult", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		})
		t.Cleanup(srv.Close)
		home = t.TempDir()
		seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")
		return home, srv.URL + "/gateway/v1"
	}

	emptyBody := `{"data":[],"meta":{"total":0,"empty_reason":"nothing matched the filter"}}`

	t.Run("readable explains the empty result", func(t *testing.T) {
		home, base := serve(t, emptyBody)
		out, err := runSurfCapture(t, bin, home, "empty-result", "--address", "BTC",
			"--surf-api-base-url", base, "-o", "readable")
		if err != nil {
			t.Fatalf("command failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "(no data)") {
			t.Errorf("readable output should explain the empty result:\n%s", out)
		}
		if !strings.Contains(out, "empty_reason: nothing matched the filter") {
			t.Errorf("readable output should surface meta.empty_reason:\n%s", out)
		}
		if strings.Contains(out, "HTTP/2.0") {
			t.Errorf("readable output should replace the envelope dump, not follow it:\n%s", out)
		}
	})

	t.Run("json keeps the envelope", func(t *testing.T) {
		home, base := serve(t, emptyBody)
		out, err := runSurfCapture(t, bin, home, "empty-result", "--address", "BTC",
			"--surf-api-base-url", base, "--json")
		if err != nil {
			t.Fatalf("command failed: %v\n%s", err, out)
		}
		var env struct {
			Data []any          `json:"data"`
			Meta map[string]any `json:"meta"`
		}
		if jerr := json.Unmarshal([]byte(out), &env); jerr != nil {
			t.Fatalf("--json output is not valid JSON: %v\n%s", jerr, out)
		}
		if env.Meta["empty_reason"] != "nothing matched the filter" {
			t.Errorf("--json should pass meta.empty_reason through unchanged:\n%s", out)
		}
		if strings.Contains(out, "(no data)") {
			t.Errorf("--json must not contain the readable hint:\n%s", out)
		}
	})

	t.Run("table renders empty result", func(t *testing.T) {
		home, base := serve(t, emptyBody)
		out, err := runSurfCapture(t, bin, home, "empty-result", "--address", "BTC",
			"--surf-api-base-url", base, "-o", "table")
		if err != nil {
			t.Fatalf("table output should not fail on an empty array: %v\n%s", err, out)
		}
		if !strings.Contains(out, "(no rows)") {
			t.Errorf("table output should say (no rows):\n%s", out)
		}
	})

	t.Run("non-empty result is untouched", func(t *testing.T) {
		home, base := serve(t, `{"data":[{"address":"BTC"}],"meta":{"total":1}}`)
		out, err := runSurfCapture(t, bin, home, "empty-result", "--address", "BTC",
			"--surf-api-base-url", base, "-o", "readable")
		if err != nil {
			t.Fatalf("command failed: %v\n%s", err, out)
		}
		if strings.Contains(out, "(no data)") {
			t.Errorf("non-empty result must not get the empty hint:\n%s", out)
		}
		if !strings.Contains(out, "BTC") {
			t.Errorf("non-empty result should render its rows:\n%s", out)
		}
	})
}
