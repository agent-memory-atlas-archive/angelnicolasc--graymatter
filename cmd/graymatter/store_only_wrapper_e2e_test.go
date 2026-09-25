package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const issue81RecorderSource = `package main
import (
    "encoding/json"
    "os"
    "strconv"
)
func main() {
    cwd, err := os.Getwd()
    if err != nil { os.Exit(98) }
    exe, err := os.Executable()
    if err != nil { os.Exit(98) }
    record := struct {
        CWD string   ` + "`json:\"cwd\"`" + `
        Args []string ` + "`json:\"args\"`" + `
        Exe string   ` + "`json:\"exe\"`" + `
    }{cwd, os.Args[1:], exe}
    f, err := os.Create(os.Getenv("ISSUE81_RECORDER_PATH"))
    if err != nil { os.Exit(98) }
    if err := json.NewEncoder(f).Encode(record); err != nil { os.Exit(98) }
    if err := f.Close(); err != nil { os.Exit(98) }
    code, _ := strconv.Atoi(os.Getenv("ISSUE81_RECORDER_EXIT"))
    os.Exit(code)
}`

type issue81Recording struct {
	CWD  string   `json:"cwd"`
	Args []string `json:"args"`
	Exe  string   `json:"exe"`
}

func issue81CopyExecutable(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func issue81Git(t *testing.T, f *issue81Fixture, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, f.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
}

func issue81WrapperPath(t *testing.T) string {
	t.Helper()
	name := "gm-claude"
	if runtime.GOOS == "windows" {
		name += ".ps1"
	}
	path, err := filepath.Abs(filepath.Join("..", "..", "examples", "claude-global", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("versioned wrapper missing: %v", err)
	}
	return path
}

func issue81PathFromEnv(env []string) string {
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(key, "PATH") {
			return value
		}
	}
	return ""
}

func issue81WrapperCommand(t *testing.T, f *issue81Fixture, wrapper, cwd, receipt, path string, exit int, inheritedNativeError bool, args ...string) issue81Output {
	t.Helper()
	extra := map[string]string{
		"PATH":                  path,
		"ISSUE81_RECORDER_PATH": receipt,
		"ISSUE81_RECORDER_EXIT": fmt.Sprint(exit),
	}
	if runtime.GOOS != "windows" {
		return f.runCommand("bash", cwd, "", extra, append([]string{wrapper}, args...)...)
	}
	parent := filepath.Join(f.root, fmt.Sprintf("invoke-%t.ps1", inheritedNativeError))
	setting := "$false"
	if inheritedNativeError {
		setting = "$true"
	}
	// The parent deliberately inherits Legacy argv and both native error
	// preferences. The versioned wrapper must establish Standard and false in
	// its own scope before it invokes the native recorder.
	source := "#requires -Version 7.3\n" +
		"$global:PSNativeCommandUseErrorActionPreference = " + setting + "\n" +
		"$global:PSNativeCommandArgumentPassing = 'Legacy'\n" +
		"& '" + strings.ReplaceAll(wrapper, "'", "''") + "' @args\n" +
		"exit $LASTEXITCODE\n"
	if err := os.WriteFile(parent, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return f.runCommand("pwsh", cwd, "", extra, append([]string{"-NoProfile", "-NonInteractive", "-File", parent}, args...)...)
}

func issue81ReadRecording(t *testing.T, path string) issue81Recording {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record issue81Recording
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode native recorder: %v\n%s", err, data)
	}
	return record
}

func issue81SameFile(t *testing.T, a, b string) bool {
	t.Helper()
	aInfo, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat recorded path %q: %v", a, err)
	}
	bInfo, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat expected path %q: %v", b, err)
	}
	return os.SameFile(aInfo, bInfo)
}

