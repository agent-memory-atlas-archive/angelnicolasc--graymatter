//go:build !windows

package usage

import "os/exec"

func hideCommandWindow(cmd *exec.Cmd) {}
