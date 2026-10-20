package setup

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isRemoteDrive reports whether dir's drive is a network drive.
func isRemoteDrive(dir string) (bool, error) {
	volume := filepath.VolumeName(dir)
	if volume == "" {
		return false, nil
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return false, err
	}
	return windows.GetDriveType(root) == windows.DRIVE_REMOTE, nil
}

// fileIDInfo is FILE_ID_INFO: the volume serial number and the 128-bit file
// ID that name one file or folder whatever path reaches it.
type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

// folderIdentity is dir's volume serial number and file ID
// (GetFileInformationByHandleEx, FileIdInfo), the same for every path that
// reaches the folder: an 8.3 short name, a trailing dot, another case.
func folderIdentity(dir string) (string, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", err
	}
	// FILE_FLAG_OPEN_REPARSE_POINT names a link itself, never its target.
	handle, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrSelection, dir, err)
	}
	defer windows.CloseHandle(handle)
	var info fileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrSelection, dir, err)
	}
	return fmt.Sprintf("%016x-%s", info.VolumeSerialNumber, hex.EncodeToString(info.FileID[:])), nil
}

// folderOwner is the owner SID of path; tests replace it.
var folderOwner = func(path string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	return owner, err
}

// checkParents refuses dir when an existing parent below its volume root is
// owned by an account other than this one, SYSTEM or Administrators: such an
// owner could replace the folder or change its access.
func checkParents(dir string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	allowed := []*windows.SID{user.User.Sid}
	for _, known := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(known)
		if err != nil {
			return err
		}
		allowed = append(allowed, sid)
	}
	root := filepath.VolumeName(dir) + `\`
	for parent := filepath.Dir(filepath.Clean(dir)); parent != root && parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		owner, err := folderOwner(parent)
		if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: the owner of %s: %w", ErrSelection, parent, err)
		}
		trusted := false
		for _, sid := range allowed {
			trusted = trusted || owner != nil && owner.Equals(sid)
		}
		if !trusted {
			return fmt.Errorf("%w: %s is owned by %v, not this account, SYSTEM or Administrators", ErrSelection, parent, owner)
		}
	}
	return nil
}
