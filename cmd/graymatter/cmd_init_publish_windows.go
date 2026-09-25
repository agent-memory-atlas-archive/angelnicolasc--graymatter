//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

var initReadWindowsSecurityInfo = windows.GetSecurityInfo

func initWindowsFileID(file *os.File) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info)
	return info, err
}

func initCheckMetadata(file *os.File, info fs.FileInfo) error {
	id, err := initWindowsFileID(file)
	if err != nil || id.NumberOfLinks != 1 {
		return errors.New("unsupported_metadata")
	}
	const ordinaryAttributes = windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_ATTRIBUTE_ARCHIVE
	if id.FileAttributes&^ordinaryAttributes != 0 {
		return errors.New("unsupported_metadata")
	}
	if !initWindowsSingleStream(file) {
		return errors.New("unsupported_metadata")
	}
	// An anchored rename installs the stage inode. If the caller cannot read
	// the complete audit SACL, it cannot prove that the replacement preserves
	// the target's security descriptor. Ordinary user tokens commonly lack
	// ACCESS_SYSTEM_SECURITY, so reject the mutation during preflight.
	if _, err := initReadWindowsSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.SACL_SECURITY_INFORMATION); err != nil {
		return errors.New("unsupported_metadata")
	}
	return nil
}

func initStageMetadata(stage, source *os.File, info fs.FileInfo) error {
	return initCompareStageMetadata(stage, source, info, false)
}

func initStageMetadataBeforeWrite(stage, source *os.File, info fs.FileInfo, snapshot []byte) error {
	if err := initVerifyPreflightMetadata(source, snapshot); err != nil {
		return err
	}
	return initCompareStageMetadata(stage, source, info, true)
}

func initCompareStageMetadata(stage, source *os.File, info fs.FileInfo, protectedStage bool) error {
	oldID, err := initWindowsFileID(source)
	if err != nil {
		return errors.New("unsupported_metadata")
	}
	newID, err := initWindowsFileID(stage)
	if err != nil || oldID.FileAttributes != newID.FileAttributes || !initWindowsSingleStream(stage) {
		return errors.New("unsupported_metadata")
	}
	for _, kind := range []windows.SECURITY_INFORMATION{
		windows.DACL_SECURITY_INFORMATION,
		windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION,
		windows.LABEL_SECURITY_INFORMATION,
		windows.SACL_SECURITY_INFORMATION,
	} {
		oldSD, oldErr := initReadWindowsSecurityInfo(windows.Handle(source.Fd()), windows.SE_FILE_OBJECT, kind)
		newSD, newErr := initReadWindowsSecurityInfo(windows.Handle(stage.Fd()), windows.SE_FILE_OBJECT, kind)
		if oldErr != nil || newErr != nil || oldSD == nil || newSD == nil ||
			!initStageSecurityEqual(oldSD, newSD, protectedStage &&
				(kind == windows.DACL_SECURITY_INFORMATION ||
					kind == windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)) {
			return errors.New("unsupported_metadata")
		}
	}
	return nil
}

func initStageSecurityEqual(oldSD, newSD *windows.SECURITY_DESCRIPTOR, allowProtectionControl bool) bool {
	oldLength, newLength := oldSD.Length(), newSD.Length()
	if oldLength == 0 || oldLength != newLength {
		return false
	}
	oldBytes := unsafe.Slice((*byte)(unsafe.Pointer(oldSD)), oldLength)
	newBytes := unsafe.Slice((*byte)(unsafe.Pointer(newSD)), newLength)
	if !allowProtectionControl {
		return bytes.Equal(oldBytes, newBytes)
	}
	if oldLength < 4 {
		return false
	}
	oldControl := windows.SECURITY_DESCRIPTOR_CONTROL(binary.LittleEndian.Uint16(oldBytes[2:4]))
	newControl := windows.SECURITY_DESCRIPTOR_CONTROL(binary.LittleEndian.Uint16(newBytes[2:4]))
	const allowed = windows.SE_DACL_PROTECTED | windows.SE_DACL_AUTO_INHERITED
	if newControl&windows.SE_DACL_PROTECTED == 0 || oldControl&^allowed != newControl&^allowed {
		return false
	}
	return bytes.Equal(oldBytes[:2], newBytes[:2]) && bytes.Equal(oldBytes[4:], newBytes[4:])
}