func TestStoreOnlyE05_VersionedClaudeWrapper(t *testing.T) {
	f := newIssue81Fixture(t)
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("pwsh"); err != nil {
			t.Fatalf("PowerShell 7.3+ is required for E05: %v", err)
		}
	} else if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("Bash is required for E05: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal(err)
	}
	wrapper := issue81WrapperPath(t)
	bin1 := filepath.Join(f.root, "first-install")
	bin2 := filepath.Join(f.root, "second-install")
	for _, dir := range []string{bin1, bin2} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	recorderSource := filepath.Join(f.root, "recorder.go")
	if err := os.WriteFile(recorderSource, []byte(issue81RecorderSource), 0o600); err != nil {
		t.Fatal(err)
	}
	name := "claude"
	gmName := "graymatter"
	if runtime.GOOS == "windows" {
		name += ".exe"
		gmName += ".exe"
	}
	firstClaude := filepath.Join(bin1, name)
	build := exec.Command("go", "build", "-o", firstClaude, recorderSource)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native recorder: %v\n%s", err, out)
	}
	issue81CopyExecutable(t, firstClaude, filepath.Join(bin2, name))
	issue81CopyExecutable(t, firstClaude, filepath.Join(bin2, gmName))
	issue81CopyExecutable(t, f.bin, filepath.Join(bin1, gmName))
	path := strings.Join([]string{bin1, bin2, issue81PathFromEnv(f.env)}, string(os.PathListSeparator))

	repo := filepath.Join(f.root, "repo-é with spaces")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	issue81Git(t, f, f.root, "init", "-q", repo)
	issue81Git(t, f, repo, "config", "user.name", "angelnicolasc")
	f.registerStore(filepath.Join(repo, ".graymatter"))
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, inherited := range []bool{false, true} {
		receipt := filepath.Join(f.root, fmt.Sprintf("root-%t.json", inherited))
		args := []string{"simple", "two words", `embedded "quotes"`, "", "Unicode-é 雪"}
		out := issue81WrapperCommand(t, f, wrapper, repo, receipt, path, 17, inherited, args...)
		if out.code != 17 {
			t.Fatalf("root wrapper inheritedNativeError=%t exit=%d err=%v stdout=%q stderr=%q", inherited, out.code, out.err, out.stdout, out.stderr)
		}
		record := issue81ReadRecording(t, receipt)
		if !issue81SameFile(t, record.CWD, repo) || !reflect.DeepEqual(record.Args, args) {
			t.Errorf("root wrapper cwd/argv changed: %+v want cwd=%q argv=%q", record, repo, args)
		}
		if !issue81SameFile(t, record.Exe, firstClaude) {
			t.Errorf("wrapper selected second installation: %q", record.Exe)
		}
	}
	if cwd, _ := os.Getwd(); cwd != originalCWD {
		t.Errorf("caller cwd changed: %q -> %q", originalCWD, cwd)
	}
	issue81AssertOnlyMarker(t, filepath.Join(repo, ".graymatter"))

	subReceipt := filepath.Join(f.root, "subdir.json")
	subOut := issue81WrapperCommand(t, f, wrapper, sub, subReceipt, path, 0, false, "from-subdir")
	if subOut.code != 0 {
		t.Fatalf("subdir wrapper: %+v", subOut)
	}
	if got := issue81ReadRecording(t, subReceipt); !issue81SameFile(t, got.CWD, repo) || !reflect.DeepEqual(got.Args, []string{"from-subdir"}) {
		t.Errorf("subdir routing/args: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(sub, ".graymatter")); !os.IsNotExist(err) {
		t.Errorf("subdir acquired an unintended store: %v", err)
	}

	// --orphan creates an actual Git worktree with a .git file and no commit.
	worktree := filepath.Join(f.root, "worktree-é with spaces")
	issue81Git(t, f, repo, "worktree", "add", "--orphan", "-b", "issue81-e2e", worktree)
	if info, err := os.Stat(filepath.Join(worktree, ".git")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("Git worktree lacks .git file: info=%v err=%v", info, err)
	}
	f.registerStore(filepath.Join(worktree, ".graymatter"))
	wReceipt := filepath.Join(f.root, "worktree.json")
	wOut := issue81WrapperCommand(t, f, wrapper, worktree, wReceipt, path, 0, false, "worktree")
	if wOut.code != 0 {
		t.Fatalf("worktree wrapper: %+v", wOut)
	}
	if got := issue81ReadRecording(t, wReceipt); !issue81SameFile(t, got.CWD, worktree) || !reflect.DeepEqual(got.Args, []string{"worktree"}) {
		t.Errorf("worktree routing/args: %+v", got)
	}
	issue81AssertOnlyMarker(t, filepath.Join(worktree, ".graymatter"))

	failing := filepath.Join(f.root, "failing-repo")
	if err := os.MkdirAll(filepath.Join(failing, ".graymatter", "MEMORY.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	issue81Git(t, f, f.root, "init", "-q", failing)
	f.registerStore(filepath.Join(failing, ".graymatter"))
	failureReceipt := filepath.Join(f.root, "failure.json")
	failure := issue81WrapperCommand(t, f, wrapper, failing, failureReceipt, path, 0, false, "never-launched")
	if failure.code != 1 {
		t.Fatalf("preparation failure exit=%d stderr=%q", failure.code, failure.stderr)
	}
	if _, err := os.Stat(failureReceipt); !os.IsNotExist(err) {
		t.Fatalf("Claude launched after preparation failed: %v", err)
	}

	outside := filepath.Join(f.root, "outside-git")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	f.registerStore(filepath.Join(outside, ".graymatter"))
	outsideReceipt := filepath.Join(f.root, "outside.json")
	out := issue81WrapperCommand(t, f, wrapper, outside, outsideReceipt, path, 0, false, "never-launched")
	if out.code != 2 {
		t.Fatalf("outside-Git exit=%d stderr=%q", out.code, out.stderr)
	}
	if _, err := os.Stat(outsideReceipt); !os.IsNotExist(err) {
		t.Fatalf("Claude launched outside Git: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside-Git wrapper wrote files: %v err=%v", entries, err)
	}
}
