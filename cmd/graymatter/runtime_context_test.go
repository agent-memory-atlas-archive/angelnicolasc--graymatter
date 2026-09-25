package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeContextProjectPriorityAndStoreExplicitness(t *testing.T) {
	base := t.TempDir()
	process := filepath.Join(base, "process")
	project := filepath.Join(base, "Project É")
	payload := filepath.Join(project, "nested")
	custom := filepath.Join(base, "custom-store")
	for _, dir := range []string{process, payload, custom} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := runtimeContextInput{
		configuredDir: ".graymatter", capturedCWD: process,
		claudeProjectDir: project, claudeProjectDirPresent: true,
		payloadCWD: payload, transport: runtimeHook,
	}
	got, err := resolveRuntimeContext(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.projectRoot != project || got.projectRootSource != "claude_project_dir" || got.storeDir != filepath.Join(project, ".graymatter") || got.agentID != deriveAgentID(project) {
		t.Fatalf("root/store/identity disagree: %+v", got)
	}

	input.dirChanged = true
	got, err = resolveRuntimeContext(input)
	if err != nil || got.storeDir != filepath.Join(process, ".graymatter") || got.agentID != deriveAgentID(project) || got.storeDirSource != "explicit" {
		t.Fatalf("explicit relative --dir must use process cwd: %+v, %v", got, err)
	}
	input.configuredDir = custom
	got, err = resolveRuntimeContext(input)
	if err != nil || got.storeDir != custom || got.agentID != deriveAgentID(project) {
		t.Fatalf("explicit absolute --dir: %+v, %v", got, err)
	}

	input.claudeProjectDirPresent = false
	input.claudeProjectDir = ""
	input.dirChanged = false
	input.configuredDir = ".graymatter"
	got, err = resolveRuntimeContext(input)
	if err != nil || got.projectRoot != payload || got.storeDir != filepath.Join(payload, ".graymatter") || got.projectRootSource != "hook_payload_cwd" {
		t.Fatalf("payload fallback: %+v, %v", got, err)
	}
	input.payloadCWD = ""
	got, err = resolveRuntimeContext(input)
	if err != nil || got.projectRoot != process || got.storeDir != filepath.Join(process, ".graymatter") || got.projectRootSource != "process_cwd" {
		t.Fatalf("process fallback: %+v, %v", got, err)
	}
}

func TestRuntimeContextRejectsBadAndConflictingRoots(t *testing.T) {
	base := t.TempDir()
	process := filepath.Join(base, "process")
	project := filepath.Join(base, "project")
	sibling := filepath.Join(base, "other-worktree")
	for _, dir := range []string{process, project, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := runtimeContextInput{configuredDir: ".graymatter", capturedCWD: process, transport: runtimeHook}
	for name, root := range map[string]string{
		"empty": "", "relative": "relative/path", "missing": filepath.Join(base, "missing"), "file": filepath.Join(base, "regular-file"),
	} {
		if name == "file" {
			if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		input.claudeProjectDirPresent, input.claudeProjectDir = true, root
		if _, err := resolveRuntimeContext(input); err == nil {
			t.Errorf("accepted %s CLAUDE_PROJECT_DIR", name)
		}
	}
	input.claudeProjectDir = project
	input.payloadCWD = sibling
	if _, err := resolveRuntimeContext(input); err == nil || !strings.Contains(err.Error(), "outside CLAUDE_PROJECT_DIR") {
		t.Fatalf("cross-project payload accepted: %v", err)
	}
	input.payloadCWD = "relative"
	if _, err := resolveRuntimeContext(input); err == nil || !strings.Contains(err.Error(), "hook payload cwd") {
		t.Fatalf("relative payload accepted: %v", err)
	}
	input.payloadCWD = ""
	input.claudeProjectDirPresent = false
	input.cwdErr = errors.New("cwd unavailable")
	if _, err := resolveRuntimeContext(input); err == nil || !strings.Contains(err.Error(), "cwd unavailable") {
		t.Fatalf("lost cwd error: %v", err)
	}
	input.claudeProjectDirPresent, input.claudeProjectDir = true, project
	if got, err := resolveRuntimeContext(input); err != nil || got.projectRoot != project {
		t.Fatalf("valid env should work without cwd: %+v, %v", got, err)
	}
	input.dirChanged = true
	if _, err := resolveRuntimeContext(input); err == nil {
		t.Fatal("explicit relative --dir must require cwd")
	}
	input.configuredDir = filepath.Join(base, "absolute-store")
	if got, err := resolveRuntimeContext(input); err != nil || got.storeDir != input.configuredDir {
		t.Fatalf("absolute --dir should work without cwd: %+v, %v", got, err)
	}
}

func TestRuntimeContextLegacyConflictAndHTTPServiceRoot(t *testing.T) {
	base := t.TempDir()
	process := filepath.Join(base, "process")
	project := filepath.Join(base, "project")
	for _, dir := range []string{process, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := runtimeContextInput{
		configuredDir: ".graymatter", capturedCWD: process,
		claudeProjectDir: project, claudeProjectDirPresent: true,
		transport: runtimeMCPStdio,
	}
	if _, err := resolveRuntimeContext(input); err != nil {
		t.Fatalf("both candidates empty: %v", err)
	}
	legacy := filepath.Join(process, ".graymatter")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "MEMORY.md"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRuntimeContext(input); err == nil || !strings.Contains(err.Error(), "pass --dir explicitly") {
		t.Fatalf("legacy store conflict not reported: %v", err)
	}
	input.dirChanged = true
	if got, err := resolveRuntimeContext(input); err != nil || got.storeDir != legacy {
		t.Fatalf("explicit selection should resolve conflict: %+v, %v", got, err)
	}
	input.dirChanged = false
	input.transport = runtimeMCPHTTP
	input.claudeProjectDir = "" // HTTP must ignore inherited Claude context
	got, err := resolveRuntimeContext(input)
	if err != nil || got.projectRoot != "" || got.agentID != "" || got.storeDir != legacy {
		t.Fatalf("HTTP service route changed by Claude env: %+v, %v", got, err)
	}
}

func TestRuntimeContextPhysicalContainmentAndLogicalIdentity(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "actual-project")
	nested := filepath.Join(physical, "nested")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{nested, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "logical-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	input := runtimeContextInput{
		configuredDir: ".graymatter", capturedCWD: physical,
		claudeProjectDir: alias, claudeProjectDirPresent: true,
		payloadCWD: nested, transport: runtimeHook,
	}
	got, err := resolveRuntimeContext(input)
	if err != nil || got.agentID != "logical-alias" || got.projectRoot != alias {
		t.Fatalf("logical alias identity changed: %+v, %v", got, err)
	}
	escape := filepath.Join(physical, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("escape symlink unavailable: %v", err)
	}
	input.payloadCWD = escape
	if _, err := resolveRuntimeContext(input); err == nil || !strings.Contains(err.Error(), "outside CLAUDE_PROJECT_DIR") {
		t.Fatalf("symlink escape accepted: %v", err)
	}
}

func TestRuntimeContextRejectsBrokenStoreDirectoryAlias(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "broken-store-alias")
	if err := os.Symlink(filepath.Join(base, "missing-target"), alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	input := runtimeContextInput{
		configuredDir: alias, dirChanged: true, capturedCWD: project,
		transport: runtimeMCPStdio,
	}
	if _, err := resolveRuntimeContext(input); err == nil {
		t.Fatal("accepted dangling store directory alias")
	}
}

func TestRuntimeContextAcceptsPreparedStoreThroughRootAlias(t *testing.T) {
	base := t.TempDir()
	physical := filepath.Join(base, "physical-root")
	store := filepath.Join(physical, ".graymatter")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "MEMORY.md"), []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "logical-root")
	if err := os.Symlink(physical, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	got, err := resolveRuntimeContext(runtimeContextInput{
		configuredDir: ".graymatter", capturedCWD: physical,
		claudeProjectDir: alias, claudeProjectDirPresent: true,
		transport: runtimeMCPStdio,
	})
	if err != nil || got.projectRoot != alias || got.storeDir != filepath.Join(alias, ".graymatter") || got.agentID != "logical-root" {
		t.Fatalf("prepared alias should be one physical store with logical identity: %+v, %v", got, err)
	}
}

func TestRuntimeContextPreservesCaseSensitiveSiblingBoundary(t *testing.T) {
	base := t.TempDir()
	upper := filepath.Join(base, "Case")
	lower := filepath.Join(base, "case")
	if err := os.Mkdir(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Skipf("filesystem does not allow case-distinct sibling dirs: %v", err)
	}
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("filesystem treats these spellings as one directory")
	}
	_, err = resolveRuntimeContext(runtimeContextInput{
		configuredDir: ".graymatter", capturedCWD: upper,
		claudeProjectDir: upper, claudeProjectDirPresent: true,
		payloadCWD: lower, transport: runtimeHook,
	})
	if err == nil || !strings.Contains(err.Error(), "outside CLAUDE_PROJECT_DIR") {
		t.Fatalf("case-distinct sibling crossed project boundary: %v", err)
	}
}