func initWindowsSingleStream(file *os.File) bool {
	var buffer [512]uint64 // FILE_STREAM_INFO requires aligned storage.
	if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileStreamInfo,
		(*byte)(unsafe.Pointer(&buffer[0])), uint32(unsafe.Sizeof(buffer))); err != nil {
		return false
	}
	return *(*uint32)(unsafe.Pointer(&buffer[0])) == 0
}

func initCaseSensitiveRoot(root *os.Root) bool {
	directory, err := root.Open(".")
	if err != nil {
		return false // Conservatively report an alias when sensitivity is unknown.
	}
	defer directory.Close()
	var flags uint32
	if err := windows.GetFileInformationByHandleEx(windows.Handle(directory.Fd()), windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags))); err != nil {
		return false
	}
	return flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0
}

func initGuardFile(file *os.File, info fs.FileInfo) (func() error, error) {
	id, err := initWindowsFileID(file)
	if err != nil {
		return nil, err
	}
	name := initWindowsMutexName(id)
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("initialization_busy")
	}
	owner := user.User.Sid
	ownerText := owner.String()
	if ownerText == "" {
		return nil, errors.New("initialization_busy")
	}
	const rights = windows.SYNCHRONIZE | windows.MUTEX_MODIFY_STATE | windows.READ_CONTROL
	// Supply an explicit protected DACL. CreateMutexEx also requests only the
	// three rights used below, rather than MUTEX_ALL_ACCESS for an existing name.
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"O:%sD:P(A;;0x%08x;;;%s)(A;;0x%08x;;;SY)", ownerText, rights, ownerText, rights))
	if err != nil || sd == nil || !sd.IsValid() {
		return nil, errors.New("initialization_busy")
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	runtime.LockOSThread()
	handle, err := windows.CreateMutexEx(&attrs, ptr, 0, rights)
	if err != nil && (!errors.Is(err, windows.ERROR_ALREADY_EXISTS) || handle == 0) {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		runtime.UnlockOSThread()
		return nil, err
	}
	// SecurityAttributes is ignored when the name already exists. Reject a
	// squatted or broadened object before waiting on it.
	if err := initVerifyWindowsMutexSecurity(handle, owner, rights); err != nil {
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		return nil, err
	}
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || (state != windows.WAIT_OBJECT_0 && state != windows.WAIT_ABANDONED) {
		_ = windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("initialization_busy")
	}
	return func() error {
		// Mutex ownership belongs to this OS thread. CloseHandle alone does
		// not release it, and the goroutine must remain pinned until release.
		releaseErr := windows.ReleaseMutex(handle)
		closeErr := windows.CloseHandle(handle)
		runtime.UnlockOSThread()
		return errors.Join(releaseErr, closeErr)
	}, nil
}

func initWindowsMutexName(id windows.ByHandleFileInformation) string {
	return fmt.Sprintf("Local\\GrayMatterInit-%08x-%08x-%08x", id.VolumeSerialNumber, id.FileIndexHigh, id.FileIndexLow)
}

func initVerifyWindowsMutexSecurity(handle windows.Handle, owner *windows.SID, rights windows.ACCESS_MASK) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || sd == nil {
		return errors.New("initialization_busy")
	}
	actualOwner, _, err := sd.Owner()
	if err != nil || actualOwner == nil || !actualOwner.Equals(owner) {
		return errors.New("initialization_busy")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 || control&windows.SE_DACL_PRESENT == 0 {
		return errors.New("initialization_busy")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return errors.New("initialization_busy")
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return errors.New("initialization_busy")
	}
	seenOwner, seenSystem := false, false
	for i := uint32(0); i < 2; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil ||
			ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 ||
			ace.Mask != rights {
			return errors.New("initialization_busy")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return errors.New("initialization_busy")
		}
		switch {
		case sid.Equals(owner) && !seenOwner:
			seenOwner = true
		case sid.Equals(system) && !seenSystem:
			seenSystem = true
		default:
			return errors.New("initialization_busy")
		}
	}
	if !seenOwner || !seenSystem {
		return errors.New("initialization_busy")
	}
	return nil
}

