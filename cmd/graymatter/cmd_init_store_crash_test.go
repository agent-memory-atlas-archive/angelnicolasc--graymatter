package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This helper runs only when explicitly started by TestStoreOnlyCrashRecovery.
// Its barrier is attached to the test's private link seam, so production code
// has no crash flag or environment-dependent behavior.
func TestStoreOnlyCrashHelper(t *testing.T) {
	if os.Getenv("GRAYMATTER_TEST_STORE_CRASH_HELPER") != "1" {
		return
	}
	dataDir := os.Getenv("GRAYMATTER_TEST_STORE_CRASH_DIR")
	phase := os.Getenv("GRAYMATTER_TEST_STORE_CRASH_PHASE")
	if dataDir == "" || (phase != "before-link" && phase != "after-link") {
		t.Fatal("invalid store crash helper setup")
	}

	ops := defaultStoreInitOps()
	realLink := ops.link
	ops.link = func(root *os.Root, oldName, newName string) error {
		if phase == "after-link" {
			if err := realLink(root, oldName, newName); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(os.Stdout, phase); err != nil {
			return err
		}
		// The parent retains stdin open until it kills this exact child handle.
		// If the pipe closes unexpectedly, exit without crossing the barrier.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(73)
		return nil
	}
	if _, err := prepareStoreDirectoryWithOps(dataDir, ops); err != nil {
		t.Fatalf("prepare store in crash helper: %v", err)
	}
	t.Fatal("crash helper passed the publication barrier")
}

func TestStoreOnlyCrashRecovery(t *testing.T) {
	for _, phase := range []string{"before-link", "after-link"} {
		t.Run(phase, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), ".graymatter")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(cancel)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreOnlyCrashHelper$")
			cmd.Env = append(os.Environ(),
				"GRAYMATTER_TEST_STORE_CRASH_HELPER=1",
				"GRAYMATTER_TEST_STORE_CRASH_DIR="+dataDir,
				"GRAYMATTER_TEST_STORE_CRASH_PHASE="+phase,
			)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			started := false
			waited := false
			t.Cleanup(func() {
				if started && !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				_ = stdin.Close()
			})
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			started = true

			barrier, readErr := bufio.NewReader(stdout).ReadString('\n')
			if readErr != nil || strings.TrimSpace(barrier) != phase {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				waited = true
				t.Fatalf("crash helper did not reach %s barrier: line=%q read=%v stderr=%q", phase, barrier, readErr, stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill crash helper at %s barrier: %v", phase, err)
			}
			if err := cmd.Wait(); err == nil {
				t.Fatal("crash helper exited successfully instead of being killed")
			}
			waited = true
			if ctx.Err() != nil {
				t.Fatalf("crash helper exceeded deadline: %v", ctx.Err())
			}

			markerPath := filepath.Join(dataDir, "MEMORY.md")
			markerBefore, markerErr := os.ReadFile(markerPath)
			if phase == "before-link" {
				if !os.IsNotExist(markerErr) {
					t.Fatalf("marker published before Link: data=%q err=%v", markerBefore, markerErr)
				}
			} else if markerErr != nil || string(markerBefore) != storeInitializationMarker {
				t.Fatalf("published marker incomplete after Link: data=%q err=%v", markerBefore, markerErr)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "gray.db")); !os.IsNotExist(err) {
				t.Fatalf("crash path opened database: %v", err)
			}
			tempPath, tempBefore := assertCrashStoreFiles(t, dataDir, phase == "after-link")

			result, err := prepareStoreDirectory(dataDir)
			if err != nil {
				t.Fatalf("recover after %s crash: %v", phase, err)
			}
			wantStatus := "created"
			if phase == "after-link" {
				wantStatus = "already_prepared"
			}
			if result.status != wantStatus || result.marker != "MEMORY.md" {
				t.Fatalf("recovery result = %+v, want status %s with MEMORY.md", result, wantStatus)
			}
			markerAfter, err := os.ReadFile(markerPath)
			if err != nil || string(markerAfter) != storeInitializationMarker {
				t.Fatalf("recovery marker incomplete: data=%q err=%v", markerAfter, err)
			}
			assertCrashStoreFiles(t, dataDir, true)
			assertUnchangedCrashTemp(t, tempPath, tempBefore)

			result, err = prepareStoreDirectory(dataDir)
			if err != nil || result.status != "already_prepared" || result.marker != "MEMORY.md" {
				t.Fatalf("second recovery = %+v, err=%v", result, err)
			}
			assertCrashStoreFiles(t, dataDir, true)
			assertUnchangedCrashTemp(t, tempPath, tempBefore)
		})
	}
}

func assertCrashStoreFiles(t *testing.T, dataDir string, wantMarker bool) (string, os.FileInfo) {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var tempPath string
	for _, entry := range entries {
		switch {
		case entry.Name() == "MEMORY.md" && wantMarker:
		case strings.HasPrefix(entry.Name(), ".init-store-") && strings.HasSuffix(entry.Name(), ".tmp") && tempPath == "":
			tempPath = filepath.Join(dataDir, entry.Name())
		default:
			t.Fatalf("unexpected store file after crash/recovery: %s", entry.Name())
		}
	}
	wantCount := 1
	if wantMarker {
		wantCount = 2
	}
	if len(entries) != wantCount || tempPath == "" {
		t.Fatalf("store entries after crash/recovery = %v, want %d entries including one stale temporary", entries, wantCount)
	}
	content, err := os.ReadFile(tempPath)
	if err != nil || string(content) != storeInitializationMarker {
		t.Fatalf("crash temporary incomplete: data=%q err=%v", content, err)
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	return tempPath, info
}

func assertUnchangedCrashTemp(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("recovery changed stale crash temporary %s", path)
	}
}
