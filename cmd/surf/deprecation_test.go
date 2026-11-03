package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runCapture runs surf under an isolated HOME and returns stdout, stderr and
// the process error, so a test can assert on both streams and failure at once.
func runCapture(t *testing.T, bin, home string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := surfCmd(t, bin, home, nil, args...)
	so, se := splitOutput(cmd)
	err = cmd.Run()
	return so.String(), se.String(), err
}

// stubSpecServer serves a one-operation OpenAPI spec. Using a stub instead of
// the live cached spec keeps these tests deterministic: the deprecated and
// completion paths must work regardless of what the current spec contains.
func stubSpecServer(t *testing.T, operationID string, deprecated bool) *httptest.Server {
	t.Helper()
	dep := ""
	if deprecated {
		dep = `"deprecated": true,`
	}
	spec := fmt.Sprintf(`{
  "openapi": "3.0.0",
  "info": {"title": "Surf Test API", "version": "1.0.0"},
  "paths": {
    "/gateway/v1/%[1]s": {
      "get": {
        "operationId": "%[1]s",
        "summary": "%[1]s summary",
        "tags": ["test"],
        %[2]s
        "parameters": [
          {"name": "address", "in": "query", "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}`, operationID, dep)

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

// TestDeprecationWarningGoesToStderr verifies that Cobra's deprecation
// message goes to stderr, not stdout. Stdout must be clean JSON so agents
// can parse it without interference.
func TestDeprecationWarningGoesToStderr(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "oldThing", true)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	// The call itself may fail (the stub has no handler for the operation);
	// only the warning routing matters here.
	stdout, stderr, _ := runCapture(t, bin, home, "old-thing", "--json")

	if strings.Contains(stdout, "is deprecated") {
		t.Errorf("deprecation warning leaked to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "is deprecated") {
		t.Errorf("deprecation warning missing from stderr:\n%s", stderr)
	}
}

// TestDeprecatedCommandHelpResolves covers `surf <deprecated> --help`. Help
// skips API-subcommand injection and resolves against Root, so deprecated
// operations must be registered there — while staying out of listings via
// Cobra's own availability rules.
func TestDeprecatedCommandHelpResolves(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "oldThing", true)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	// Help goes to stderr (stdout is reserved for data).
	_, out, err := runCapture(t, bin, home, "old-thing", "--help")
	if err != nil {
		t.Fatalf("--help on a deprecated command should succeed: %v\n%s", err, out)
	}
	if strings.Contains(out, "unknown command") {
		t.Errorf("deprecated command --help should not be rejected:\n%s", out)
	}
	if !strings.Contains(out, "--address") {
		t.Errorf("deprecated command --help should show its flags:\n%s", out)
	}

	_, root, err := runCapture(t, bin, home, "--help")
	if err != nil {
		t.Fatalf("root --help failed: %v", err)
	}
	if strings.Contains(root, "old-thing") {
		t.Errorf("deprecated command should stay out of root help:\n%s", root)
	}
}

// TestCompletionScriptsGoToStdout guards `surf completion bash > file`.
// Root writes to stderr so API JSON owns stdout, which silently produced an
// empty completion file.
func TestCompletionScriptsGoToStdout(t *testing.T) {
	bin := buildBareSurfBin(t)

	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			stdout, stderr, err := runCapture(t, bin, t.TempDir(), "completion", shell)
			if err != nil {
				t.Fatalf("completion %s failed: %v\n%s", shell, err, stderr)
			}
			if stdout == "" {
				t.Errorf("completion %s wrote nothing to stdout; a redirect would produce an empty file", shell)
			}
			if !strings.Contains(stdout, "surf") {
				t.Errorf("completion %s script does not mention the binary name", shell)
			}
		})
	}

	t.Run("help stays on stderr", func(t *testing.T) {
		stdout, stderr, _ := runCapture(t, bin, t.TempDir(), "completion")
		if stdout != "" {
			t.Errorf("bare `surf completion` should keep help off stdout:\n%s", stdout)
		}
		if !strings.Contains(stderr, "completion") {
			t.Errorf("bare `surf completion` should print help to stderr:\n%s", stderr)
		}
	})
}

// TestShellCompletionProtocol covers the hidden __complete command the shell
// scripts call. It was routed through the API subcommand and rejected, which
// made completion appear to work while offering nothing.
func TestShellCompletionProtocol(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	stdout, stderr, err := runCapture(t, bin, home, "__complete", "new-thing", "--a")
	if err != nil {
		t.Fatalf("__complete failed: %v\n%s", err, stderr)
	}
	if strings.Contains(stdout, "unknown command") {
		t.Fatalf("__complete was routed to the API subcommand:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--address") {
		t.Errorf("__complete should offer the --address flag, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, ":") {
		t.Errorf("__complete should end with a directive line, got:\n%s", stdout)
	}
}
