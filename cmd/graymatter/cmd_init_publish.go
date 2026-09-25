package main

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type initFilePlan struct {
	path     string
	root     *os.Root
	rel      string
	holder   *os.File
	info     fs.FileInfo
	before   []byte
	after    []byte
	metadata []byte
	status   string
	perm     fs.FileMode
}

var initOpenExistingFile = initOpenRegular

var initOpenStage = initCreateStageFile
var initStageRandom = rand.Reader
var initReplacePublishedFile = initReplaceFile

var initAcquireFileGuard = initGuardFile
var initMakeDir = (*os.Root).Mkdir

var errInitPublicationConflict = errors.New("publication_conflict")

func (p *initFilePlan) close() error {
	var errs []error
	if p.holder != nil {
		errs = append(errs, p.holder.Close())
		p.holder = nil
	}
	if p.root != nil {
		errs = append(errs, p.root.Close())
		p.root = nil
	}
	return errors.Join(errs...)
}

func initAnchor(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", errors.New("path_not_absolute")
	}
	ancestor := filepath.Dir(path)
	for {
		_, err := os.Lstat(ancestor)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return nil, "", errors.New("missing_ancestor")
		}
		ancestor = parent
	}
	physical, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(physical)
	if err != nil {
		return nil, "", err
	}
	rel, err := filepath.Rel(ancestor, path)
	if err != nil || !filepath.IsLocal(rel) {
		_ = root.Close()
		return nil, "", errors.New("invalid_relative_path")
	}
	return root, rel, nil
}

