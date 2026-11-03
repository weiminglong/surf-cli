package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// stubMetrics counts requests the CLI makes to the stub, so tests can assert
// that completion does not sync or post telemetry.
type stubMetrics struct {
	mu          sync.Mutex
	openapiGets int
	eventPosts  int
}

func (m *stubMetrics) snapshot() (openapiGets, eventPosts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openapiGets, m.eventPosts
}

func (m *stubMetrics) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openapiGets, m.eventPosts = 0, 0
}

func stubSpecServer(t *testing.T, operationID string, deprecated bool) *httptest.Server {
	t.Helper()
	return stubSpecServerWithMetrics(t, operationID, deprecated, nil)
}

func stubSpecServerWithMetrics(t *testing.T, operationID string, deprecated bool, metrics *stubMetrics) *httptest.Server {
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
		switch {
		case r.URL.Path == "/gateway/openapi.json":
			if metrics != nil {
				metrics.mu.Lock()
				metrics.openapiGets++
				metrics.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, spec)
		case r.URL.Path == "/gateway/cli/event":
			if metrics != nil {
				metrics.mu.Lock()
				metrics.eventPosts++
				metrics.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{}")
		default:
			http.NotFound(w, r)
		}
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

// expireSurfCache moves the cached spec's TTL into the past.
func expireSurfCache(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, ".surf", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse cache config: %v", err)
	}
	cfg["surf.expires"] = "2020-01-01T00:00:00Z"
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode cache config: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write cache config: %v", err)
	}
}

// TestCompletionUsesExpiredCache covers the day-after case: completion must not
// hit the network, but it also must not go silent just because the cached spec
// is older than its TTL.
func TestCompletionUsesExpiredCache(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")
	expireSurfCache(t, home)

	stdout, stderr, err := runCapture(t, bin, home, "__complete", "")
	if err != nil {
		t.Fatalf("__complete failed: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "new-thing") {
		t.Errorf("completion should offer API commands from an expired cache, got:\n%s", stdout)
	}
}

// TestCompletionProtocolSkipsTelemetry guards the prompt: a telemetry POST on
// every Tab would stall completion on a slow network.
func TestCompletionProtocolSkipsTelemetry(t *testing.T) {
	old := os.Args
	t.Cleanup(func() { os.Args = old })

	for _, argv := range [][]string{
		{"surf", "__complete", "x"},
		{"surf", "__completeNoDesc", "x"},
	} {
		os.Args = argv
		if !isCompletionProtocol() {
			t.Errorf("%v should be treated as a completion request", argv)
		}
	}

	os.Args = []string{"surf", "market-price", "--symbol", "BTC"}
	if isCompletionProtocol() {
		t.Error("a normal command must still report telemetry")
	}
}

// TestCompletionHelpGoesToStderr keeps help consistent with every other
// command; only generated scripts and the __complete protocol own stdout.
func TestCompletionHelpGoesToStderr(t *testing.T) {
	bin := buildBareSurfBin(t)

	stdout, stderr, err := runCapture(t, bin, t.TempDir(), "completion", "bash", "--help")
	if err != nil {
		t.Fatalf("completion bash --help failed: %v", err)
	}
	if stdout != "" {
		t.Errorf("completion help should not write to stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "Generate the autocompletion script") {
		t.Errorf("completion help missing from stderr:\n%s", stderr)
	}
}

// TestCompactImpliesJSON matches the flag's documented behaviour: an agent
// that passes --compact should not have to discover --json too.
func TestCompactImpliesJSON(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	stdout, stderr, err := runCapture(t, bin, home, "list-operations", "--compact")
	if err != nil {
		t.Fatalf("--compact failed: %v\n%s", err, stderr)
	}
	var ops []map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &ops); jerr != nil {
		t.Fatalf("--compact should emit JSON: %v\n%s", jerr, stdout)
	}
	if len(ops) != 1 || ops[0]["name"] != "new-thing" {
		t.Errorf("unexpected catalog: %s", stdout)
	}
}

// runCaptureEnv runs surf with an explicit environment, replacing any keys it
// overrides so duplicate entries cannot make the result OS-dependent.
func runCaptureEnv(t *testing.T, bin, home string, extra map[string]string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	merged := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"SURF_API_BASE_URL": "", "SURF_TELEMETRY_DISABLED": "1",
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "NO_PROXY": "*",
		"http_proxy": "", "https_proxy": "", "all_proxy": "", "no_proxy": "*",
	}
	for k, v := range extra {
		merged[k] = v
	}
	env := make([]string, 0, len(os.Environ())+len(merged))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.Index(kv, "="); i >= 0 {
			key = kv[:i]
		}
		if _, replaced := merged[key]; !replaced {
			env = append(env, kv)
		}
	}
	for k, v := range merged {
		env = append(env, k+"="+v)
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = env
	so, se := splitOutput(cmd)
	err = cmd.Run()
	return so.String(), se.String(), err
}

// mutateCacheConfig edits ~/.surf/config.json in the isolated HOME.
func mutateCacheConfig(t *testing.T, home string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(home, ".surf", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse cache config: %v", err)
	}
	mutate(cfg)
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode cache config: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write cache config: %v", err)
	}
}

