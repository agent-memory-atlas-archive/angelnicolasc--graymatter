//go:build linux || darwin

package main

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func initCheckMetadata(file *os.File, info fs.FileInfo) error {
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) {
		return errors.New("unsupported_metadata")
	}
	stat, ok := current.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("unsupported_metadata")
	}
	if current.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("unsupported_metadata")
	}
	n, err := unix.Flistxattr(int(file.Fd()), nil)
	if err != nil || n != 0 {
		return errors.New("unsupported_metadata")
	}
	return nil
}

func initStageMetadata(stage, source *os.File, info fs.FileInfo) error {
	old, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("unsupported_metadata")
	}
	staged, err := stage.Stat()
	if err != nil {
		return err
	}
	current, ok := staged.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("unsupported_metadata")
	}
	if old.Uid != current.Uid || old.Gid != current.Gid {
		if err := stage.Chown(int(old.Uid), int(old.Gid)); err != nil {
			return err
		}
	}
	return stage.Chmod(info.Mode().Perm())
}

func initGuardFile(file *os.File, info fs.FileInfo) (func() error, error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("initialization_busy")
		}
		return nil, err
	}
	return func() error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }, nil
}

func initReplaceFile(root *os.Root, stage, target string) error {
	return root.Rename(stage, target)
}

func initCaseSensitiveRoot(root *os.Root) bool { return true }

func initOpenRegular(root *os.Root, rel string) (*os.File, error) {
	// A leaf may turn into a FIFO after Lstat. O_NONBLOCK prevents preflight
	// from waiting for a writer before the handle's type is checked.
	file, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("unsafe_leaf")
	}
	return file, nil
}

func initCaptureMetadata(_ *os.File) ([]byte, error) { return nil, nil }

func initPrepareStageMetadata(_ *os.File, _ []byte) error { return nil }

func initVerifyPreflightMetadata(_ *os.File, _ []byte) error { return nil }

func initStageMetadataBeforeWrite(stage, source *os.File, info fs.FileInfo, _ []byte) error {
	return initStageMetadata(stage, source, info)
}

func initRestoreStageMetadata(_ *os.Root, _ string, _ fs.FileInfo, _ []byte) error { return nil }

func initCreateStageFile(p *initFilePlan, name string) (*os.File, error) {
	return p.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func initVerifyMetadata(source *os.File, preflight fs.FileInfo, stage *os.File, _ []byte) error {
	current, err := source.Stat()
	if err != nil || !os.SameFile(preflight, current) || current.Mode() != preflight.Mode() {
		return errors.New("unsupported_metadata")
	}
	before, beforeOK := preflight.Sys().(*syscall.Stat_t)
	now, nowOK := current.Sys().(*syscall.Stat_t)
	if !beforeOK || !nowOK || before.Uid != now.Uid || before.Gid != now.Gid || before.Nlink != now.Nlink {
		return errors.New("unsupported_metadata")
	}
	if err := initCheckMetadata(source, current); err != nil {
		return err
	}
	stagedInfo, err := stage.Stat()
	if err != nil || stagedInfo.Mode() != current.Mode() {
		return errors.New("unsupported_metadata")
	}
	staged, ok := stagedInfo.Sys().(*syscall.Stat_t)
	if !ok || staged.Uid != now.Uid || staged.Gid != now.Gid {
		return errors.New("unsupported_metadata")
	}
	n, err := unix.Flistxattr(int(stage.Fd()), nil)
	if err != nil || n != 0 {
		return errors.New("unsupported_metadata")
	}
	return nil
}
