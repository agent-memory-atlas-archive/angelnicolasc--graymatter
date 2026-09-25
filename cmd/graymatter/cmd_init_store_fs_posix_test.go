//go:build linux || darwin

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStoreOnlyRejectsRealFIFOAndSocket(t *testing.T) {
	for _, kind := range []string{"fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			// Unix-domain socket paths are short on every supported host, including
			// macOS where the kernel's path limit is lower than a long t.TempDir.
			dir, err := os.MkdirTemp("/tmp", "gmso-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			leaf := filepath.Join(dir, "MEMORY.md")
			if kind == "fifo" {
				if err := syscall.Mkfifo(leaf, 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				listener, err := net.Listen("unix", leaf)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			}
			if _, err := prepareStoreDirectory(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("special leaf accepted: %v", err)
			}
			if _, err := os.Lstat(leaf); err != nil {
				t.Fatalf("special leaf removed: %v", err)
			}
		})
	}
}