// TestCacheVersionMismatchResyncs guards against a CLI upgrade silently
// serving a cache that predates new fields (enums, bounds, examples).
func TestCacheVersionMismatchResyncs(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	metrics := &stubMetrics{}
	srv := stubSpecServerWithMetrics(t, "newThing", false, metrics)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	mutateCacheConfig(t, home, func(cfg map[string]any) {
		cfg["surf.cache_version"] = 1
	})
	metrics.reset()

	stdout, stderr, err := runCapture(t, bin, home, "list-operations", "--json")
	if err != nil {
		t.Fatalf("list-operations failed: %v\n%s", err, stderr)
	}
	if gets, _ := metrics.snapshot(); gets == 0 {
		t.Errorf("a cache written by an older CLI should be re-synced, got %d spec requests", gets)
	}
	var ops []map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &ops); jerr != nil {
		t.Fatalf("output is not JSON: %v", jerr)
	}
}

// TestCompletionOfflineWithExpiredCache verifies that help/list operations fall
// back to an expired cache when the network is unavailable.
func TestOfflineExpiredCacheStillServesMetadata(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "marketPrice", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")
	expireSurfCache(t, home)
	srv.Close() // make every later request fail

	stdout, stderr, err := runCapture(t, bin, home, "list-operations", "--json")
	if err != nil {
		t.Fatalf("list-operations should fall back to the stale cache: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "market-price") {
		t.Errorf("stale catalog missing commands:\n%s", stdout)
	}

	_, _, err = runCapture(t, bin, home, "market-price", "--help")
	if err != nil {
		t.Fatalf("--help should fall back to the stale cache: %v", err)
	}
}

// TestCompletionProtocolWithHelpWord guards a regression where any --help in
// the completed line sent candidates to stderr, leaving the shell empty.
func TestCompletionProtocolWithHelpWord(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	stdout, stderr, err := runCapture(t, bin, home, "__complete", "new-thing", "--help", "")
	if err != nil {
		t.Fatalf("__complete failed: %v\n%s", err, stderr)
	}
	if stdout == "" {
		t.Errorf("completion candidates must go to stdout even when the line contains --help")
	}
}

// TestInvalidCompletionKeepsStdoutClean verifies usage for a bad invocation
// stays on stderr, so `surf completion bash > file` cannot capture an error.
func TestInvalidCompletionKeepsStdoutClean(t *testing.T) {
	bin := buildBareSurfBin(t)

	stdout, stderr, err := runCapture(t, bin, t.TempDir(), "completion", "bash", "extra")
	if err == nil {
		t.Fatal("extra arguments should fail")
	}
	if stdout != "" {
		t.Errorf("invalid completion invocation wrote to stdout:\n%s", stdout)
	}
	if stderr == "" {
		t.Error("expected usage/error on stderr")
	}
}

// TestCompletionSkipsTelemetry verifies Tab never posts a telemetry event,
// while a normal local command still does.
func TestCompletionSkipsTelemetry(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	metrics := &stubMetrics{}
	srv := stubSpecServerWithMetrics(t, "newThing", false, metrics)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	env := map[string]string{"SURF_TELEMETRY_DISABLED": "0", "SURF_API_BASE_URL": srv.URL + "/gateway"}

	metrics.reset()
	if _, stderr, err := runCaptureEnv(t, bin, home, env, "__complete", ""); err != nil {
		t.Fatalf("__complete failed: %v\n%s", err, stderr)
	}
	if _, posts := metrics.snapshot(); posts != 0 {
		t.Errorf("__complete posted %d telemetry events", posts)
	}

	metrics.reset()
	if _, stderr, err := runCaptureEnv(t, bin, home, env, "version"); err != nil {
		t.Fatalf("version failed: %v\n%s", err, stderr)
	}
	// version is not a completion invocation, so it still reports.
	if _, posts := metrics.snapshot(); posts == 0 {
		t.Errorf("a normal command should still report telemetry")
	}
}

// TestListOperationsJSONAlone verifies --json works without --compact.
func TestListOperationsJSONAlone(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	stdout, stderr, err := runCapture(t, bin, home, "list-operations", "--json")
	if err != nil {
		t.Fatalf("list-operations --json failed: %v\n%s", err, stderr)
	}
	var ops []map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &ops); jerr != nil {
		t.Fatalf("--json should emit JSON, got: %v\n%s", jerr, stdout)
	}
	if len(ops) != 1 || ops[0]["name"] != "new-thing" {
		t.Errorf("unexpected catalog: %s", stdout)
	}
}

// TestUnknownFlagSuggestionEndToEnd proves the suggestion hook is wired onto
// the command, not only callable in isolation.
func TestUnknownFlagSuggestionEndToEnd(t *testing.T) {
	bin := buildBareSurfBin(t)
	home := t.TempDir()
	srv := stubSpecServer(t, "newThing", false)
	seedSurfSpecCache(t, bin, home, srv.URL+"/gateway", srv.URL+"/gateway/openapi.json")

	_, stderr, err := runCapture(t, bin, home, "new-thing", "--adress", "x")
	if err == nil {
		t.Fatal("an unknown flag should fail")
	}
	if !strings.Contains(stderr, "Did you mean") || !strings.Contains(stderr, "--address") {
		t.Errorf("expected a suggestion for --address, got:\n%s", stderr)
	}
}
