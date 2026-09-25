package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func initCLIEnv(home string) []string {
	allowed := map[string]bool{
		"PATH": true, "SYSTEMROOT": true, "COMSPEC": true, "TEMP": true,
		"TMP": true, "WINDIR": true, "PATHEXT": true, "LANG": true,
		"LC_ALL": true, "TZ": true,
	}
	replacements := map[string]string{
		"HOME": home, "USERPROFILE": home,
		"APPDATA":           filepath.Join(home, "AppData", "Roaming"),
		"LOCALAPPDATA":      filepath.Join(home, "AppData", "Local"),
		"XDG_CONFIG_HOME":   filepath.Join(home, "xdg"),
		"CODEX_HOME":        filepath.Join(home, "codex"),
		"CLAUDE_CONFIG_DIR": filepath.Join(home, "claude"),
		"OPENAI_API_KEY":    "", "ANTHROPIC_API_KEY": "", "VOYAGE_API_KEY": "",
		"COHERE_API_KEY": "", "GEMINI_API_KEY": "", "GOOGLE_API_KEY": "",
		"AZURE_OPENAI_API_KEY": "", "OPENROUTER_API_KEY": "", "MISTRAL_API_KEY": "",
		"TOGETHER_API_KEY": "", "GROQ_API_KEY": "", "HF_TOKEN": "",
		"OPENAI_BASE_URL": "", "ANTHROPIC_BASE_URL": "",
		"GRAYMATTER_OLLAMA_URL": "disabled://adjacent81-tests",
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if upper == "CLAUDE_PROJECT_DIR" {
			continue
		}
		if _, replace := replacements[upper]; !replace && allowed[upper] {
			env = append(env, entry)
		}
	}
	for key, value := range replacements {
		env = append(env, key+"="+value)
	}
	return env
}

