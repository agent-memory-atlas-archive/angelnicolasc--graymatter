package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Legacy init tests exercise PATH setup. Keep every in-process test away from
// the real user PATH, including tests that run the interactive wizard.
func TestMain(m *testing.M) {
	// A detached child must never execute the test suite as if it were the CLI.
	// The CLI's daemon and consolidate commands can outlive go test on Windows,
	// keeping graymatter.test.exe locked and recursively starting more tests.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(2)
	}
	env, err := captureE2EBuildEnvironment()
	// Helper subprocesses may deliberately lack build tools or a usable cwd.
	// Report capture failures only if a test actually requests a binary.
	cliE2EBinaries = &e2eBinaryCache{env: env, initErr: err}
	// In-process command tests use a direct store. Daemon E2E tests compile
	// and launch the real CLI binary with their own isolated environment.
	noDaemon = true
	initAddExeDirToUserPath = func() (bool, error) { return false, nil }
	initPreflightPath = func() error { return nil }
	code := m.Run()
	if err := cliE2EBinaries.close(); err != nil {
		fmt.Fprintln(os.Stderr, "clean E2E binary cache:", err)
		code = 1
	}
	os.Exit(code)
}