func planInitFile(path string, perm fs.FileMode, render func([]byte, bool) ([]byte, string, error)) (_ *initFilePlan, err error) {
	root, rel, err := initAnchor(path)
	if err != nil {
		return nil, err
	}
	p := &initFilePlan{path: path, root: root, rel: rel, perm: perm}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.close())
		}
	}()
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	} else if err != nil {
		return nil, err
	} else {
		if !info.Mode().IsRegular() {
			return nil, errors.New("unsafe_leaf")
		}
		p.info = info
		p.holder, err = initOpenExistingFile(root, rel)
		if err != nil {
			return nil, err
		}
		opened, statErr := p.holder.Stat()
		if statErr != nil || !os.SameFile(info, opened) {
			return nil, errors.New("file_changed")
		}
		p.before, err = io.ReadAll(io.LimitReader(p.holder, initConfigLimit+1))
		if err != nil {
			return nil, err
		}
		if len(p.before) > initConfigLimit {
			return nil, errors.New("config_too_large")
		}
		if after, statErr := root.Lstat(rel); statErr != nil || !os.SameFile(info, after) {
			return nil, errors.New("file_changed")
		}
	}
	p.after, p.status, err = render(p.before, p.info != nil)
	if err != nil {
		return nil, err
	}
	if len(p.after) > initConfigLimit {
		return nil, errors.New("config_too_large")
	}
	if p.info != nil && p.status != "preserved" && p.status != "unchanged" {
		if err := initCheckMetadata(p.holder, p.info); err != nil {
			return nil, err
		}
		p.metadata, err = initCaptureMetadata(p.holder)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

type initPublishResult struct {
	status    string
	changed   bool
	uncertain bool
	residue   string
	err       error
}

func initMkdirAll(root *os.Root, name string, perm fs.FileMode) (bool, bool, error) {
	if name == "." {
		return false, false, nil
	}
	created := false
	current := ""
	for _, part := range strings.Split(filepath.Clean(name), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := initMakeDir(root, current, perm); err == nil {
			created = true
		} else if errors.Is(err, fs.ErrExist) {
			info, statErr := root.Lstat(current)
			if statErr != nil || info.Mode()&os.ModeType != os.ModeDir {
				return created, statErr != nil, errInitPublicationConflict
			}
		} else {
			return created, true, err
		}
	}
	return created, false, nil
}

func (p *initFilePlan) apply() (out initPublishResult) {
	if p.status == "preserved" || p.status == "unchanged" {
		return initPublishResult{status: p.status}
	}
	parent := filepath.Dir(p.rel)
	if parent != "." {
		// The root is anchored to the nearest preflight ancestor. Root refuses
		// escapes if an intermediate directory is replaced with a symlink.
		created, uncertain, err := initMkdirAll(p.root, parent, 0o755)
		out.changed = created
		if err != nil {
			return initPublishResult{status: "failed", changed: created, uncertain: uncertain, err: err}
		}
	}
	if p.info != nil {
		// Replacements planned against an inode already replaced by another
		// init can fail before creating any staging file.
		current, err := p.root.Lstat(p.rel)
		if err != nil || !os.SameFile(current, p.info) {
			out.status, out.err = "failed", errInitPublicationConflict
			return out
		}
	}
	var stage string
	var file *os.File
	for i := 0; i < 10; i++ {
		var random [16]byte
		if _, err := io.ReadFull(initStageRandom, random[:]); err != nil {
			out.status, out.err = "failed", fmt.Errorf("stage random: %w", err)
			return out
		}
		stage = filepath.Join(parent, fmt.Sprintf(".init-%x.tmp", random))
		var err error
		file, err = initOpenStage(p, stage)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			out.status, out.err = "failed", err
			out.uncertain = true
			_, statErr := p.root.Lstat(stage)
			if statErr == nil {
				out.residue = filepath.Join(p.root.Name(), stage)
			}
			return out
		}
		break
	}
	if file == nil {
		out.status, out.err = "failed", errors.New("stage_name_conflict")
		return out
	}
	out.changed = true
	out.status = "failed"
	stageInfo, _ := file.Stat()
	cleanup := func() error {
		current, err := p.root.Lstat(stage)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || current == nil || stageInfo == nil || !os.SameFile(current, stageInfo) {
			return errors.New("stage_cleanup_uncertain")
		}
		return p.root.Remove(stage)
	}
	defer func() {
		if out.status == "publication_unknown" {
			if current, err := p.root.Lstat(stage); err == nil && stageInfo != nil && os.SameFile(current, stageInfo) {
				out.residue = filepath.Join(p.root.Name(), stage)
			}
			return
		}
		if err := cleanup(); err != nil {
			out.residue = filepath.Join(p.root.Name(), stage)
			out.uncertain = true
			out.err = errors.Join(out.err, err)
			if out.status == "created" || out.status == "updated" {
				out.status = "applied_cleanup_failed"
			}
		}
	}()
	var writeErr error
	if p.info != nil {
		// A replacement stage must have the source metadata before it receives
		// any content. A mismatched ACL must fail while the stage is empty.
		writeErr = initVerifyPreflightMetadata(p.holder, p.metadata)
		if writeErr == nil {
			writeErr = initStageMetadataBeforeWrite(file, p.holder, p.info, p.metadata)
		}
	}
	if writeErr == nil {
		var n int
		n, writeErr = file.Write(p.after)
		if writeErr == nil && n != len(p.after) {
			writeErr = io.ErrShortWrite
		}
	}
	if writeErr == nil && p.info == nil {
		writeErr = file.Chmod(p.perm)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		if writeErr != nil {
			writeErr = fmt.Errorf("stage write or sync: %w", writeErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("stage close: %w", closeErr)
		}
		out.err = errors.Join(writeErr, closeErr)
		return out
	}
	if p.info == nil {
		if _, err := p.root.Lstat(p.rel); !errors.Is(err, fs.ErrNotExist) {
			out.err = errors.New("publication_conflict")
			return out
		}
		if err := p.root.Link(stage, p.rel); err != nil {
			out.err = err
			after, statErr := p.root.Lstat(p.rel)
			if statErr == nil && stageInfo != nil && os.SameFile(after, stageInfo) {
				out.status = "applied_cleanup_failed"
			} else if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
				out.uncertain = true
				out.status = "publication_unknown"
			}
			return out
		}
		out.status = "created"
		return out
	}
	guard, err := initAcquireFileGuard(p.holder, p.info)
	if err != nil {
		out.err = err
		return out
	}
	defer func() {
		if releaseErr := guard(); releaseErr != nil {
			out.err = errors.Join(out.err, releaseErr)
			out.uncertain = true
			if out.status == "updated" || out.status == "created" {
				out.status = "applied_cleanup_failed"
			}
		}
	}()
	current, err := p.root.Lstat(p.rel)
	if err != nil || !os.SameFile(current, p.info) || current.Mode() != p.info.Mode() || !current.ModTime().Equal(p.info.ModTime()) || current.Size() != p.info.Size() {
		out.err = errors.New("publication_conflict")
		return out
	}
	fresh, err := initOpenRegular(p.root, p.rel)
	if err != nil {
		out.err = err
		return out
	}
	freshInfo, err := fresh.Stat()
	if err == nil && !os.SameFile(freshInfo, p.info) {
		err = errors.New("publication_conflict")
	}
	var content []byte
	if err == nil {
		content, err = io.ReadAll(io.LimitReader(fresh, initConfigLimit+1))
	}
	err = errors.Join(err, fresh.Close())
	if err != nil || sha256.Sum256(content) != sha256.Sum256(p.before) {
		out.err = errors.New("publication_conflict")
		return out
	}
	stagedInfo, err := p.root.Lstat(stage)
	if err != nil || stageInfo == nil || !os.SameFile(stagedInfo, stageInfo) {
		out.err = errors.New("publication_conflict")
		return out
	}
	if err := initRestoreStageMetadata(p.root, stage, stageInfo, p.metadata); err != nil {
		out.err = err
		return out
	}
	staged, err := initOpenRegular(p.root, stage)
	if err != nil {
		out.err = err
		return out
	}
	openedStageInfo, err := staged.Stat()
	if err == nil && !os.SameFile(openedStageInfo, stageInfo) {
		err = errors.New("publication_conflict")
	}
	if err == nil {
		err = initVerifyMetadata(p.holder, p.info, staged, p.metadata)
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		out.err = err
		return out
	}
	if err := initReplacePublishedFile(p.root, stage, p.rel); err != nil {
		out.err = err
		targetNow, targetErr := p.root.Lstat(p.rel)
		stageNow, stageErr := p.root.Lstat(stage)
		switch {
		case targetErr == nil && stageInfo != nil && os.SameFile(targetNow, stageInfo):
			out.status = "applied_cleanup_failed"
		case targetErr == nil && os.SameFile(targetNow, p.info) && stageErr == nil &&
			stageInfo != nil && os.SameFile(stageNow, stageInfo):
			out.status = "failed"
		default:
			out.uncertain = true
			out.status = "publication_unknown"
		}
		return out
	}
	out.status = "updated"
	return out
}
