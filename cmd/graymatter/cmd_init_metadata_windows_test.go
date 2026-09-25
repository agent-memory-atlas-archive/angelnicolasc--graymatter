package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func initAllowAuditSACLForTest(t *testing.T) {
	t.Helper()
	// These fixtures exercise DACL, file identity, and publication behavior
	// under ordinary Windows tokens. Audit and mandatory-label reads can be
	// denied by the runner; supply stable descriptors for those two fields
	// only. Production still requires both real descriptors and rejects a
	// replacement when either cannot be inspected.
	audit, err := windows.SecurityDescriptorFromString("S:(AU;SA;FA;;;WD)")
	if err != nil || audit == nil || !audit.IsValid() {
		t.Fatalf("synthetic audit descriptor: %v", err)
	}
	label, err := windows.SecurityDescriptorFromString("S:(ML;;NW;;;ME)")
	if err != nil || label == nil || !label.IsValid() {
		t.Fatalf("synthetic label descriptor: %v", err)
	}
	previous := initReadWindowsSecurityInfo
	initReadWindowsSecurityInfo = func(handle windows.Handle, objectType windows.SE_OBJECT_TYPE,
		kind windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
		switch kind {
		case windows.SACL_SECURITY_INFORMATION:
			return audit, nil
		case windows.LABEL_SECURITY_INFORMATION:
			return label, nil
		}
		return previous(handle, objectType, kind)
	}
	t.Cleanup(func() { initReadWindowsSecurityInfo = previous })
}

func TestInitI10WindowsRejectsSquattedMutex(t *testing.T) {
	initAllowAuditSACLForTest(t)
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"owner":"before"}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	id, err := initWindowsFileID(plan.holder)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(initWindowsMutexName(id))
	if err != nil {
		t.Fatal(err)
	}
	// A broad pre-existing DACL must not be accepted merely because the
	// process can open and wait on the named object.
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateMutexEx(&attrs, name, 0, windows.MUTEX_ALL_ACCESS)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	out := plan.apply()
	if out.status != "failed" || out.err == nil || out.err.Error() != "initialization_busy" {
		t.Fatalf("squatted mutex accepted: %+v", out)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("squatted mutex changed target: %s err=%v", data, err)
	}
}