func initReplaceFile(root *os.Root, stage, target string) error {
	// Go's Windows Root.Rename calls handle-relative Renameat. It uses NT
	// RootDirectory and replace semantics, so replacing an intermediate path
	// cannot redirect this publication to an unrelated directory.
	return root.Rename(stage, target)
}

func initOpenRegular(root *os.Root, rel string) (*os.File, error) {
	file, err := root.Open(rel)
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

func initCaptureMetadata(file *os.File) ([]byte, error) {
	id, err := initWindowsFileID(file)
	if err != nil {
		return nil, errors.New("unsupported_metadata")
	}
	snapshot := make([]byte, 8)
	binary.LittleEndian.PutUint32(snapshot[:4], id.FileAttributes)
	binary.LittleEndian.PutUint32(snapshot[4:], id.NumberOfLinks)
	for _, kind := range []windows.SECURITY_INFORMATION{
		windows.DACL_SECURITY_INFORMATION,
		windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION,
		windows.LABEL_SECURITY_INFORMATION,
		windows.SACL_SECURITY_INFORMATION,
	} {
		sd, err := initReadWindowsSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, kind)
		if err != nil || sd == nil || !sd.IsValid() || sd.Length() == 0 || sd.Length() > 64<<10 {
			return nil, errors.New("unsupported_metadata")
		}
		var size [4]byte
		binary.LittleEndian.PutUint32(size[:], sd.Length())
		snapshot = append(snapshot, size[:]...)
		snapshot = append(snapshot, unsafe.Slice((*byte)(unsafe.Pointer(sd)), sd.Length())...)
	}
	return snapshot, nil
}

func initSnapshotDescriptor(snapshot []byte, index int) (*windows.SECURITY_DESCRIPTOR, []uint64, error) {
	offset := 8 // FileAttributes and NumberOfLinks precede the descriptors.
	for i := 0; i <= index; i++ {
		if len(snapshot) < offset+4 {
			return nil, nil, errors.New("unsupported_metadata")
		}
		size := int(binary.LittleEndian.Uint32(snapshot[offset : offset+4]))
		offset += 4
		if size == 0 || size > 64<<10 || len(snapshot) < offset+size {
			return nil, nil, errors.New("unsupported_metadata")
		}
		if i == index {
			aligned := make([]uint64, (size+7)/8)
			copy(unsafe.Slice((*byte)(unsafe.Pointer(&aligned[0])), size), snapshot[offset:offset+size])
			sd := (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(&aligned[0]))
			if !sd.IsValid() {
				return nil, nil, errors.New("unsupported_metadata")
			}
			return sd, aligned, nil
		}
		offset += size
	}
	return nil, nil, errors.New("unsupported_metadata")
}

func initVerifyPreflightMetadata(file *os.File, snapshot []byte) error {
	current, err := initCaptureMetadata(file)
	if err != nil || !bytes.Equal(current, snapshot) {
		return errors.New("unsupported_metadata")
	}
	return nil
}

func initPrepareStageMetadata(stage *os.File, snapshot []byte) error {
	daclSD, daclBacking, err := initSnapshotDescriptor(snapshot, 0)
	if err != nil {
		return err
	}
	combinedSD, combinedBacking, err := initSnapshotDescriptor(snapshot, 1)
	if err != nil {
		return err
	}
	control, _, err := combinedSD.Control()
	if err != nil {
		return errors.New("unsupported_metadata")
	}
	dacl, _, err := daclSD.DACL()
	if err != nil || dacl == nil {
		return errors.New("unsupported_metadata")
	}
	kind := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		kind = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	err = windows.SetSecurityInfo(windows.Handle(stage.Fd()), windows.SE_FILE_OBJECT, kind,
		nil, nil, dacl, nil)
	runtime.KeepAlive(daclBacking)
	runtime.KeepAlive(combinedBacking)
	if err != nil {
		return errors.New("unsupported_metadata")
	}
	return nil
}

