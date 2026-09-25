//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows/registry"
)

func preflightUserPath() error {
	if _, err := os.Executable(); err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.READ)
	if err != nil {
		return err
	}
	defer key.Close()
	_, kind, err := key.GetStringValue("Path")
	if err == registry.ErrNotExist {
		return nil
	}
	if err != nil {
		return err
	}
	if kind != registry.SZ && kind != registry.EXPAND_SZ {
		return registry.ErrUnexpectedType
	}
	return nil
}