func TestInitI09WindowsRejectsACLDriftToStageDefaults(t *testing.T) {
	initAllowAuditSACLForTest(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "config.json")
	sibling := filepath.Join(parent, "sibling.json")
	before := []byte(`{"owner":"before"}`)
	for _, name := range []string{path, sibling} {
		if err := os.WriteFile(name, before, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("current user SID: %v", err)
	}
	custom, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;;GA;;;%s)(A;;GA;;;SY)", user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	customDACL, _, err := custom.DACL()
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, customDACL, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	fresh, err := os.Open(sibling)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	defaultSD, err := windows.GetSecurityInfo(windows.Handle(fresh.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	defaultDACL, _, err := defaultSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, defaultDACL, nil); err != nil {
		t.Fatal(err)
	}
	// The new ACL now matches a fresh sibling (and the staged file), so
	// comparing source only with stage would miss the preflight drift.
	if err := initStageMetadata(fresh, plan.holder, plan.info); err != nil {
		t.Fatalf("ACL drift fixture does not match stage defaults: %v", err)
	}
	out := plan.apply()
	if out.status != "failed" || out.err == nil || out.err.Error() != "unsupported_metadata" {
		t.Fatalf("ACL drift accepted: %+v", out)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("ACL drift changed target: %s err=%v", data, err)
	}
}

func TestInitI09WindowsRejectsAttributeDriftToStageDefaults(t *testing.T) {
	initAllowAuditSACLForTest(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "config.json")
	sibling := filepath.Join(parent, "sibling.json")
	before := []byte(`{"owner":"before"}`)
	for _, name := range []string{path, sibling} {
		if err := os.WriteFile(name, before, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(name, windows.FILE_ATTRIBUTE_NORMAL); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if err := windows.SetFileAttributes(name, windows.FILE_ATTRIBUTE_ARCHIVE); err != nil {
		t.Fatal(err)
	}
	fresh, err := os.Open(sibling)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initStageMetadata(fresh, plan.holder, plan.info); err != nil {
		t.Fatalf("attribute drift fixture does not match stage defaults: %v", err)
	}
	out := plan.apply()
	if out.status != "failed" || out.err == nil || out.err.Error() != "unsupported_metadata" {
		t.Fatalf("attribute drift accepted: %+v", out)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("attribute drift changed target: %s err=%v", data, err)
	}
}

func TestInitI09WindowsStageBirthMetadataBeforeContent(t *testing.T) {
	initAllowAuditSACLForTest(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"owner":"before"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	stageName := ".init-stage-birth-test.tmp"
	stage, err := initCreateStageFile(plan, stageName)
	if err != nil {
		t.Fatalf("stage creation: %T %v", err, err)
	}
	defer plan.root.Remove(stageName)
	defer stage.Close()
	if info, err := stage.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("new stage is not empty: info=%v err=%v", info, err)
	}
	if err := initStageMetadataBeforeWrite(stage, plan.holder, plan.info, plan.metadata); err != nil {
		t.Fatalf("birth stage metadata: %T %v", err, err)
	}
	if err := initPrepareStageMetadata(stage, plan.metadata); err != nil {
		t.Fatalf("prepare empty stage metadata: %T %v", err, err)
	}
	if err := initStageMetadata(stage, plan.holder, plan.info); err != nil {
		t.Fatalf("stage metadata before content: %T %v", err, err)
	}
	if _, err := stage.Write([]byte(`{"owner":"after"}`)); err != nil {
		t.Fatalf("stage write: %T %v", err, err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatalf("stage sync: %T %v", err, err)
	}
}

func TestInitI09WindowsPrivateStageUnderReadableParent(t *testing.T) {
	initAllowAuditSACLForTest(t)
	parent := t.TempDir()
	path := filepath.Join(parent, "config.json")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("current user SID: %v", err)
	}
	// The parent grants read access to Everyone and would pass that grant to
	// an ordinary temporary file. The existing config has a private DACL.
	parentSD, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FR;;;WD)", user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	parentDACL, _, err := parentSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	parentName, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		t.Fatal(err)
	}
	parentHandle, err := windows.CreateFile(parentName, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(parentHandle)
	if err := windows.SetSecurityInfo(parentHandle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, parentDACL, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"secret":"before"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	privateSD, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;;FA;;;%s)(A;;FA;;;SY)", user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	privateDACL, _, err := privateSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	targetName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	targetHandle, err := windows.CreateFile(targetName, windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(targetHandle)
	if err := windows.SetSecurityInfo(targetHandle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, privateDACL, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"secret":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	stageName := ".init-private-stage-test.tmp"
	stage, err := initCreateStageFile(plan, stageName)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.root.Remove(stageName)
	defer stage.Close()
	if info, err := stage.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("stage must be empty at creation: info=%v err=%v", info, err)
	}
	stageSD, err := windows.GetSecurityInfo(windows.Handle(stage.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := stageSD.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("stage inherited parent DACL: control=%x err=%v", control, err)
	}
	stageDACL, _, err := stageSD.DACL()
	if err != nil || stageDACL == nil || stageDACL.AceCount != 2 {
		t.Fatalf("stage DACL is not private: ACL=%v err=%v", stageDACL, err)
	}
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < uint32(stageDACL.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(stageDACL, i, &ace); err != nil || ace == nil {
			t.Fatalf("stage ACE %d: %v", i, err)
		}
		if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); sid.Equals(everyone) {
			t.Fatal("stage granted Everyone access")
		}
	}
	if err := initStageMetadataBeforeWrite(stage, plan.holder, plan.info, plan.metadata); err != nil {
		t.Fatalf("private stage metadata before content: %v", err)
	}
	if _, err := stage.Write([]byte(`{"secret":"after"}`)); err != nil {
		t.Fatal(err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := plan.root.Remove(stageName); err != nil {
		t.Fatal(err)
	}
	if out := plan.apply(); out.err != nil || out.status != "updated" || !out.changed {
		t.Fatalf("private replacement failed: %+v", out)
	}
}

func TestInitI09UnreadableAuditSACLRejectsBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"mcpServers":{}}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	readSecurityInfo := initReadWindowsSecurityInfo
	initReadWindowsSecurityInfo = func(handle windows.Handle, objectType windows.SE_OBJECT_TYPE,
		kind windows.SECURITY_INFORMATION) (*windows.SECURITY_DESCRIPTOR, error) {
		if kind == windows.SACL_SECURITY_INFORMATION {
			return nil, windows.ERROR_ACCESS_DENIED
		}
		return readSecurityInfo(handle, objectType, kind)
	}
	t.Cleanup(func() { initReadWindowsSecurityInfo = readSecurityInfo })
	plan, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if plan != nil || err == nil || err.Error() != "unsupported_metadata" {
		t.Fatalf("unreadable audit SACL accepted: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, before) {
		t.Fatalf("target changed: %v", err)
	}
	currentInfo, err := os.Stat(path)
	if err != nil || !os.SameFile(oldInfo, currentInfo) || !oldInfo.ModTime().Equal(currentInfo.ModTime()) {
		t.Fatalf("target metadata changed: %v", err)
	}
}

func TestInitI09WindowsAlternateStreamRejectsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stream := path + ":owner-data"
	if err := os.WriteFile(stream, []byte("private"), 0o600); err != nil {
		t.Skipf("fixture filesystem does not support alternate streams: %v", err)
	}
	_, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err == nil || err.Error() != "unsupported_metadata" {
		t.Fatalf("alternate stream was accepted: %v", err)
	}
	data, err := os.ReadFile(stream)
	if err != nil || string(data) != "private" {
		t.Fatalf("alternate stream changed: %q %v", data, err)
	}
}
