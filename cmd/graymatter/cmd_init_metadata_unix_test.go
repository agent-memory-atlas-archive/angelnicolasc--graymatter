//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func initAllowAuditSACLForTest(t *testing.T) {}

func initUnixReplacePlan(t *testing.T) (*initFilePlan, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"mcpServers":{}}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plan.close() })
	return plan, path, before
}

func TestInitI09FIFOSwapBeforeOpenRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := initOpenExistingFile
	initOpenExistingFile = func(root *os.Root, rel string) (*os.File, error) {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return nil, err
		}
		return previous(root, rel)
	}
	t.Cleanup(func() { initOpenExistingFile = previous })
	_, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err == nil || err.Error() != "unsafe_leaf" {
		t.Fatalf("FIFO swap was accepted: %v", err)
	}
}

func TestInitI09ChangedXattrBeforePublishRejected(t *testing.T) {
	plan, path, before := initUnixReplacePlan(t)
	if err := unix.Setxattr(path, "user.graymatter-init-test", []byte("private"), 0); err != nil {
		t.Skipf("fixture filesystem has no user xattrs: %v", err)
	}
	out := plan.apply()
	if out.err == nil || out.status != "failed" || out.uncertain {
		t.Fatalf("xattr mutation was published: %+v", out)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, before) {
		t.Fatalf("target changed after xattr mutation: %v", err)
	}
}

func TestInitI09SpecialModeRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	_, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err == nil || err.Error() != "unsupported_metadata" {
		t.Fatalf("special mode was accepted: %v", err)
	}
}

func TestInitI09RenameErrorReceipts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		publishThenE bool
		wantStatus   string
	}{
		{"not published", false, "failed"},
		{"published with error", true, "applied_cleanup_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, path, before := initUnixReplacePlan(t)
			previous := initReplacePublishedFile
			initReplacePublishedFile = func(root *os.Root, stage, target string) error {
				if tc.publishThenE {
					if err := previous(root, stage, target); err != nil {
						return err
					}
				}
				return errors.New("injected rename error")
			}
			t.Cleanup(func() { initReplacePublishedFile = previous })
			out := plan.apply()
			if out.status != tc.wantStatus || out.err == nil || out.uncertain {
				t.Fatalf("rename receipt: %+v", out)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.publishThenE == bytes.Equal(data, before) {
				t.Fatalf("publication does not match receipt: %s", data)
			}
		})
	}
}
