package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// Capture build inputs before any fixture changes the process environment.
// The binaries are immutable package assets; every caller executes its own
// copy so fixture PATH entries, executable identities and cleanup stay local.
type e2eBuildEnvironment struct {
	cwd, tempDir    string
	env             []string
	goPath, gitPath string
	goErr, gitErr   error
}

func captureE2EBuildEnvironment() (e2eBuildEnvironment, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return e2eBuildEnvironment{}, err
	}
	env := e2eBuildEnvironment{cwd: cwd, tempDir: os.TempDir(), env: os.Environ()}
	env.goPath, env.goErr = exec.LookPath("go")
	env.gitPath, env.gitErr = exec.LookPath("git")
	return env, nil
}

func (env e2eBuildEnvironment) command(tool string, args ...string) (*exec.Cmd, error) {
	path, err := env.goPath, env.goErr
	if tool == "git" {
		path, err = env.gitPath, env.gitErr
	}
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Dir, cmd.Env = env.cwd, append([]string(nil), env.env...)
	return cmd, nil
}

type e2eBuiltBinary struct {
	path, archiveSHA256 string
}

type e2eBinaryBuild struct {
	binary e2eBuiltBinary
	err    error
}

type e2eBinaryCache struct {
	mu      sync.Mutex
	env     e2eBuildEnvironment
	initErr error
	dir     string
	builds  map[string]e2eBinaryBuild
	closed  bool
}

var cliE2EBinaries *e2eBinaryCache

// Builders return errors rather than calling testing.Fatal: every concurrent
// caller must observe the same success or failure, never a partial executable.
func (c *e2eBinaryCache) get(key string, build func(e2eBuildEnvironment, string) (e2eBuiltBinary, error)) (e2eBuiltBinary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return e2eBuiltBinary{}, errors.New("E2E binary cache is closed")
	}
	if c.initErr != nil {
		return e2eBuiltBinary{}, c.initErr
	}
	if result, ok := c.builds[key]; ok {
		return result.binary, result.err
	}
	if c.builds == nil {
		c.builds = make(map[string]e2eBinaryBuild)
	}
	var result e2eBinaryBuild
	if c.dir == "" {
		c.dir, result.err = os.MkdirTemp(c.env.tempDir, "graymatter-e2e-binaries-")
	}
	if result.err == nil {
		var dir string
		dir, result.err = os.MkdirTemp(c.dir, "build-")
		if result.err == nil {
			result.binary, result.err = build(c.env, dir)
		}
	}
	if result.err != nil {
		result.binary = e2eBuiltBinary{}
	}
	c.builds[key] = result
	return result.binary, result.err
}

func (c *e2eBinaryCache) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.dir == "" {
		return nil
	}
	return os.RemoveAll(c.dir)
}

func copyCurrentE2EBinary(t *testing.T, destination string) {
	t.Helper()
	binary, err := cliE2EBinaries.get("current", func(env e2eBuildEnvironment, dir string) (e2eBuiltBinary, error) {
		path := filepath.Join(dir, "graymatter.exe")
		cmd, err := env.command("go", "build", "-o", path, ".")
		if err != nil {
			return e2eBuiltBinary{}, err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return e2eBuiltBinary{}, fmt.Errorf("build CLI: %w\n%s", err, out)
		}
		return e2eBuiltBinary{path: path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	issue81CopyExecutable(t, binary.path, destination)
}

func TestE2EBinaryCacheConcurrentReuseAndFailures(t *testing.T) {
	cache := &e2eBinaryCache{env: e2eBuildEnvironment{tempDir: t.TempDir()}}
	t.Cleanup(func() {
		if err := cache.close(); err != nil {
			t.Error(err)
		}
	})
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			var calls atomic.Int32
			failure := errors.New("compiler rejected source")
			build := func(_ e2eBuildEnvironment, dir string) (e2eBuiltBinary, error) {
				calls.Add(1)
				path := filepath.Join(dir, "binary")
				if err := os.WriteFile(path, []byte("fixture executable"), 0o755); err != nil {
					return e2eBuiltBinary{}, err
				}
				if fail {
					return e2eBuiltBinary{path: path}, failure
				}
				return e2eBuiltBinary{path: path}, nil
			}
			const n = 12
			var wg sync.WaitGroup
			binaries, errs := make([]e2eBuiltBinary, n), make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					binaries[i], errs[i] = cache.get(t.Name(), build)
				}()
			}
			wg.Wait()
			if calls.Load() != 1 {
				t.Fatalf("compiler ran %d times", calls.Load())
			}
			for i := range n {
				if fail {
					if !errors.Is(errs[i], failure) || binaries[i].path != "" {
						t.Fatalf("caller %d received partial build or lost error: %+v %v", i, binaries[i], errs[i])
					}
				} else if errs[i] != nil || binaries[i].path == "" || binaries[i] != binaries[0] {
					t.Fatalf("caller %d did not share successful build: %+v %v", i, binaries[i], errs[i])
				}
			}
		})
	}
}