func TestInitI08CommandOutputAndExitContract(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "graymatter")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build command: %v: %s", err, out)
	}
	base := t.TempDir()
	project := filepath.Join(base, "project")
	home := filepath.Join(base, "home")
	for _, dir := range []string{project, home, filepath.Join(home, "AppData", "Roaming"),
		filepath.Join(home, "AppData", "Local"), filepath.Join(home, "xdg"), filepath.Join(home, "codex"), filepath.Join(home, "claude")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(project, ".graymatter")
	run := func(args ...string) ([]byte, []byte, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = project
		command.Env = initCLIEnv(home)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		stdout, err := command.Output()
		if ctx.Err() != nil {
			t.Fatalf("init command timed out: %v", ctx.Err())
		}
		if err == nil {
			return stdout, stderr.Bytes(), 0
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("execute command: %v", err)
		}
		return stdout, stderr.Bytes(), exit.ExitCode()
	}
	baseArgs := []string{"init", "--only", "claudecode", "--skip-instructions", "--no-path", "--dir", store}
	stdout, stderr, exit := run(append(append([]string{}, baseArgs...), "--json")...)
	if exit != 0 || len(stderr) != 0 {
		t.Fatalf("JSON setup exit=%d stderr=%s", exit, stderr)
	}
	var success setupResult
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(&success); err != nil || success.Mode != "setup" || !success.Complete || success.RuntimeVerified {
		t.Fatalf("invalid success envelope: %s err=%v", stdout, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON setup emitted another object: %s", stdout)
	}
	settingsBytes, err := os.ReadFile(filepath.Join(project, ".mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(settingsBytes, &settings); err != nil ||
		!reflect.DeepEqual(settings.MCPServers["graymatter"].Args, []string{"mcp", "serve", "--dir", store}) {
		t.Fatalf("persisted MCP routing does not match selected store: %s err=%v", settingsBytes, err)
	}
	storeOnlyPath := filepath.Join(project, "standalone")
	stdout, stderr, exit = run("init", "--store-only", "--dir", storeOnlyPath, "--json")
	var storeOnly storeOnlyInitResult
	if err := json.Unmarshal(stdout, &storeOnly); err != nil || exit != 0 || len(stderr) != 0 ||
		storeOnly.Mode != "store-only" || storeOnly.Status != "created" || storeOnly.RuntimeVerified ||
		storeOnly.DataDir != storeOnlyPath {
		t.Fatalf("store-only mode was not distinct: exit=%d result=%+v stderr=%s err=%v", exit, storeOnly, stderr, err)
	}
	if current, err := os.ReadFile(filepath.Join(project, ".mcp.json")); err != nil || !bytes.Equal(current, settingsBytes) {
		t.Fatalf("store-only changed installed MCP config: %s err=%v", current, err)
	}
	secret := "secret-canary-9007199254740993"
	if err := os.WriteFile(filepath.Join(project, ".mcp.json"), []byte(`{"mcpServers":{"graymatter":{"command":"`+secret+`"}},`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exit = run(append(append([]string{}, baseArgs...), "--json", "--best-effort")...)
	if exit == 0 || bytes.Contains(stdout, []byte(secret)) || bytes.Contains(stderr, []byte(secret)) {
		t.Fatalf("preflight redaction/exit: exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	var failed setupResult
	decoder = json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(&failed); err != nil || failed.Phase != "preflight" || failed.Status != "failed" || failed.Complete {
		t.Fatalf("invalid failed envelope: %s err=%v", stdout, err)
	}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON error emitted another object: %s", stdout)
	}
	stdout, stderr, exit = run(append(append([]string{}, baseArgs...), "--quiet")...)
	if exit == 0 || bytes.Contains(stdout, []byte(secret)) || bytes.Contains(stderr, []byte(secret)) ||
		!bytes.Contains(stdout, []byte("invalid_document")) || bytes.Contains(stdout, []byte("store: unchanged")) {
		t.Fatalf("quiet failure output: exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	stdout, stderr, exit = run("init", "--json", "--not-a-real-flag")
	if exit == 0 || len(stdout) != 0 || !bytes.Contains(stderr, []byte("unknown flag")) {
		t.Fatalf("parser error output: exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
}

type initFailWriter struct{}

func (initFailWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func TestInitI08JSONEncoderFailureDoesNotRetry(t *testing.T) {
	result := newSetupResult(false)
	command := &cobra.Command{}
	command.SetOut(initFailWriter{})
	if err := renderSetupResult(command, &result, false, true); err == nil {
		t.Fatal("encoder failure was reported as success")
	}
}

func TestInitI07PartialApplySubprocessHelper(t *testing.T) {
	mode := os.Getenv("_GRAYMATTER_INIT_PARTIAL_HELPER")
	if mode == "" {
		return
	}
	if mode == "partial" || mode == "best-effort" {
		original := applyInitFile
		applyInitFile = func(p *initFilePlan) initPublishResult {
			if strings.HasSuffix(p.path, filepath.Join(".cursor", "mcp.json")) {
				return initPublishResult{status: "failed", err: errors.New("injected apply failure")}
			}
			return original(p)
		}
	}
	store := filepath.Join(t.TempDir(), "unused")
	if requested := os.Getenv("_GRAYMATTER_INIT_PARTIAL_STORE"); requested != "" {
		store = requested
	}
	os.Args = []string{os.Args[0], "init", "--only", "claudecode,cursor", "--skip-instructions", "--no-path", "--dir", store, "--json"}
	if mode == "best-effort" {
		os.Args = append(os.Args, "--best-effort")
	}
	main()
	os.Exit(0) // Suppress the test runner's PASS line after JSON output.
}

func TestInitI07PartialApplySubprocessExitAndRetry(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(project, mode string) (setupResult, []byte, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "-test.run=^TestInitI07PartialApplySubprocessHelper$")
		command.Dir = project
		command.Env = append(initCLIEnv(home), "_GRAYMATTER_INIT_PARTIAL_HELPER="+mode,
			"_GRAYMATTER_INIT_PARTIAL_STORE="+filepath.Join(project, "store"))
		var stderr bytes.Buffer
		command.Stderr = &stderr
		stdout, err := command.Output()
		if ctx.Err() != nil {
			t.Fatalf("init subprocess timed out: %v", ctx.Err())
		}
		exit := 0
		if err != nil {
			var failed *exec.ExitError
			if !errors.As(err, &failed) {
				t.Fatalf("init subprocess: %v", err)
			}
			exit = failed.ExitCode()
		}
		var result setupResult
		decoder := json.NewDecoder(bytes.NewReader(stdout))
		if err := decoder.Decode(&result); err != nil {
			t.Fatalf("invalid JSON receipt: %s err=%v stderr=%s", stdout, err, stderr.Bytes())
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			t.Fatalf("extra subprocess output: %s", stdout)
		}
		return result, stderr.Bytes(), exit
	}
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	partial, stderr, exit := run(project, "partial")
	if exit != 1 || partial.Mode != "setup" || partial.Phase != "apply" || partial.Status != "partial" ||
		partial.Complete || !partial.Changed || partial.EffectsUncertain || !bytes.Contains(stderr, []byte("initialization incomplete")) {
		t.Fatalf("partial subprocess receipt: exit=%d result=%+v stderr=%s", exit, partial, stderr)
	}
	mcpPath := filepath.Join(project, ".mcp.json")
	firstBytes, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("failed client was published: %v", err)
	}
	retry, stderr, exit := run(project, "retry")
	if exit != 0 || len(stderr) != 0 || retry.Status != "complete" || !retry.Complete {
		t.Fatalf("retry receipt: exit=%d result=%+v stderr=%s", exit, retry, stderr)
	}
	if data, err := os.ReadFile(mcpPath); err != nil || !bytes.Equal(data, firstBytes) {
		t.Fatalf("retry changed earlier client: %s err=%v", data, err)
	}
	if info, err := os.Stat(mcpPath); err != nil || !info.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatalf("retry changed earlier client mtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".cursor", "mcp.json")); err != nil {
		t.Fatalf("retry did not publish failed client: %v", err)
	}
	again, stderr, exit := run(project, "retry")
	if exit != 0 || len(stderr) != 0 || again.Status != "complete" || again.Changed {
		t.Fatalf("idempotent rerun receipt: exit=%d result=%+v stderr=%s", exit, again, stderr)
	}
	bestEffortProject := filepath.Join(base, "best-effort")
	if err := os.MkdirAll(bestEffortProject, 0o755); err != nil {
		t.Fatal(err)
	}
	bestEffort, stderr, exit := run(bestEffortProject, "best-effort")
	if exit != 0 || len(stderr) != 0 || bestEffort.Status != "partial" || !bestEffort.BestEffort ||
		bestEffort.EffectsUncertain || bestEffort.Complete {
		t.Fatalf("best-effort receipt: exit=%d result=%+v stderr=%s", exit, bestEffort, stderr)
	}
}
