package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helpOrderSpecServer serves one operation with a body, a success response and
// error responses, so the help layout can be asserted without depending on the
// live spec (which CI machines do not have cached).
func helpOrderSpecServer(t *testing.T) *httptest.Server {
	t.Helper()
	spec := `{
  "openapi": "3.0.0",
  "info": {"title": "Surf Test API", "version": "1.0.0"},
  "paths": {
    "/gateway/v1/helpOrder": {
      "post": {
        "operationId": "helpOrder",
        "summary": "Help order fixture",
        "description": "Prose that belongs at the top.",
        "tags": ["test"],
        "parameters": [
          {"name": "address", "in": "query", "schema": {"type": "string"}}
        ],
        "requestBody": {
          "content": {
            "application/json": {
              "schema": {"type": "object", "properties": {"sql": {"type": "string"}}},
              "example": {"sql": "SELECT 1"}
            }
          }
        },
        "responses": {
          "200": {
            "description": "OK",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "data": {
                      "type": "array",
                      "items": {"type": "object", "properties": {"id": {"type": "string", "description": "Article ID"}}}
                    },
                    "meta": {
                      "type": "object",
                      "properties": {
                        "cached": {"type": "boolean", "description": "Served from cache"},
                        "empty_reason": {"type": "string", "description": "Why empty"}
                      }
                    }
                  }
                }
              }
            }
          },
          "408": {
            "description": "Timed out",
            "content": {
              "application/json": {
                "schema": {"type": "object", "properties": {"recommended_action": {"type": "string"}}}
              }
            }
          },
          "default": {
            "description": "Error",
            "content": {
              "application/json": {
                "schema": {"type": "object", "properties": {"error": {"type": "object", "properties": {"code": {"type": "string"}}}}}
              }
            }
          }
        }
      }
    }
  }
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/openapi.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, spec)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHelpPutsFlagsBeforeResponseSchema is the integration counterpart to
// TestSplitHelpBlocks: it asserts the ordering users and agents actually see
// in the final --help output, using a stub spec so it runs in CI too.
func TestHelpPutsFlagsBeforeResponseSchema(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := helpOrderSpecServer(t)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	// Help is written to stderr so stdout stays parseable.
	_, out, err := runCapture(t, bin, home, "help-order", "--help")
	if err != nil {
		t.Fatalf("--help failed: %v\n%s", err, out)
	}

	usage := strings.Index(out, "\nUsage:")
	flags := strings.Index(out, "\nFlags:")
	responseBlock := strings.Index(out, "Format of the response:")
	response200 := strings.Index(out, "## Response 200")
	response408 := strings.Index(out, "## Response 408")
	responseDefault := strings.Index(out, "## Response default")
	inputExample := strings.Index(out, "## Input Example")

	if usage < 0 || flags < 0 {
		t.Fatalf("--help has no Usage/Flags section:\n%s", out)
	}
	if response200 < 0 || responseBlock < 0 {
		t.Fatalf("--help has no success-response block:\n%s", out)
	}

	// Flags come before the success schema, which comes after them.
	if flags > response200 {
		t.Errorf("Flags rendered after the response schema (flags@%d, response@%d):\n%s", flags, response200, out)
	}
	if responseBlock < flags {
		t.Errorf("response block should follow Flags (block@%d, flags@%d):\n%s", responseBlock, flags, out)
	}

	// The flag needed to construct the call is visible before the schema.
	if pos := strings.Index(out, "--address"); pos < 0 || pos > responseBlock {
		t.Errorf("--address should appear before the response block (pos=%d, block=%d):\n%s", pos, responseBlock, out)
	}

	// Body-type input example stays above Usage.
	if inputExample < 0 || inputExample > usage {
		t.Errorf("## Input Example must stay above Usage (pos=%d, usage=%d):\n%s", inputExample, usage, out)
	}

	// Error responses stay above Usage with the prose that explains recovery.
	for name, pos := range map[string]int{"default": responseDefault, "408": response408} {
		if pos < 0 {
			t.Errorf("## Response %s missing from --help:\n%s", name, out)
			continue
		}
		if pos > usage {
			t.Errorf("## Response %s should stay above Usage (pos=%d, usage=%d)", name, pos, usage)
		}
	}
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
		out, _, err := runCapture(t, bin, home, "empty-result", "--address", "BTC",
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
		out, errOut, err := runCapture(t, bin, home, "empty-result", "--address", "BTC",
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
		if strings.Contains(errOut, "(no data)") {
			t.Errorf("--json should not add side-channel text to stderr:\n%s", errOut)
		}
	})

	t.Run("table renders empty result", func(t *testing.T) {
		home, base := serve(t, emptyBody)
		out, _, err := runCapture(t, bin, home, "empty-result", "--address", "BTC",
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
		out, _, err := runCapture(t, bin, home, "empty-result", "--address", "BTC",
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