func TestE2EBinaryCacheOutlivesFixtureAndCopiesStayIndependent(t *testing.T) {
	cache := &e2eBinaryCache{env: e2eBuildEnvironment{tempDir: t.TempDir()}}
	t.Cleanup(func() { _ = cache.close() })
	build := func(_ e2eBuildEnvironment, dir string) (e2eBuiltBinary, error) {
		path := filepath.Join(dir, "binary")
		return e2eBuiltBinary{path: path}, os.WriteFile(path, []byte("original executable"), 0o755)
	}
	var firstCopy, source string
	t.Run("first_fixture", func(t *testing.T) {
		binary, err := cache.get("current", build)
		if err != nil {
			t.Fatal(err)
		}
		source = binary.path
		firstCopy = filepath.Join(t.TempDir(), "graymatter.exe")
		issue81CopyExecutable(t, source, firstCopy)
		if err := os.WriteFile(firstCopy, []byte("fixture modification"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(firstCopy); !os.IsNotExist(err) {
		t.Fatalf("first fixture did not clean its executable: %v", err)
	}
	got, err := cache.get("current", func(e2eBuildEnvironment, string) (e2eBuiltBinary, error) {
		return e2eBuiltBinary{}, errors.New("first fixture cleanup caused a rebuild")
	})
	if err != nil || got.path != source {
		t.Fatalf("package binary did not survive fixture cleanup: %+v %v", got, err)
	}
	second := filepath.Join(t.TempDir(), "graymatter.exe")
	issue81CopyExecutable(t, got.path, second)
	if data, err := os.ReadFile(second); err != nil || string(data) != "original executable" {
		t.Fatalf("fixture modified shared executable: %q %v", data, err)
	}
	previous, err := cache.get("previous-revision", build)
	if err != nil || previous.path == source {
		t.Fatalf("different revisions shared one binary: %+v %v", previous, err)
	}
	if err := cache.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("package cleanup left cached executable: %v", err)
	}
	if _, err := cache.get("current", build); err == nil {
		t.Fatal("closed cache served a deleted executable")
	}
}

func TestE2EBuildInputsSurviveFixtureEnvironment(t *testing.T) {
	env, err := captureE2EBuildEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	// This unit test does not execute the tools and must also run in -short
	// environments without Git. Use the known test executable as both paths.
	toolPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env.goPath, env.gitPath, env.goErr, env.gitErr = toolPath, toolPath, nil, nil
	for _, key := range []string{"HOME", "USERPROFILE", "TMP", "TEMP", "PATH", "GOWORK", "GOCACHE", "GOMODCACHE"} {
		t.Setenv(key, filepath.Join(t.TempDir(), "fixture-only"))
	}
	t.Chdir(t.TempDir())
	for _, tool := range []string{"go", "git"} {
		cmd, err := env.command(tool, "version")
		if err != nil {
			t.Fatal(err)
		}
		wantPath := env.goPath
		if tool == "git" {
			wantPath = env.gitPath
		}
		if cmd.Dir != env.cwd || cmd.Path != wantPath || !reflect.DeepEqual(cmd.Env, env.env) {
			t.Fatalf("fixture changed %s build inputs: dir=%s path=%s", tool, cmd.Dir, cmd.Path)
		}
		cmd.Env[0] = "changed=only-command-copy"
		if reflect.DeepEqual(cmd.Env, env.env) {
			t.Fatal("command could mutate the captured environment")
		}
	}
}