func initRestoreStageMetadata(root *os.Root, name string, expected fs.FileInfo, snapshot []byte) error {
	parent, err := root.Open(filepath.Dir(name))
	if err != nil {
		return errors.New("unsupported_metadata")
	}
	defer parent.Close()
	leaf, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return errors.New("unsupported_metadata")
	}
	object := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()), ObjectName: leaf,
		Attributes: windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	var ioStatus windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.WRITE_DAC|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		object, &ioStatus, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0, 0)
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return errors.New("unsupported_metadata")
	}
	file := os.NewFile(uintptr(handle), filepath.Join(root.Name(), name))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return errors.New("unsupported_metadata")
	}
	current, err := file.Stat()
	if err == nil && !os.SameFile(current, expected) {
		err = errors.New("publication_conflict")
	}
	if err == nil {
		err = initPrepareStageMetadata(file, snapshot)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return errors.New("unsupported_metadata")
	}
	return nil
}

func initCreateStageFile(p *initFilePlan, name string) (*os.File, error) {
	// Open the parent through the preflight root and create the leaf relative
	// to that handle. A lexical parent swap cannot redirect the stage.
	parent, err := p.root.Open(filepath.Dir(name))
	if err != nil {
		return nil, fmt.Errorf("open stage parent: %w", err)
	}
	defer parent.Close()
	leaf, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return nil, fmt.Errorf("encode stage leaf: %w", err)
	}
	var sd *windows.SECURITY_DESCRIPTOR
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if p.info != nil {
		current, err := initCaptureMetadata(p.holder)
		if err != nil || !bytes.Equal(current, p.metadata) {
			return nil, errors.New("unsupported_metadata")
		}
		id, err := initWindowsFileID(p.holder)
		if err != nil {
			return nil, errors.New("unsupported_metadata")
		}
		attrs = id.FileAttributes
		// Use the exact preflight DACL. A fresh read could race with an ACL
		// expansion after the snapshot and create a more permissive stage.
		var backing []uint64
		sd, backing, err = initSnapshotDescriptor(p.metadata, 0)
		if err != nil {
			return nil, errors.New("unsupported_metadata")
		}
		defer runtime.KeepAlive(backing)
		// A source DACL may be unprotected. Force protection on the birth
		// descriptor so a parent ACL change cannot add an ACE to the empty
		// stage that a reader holds open until content is written. The
		// original inheritance control is restored under the file guard just
		// before publication, after the content has been written and closed.
		absolute, err := sd.ToAbsolute()
		if err != nil || absolute == nil ||
			absolute.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED) != nil {
			return nil, errors.New("unsupported_metadata")
		}
		sd, err = absolute.ToSelfRelative()
		if err != nil || sd == nil || !sd.IsValid() {
			return nil, errors.New("unsupported_metadata")
		}
	} else {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
			return nil, errors.New("unsupported_metadata")
		}
		owner := user.User.Sid.String()
		if owner == "" {
			return nil, errors.New("unsupported_metadata")
		}
		// A new stage is private from birth, even when its parent is shared.
		sd, err = windows.SecurityDescriptorFromString(fmt.Sprintf(
			"D:P(A;;FA;;;%s)(A;;FA;;;SY)", owner))
		if err != nil || sd == nil || !sd.IsValid() {
			return nil, errors.New("unsupported_metadata")
		}
	}
	object := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()), ObjectName: leaf,
		Attributes:         windows.OBJ_DONT_REPARSE | windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: sd,
	}
	var handle windows.Handle
	var ioStatus windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.WRITE_DAC,
		object, &ioStatus, nil, attrs,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			return nil, fs.ErrExist
		}
		return nil, fmt.Errorf("create stage: %w", err)
	}
	file := os.NewFile(uintptr(handle), filepath.Join(p.root.Name(), name))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("stage_open_failed")
	}
	return file, nil
}

func initVerifyMetadata(source *os.File, preflight fs.FileInfo, stage *os.File, snapshot []byte) error {
	current, err := source.Stat()
	if err != nil || !os.SameFile(preflight, current) || current.Mode() != preflight.Mode() {
		return errors.New("unsupported_metadata")
	}
	if err := initCheckMetadata(source, current); err != nil {
		return err
	}
	latest, err := initCaptureMetadata(source)
	if err != nil || !bytes.Equal(snapshot, latest) {
		return errors.New("unsupported_metadata")
	}
	return initStageMetadata(stage, source, current)
}
