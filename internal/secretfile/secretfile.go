// Package secretfile opens operator-supplied secret files (keys, DSNs) and checks
// that only their owner can read them, allowing for systemd credentials.
package secretfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

var (
	// ErrUnavailable means the file could not be opened.
	ErrUnavailable = errors.New("secret file unavailable")
	// ErrPermissions means the file is not a regular file readable only as allowed.
	ErrPermissions = errors.New("secret file must be a regular owner-only file")
)

// credentialsDirectory is the variable systemd sets for a unit with LoadCredential=.
const credentialsDirectory = "CREDENTIALS_DIRECTORY"

// Open opens path for reading and checks its mode. Outside $CREDENTIALS_DIRECTORY the
// file must give no access to group or others (perm&0o077 == 0). A file directly in
// $CREDENTIALS_DIRECTORY may also be 0440: systemd writes each credential 0400 root and
// grants the DynamicUser read access with an ACL entry user:<uid>:r-- (exec-credential.c,
// write_credential); on a tmpfs with ACL support (kernel >= 6.3, noswap) stat shows the
// ACL mask in the group bits, so the file reads as 0440. On ramfs systemd chowns the file
// to the service user and it stays 0400. Such a file is opened without following a
// symlink, so the relaxed rule never applies to a file elsewhere. Errors carry no path.
func Open(path string) (*os.File, fs.FileInfo, error) {
	inCredentials := InCredentialsDir(path, os.Getenv(credentialsDirectory))
	var f *os.File
	var err error
	if inCredentials {
		f, err = os.OpenFile(filepath.Clean(path), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, ErrPermissions
		}
	} else {
		f, err = os.Open(path) //nolint:gosec // Operator-supplied secret path, never request input.
	}
	if err != nil {
		return nil, nil, ErrUnavailable
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !AllowedMode(info.Mode().Perm(), inCredentials) {
		_ = f.Close()
		return nil, nil, ErrPermissions
	}
	return f, info, nil
}

// InCredentialsDir reports whether path, once cleaned, names an entry directly in the
// absolute directory dir. An empty or relative dir never matches.
func InCredentialsDir(path, dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	return filepath.Dir(filepath.Clean(path)) == filepath.Clean(dir)
}

// AllowedMode reports whether perm is acceptable for a secret file; see Open.
func AllowedMode(perm fs.FileMode, inCredentials bool) bool {
	if inCredentials {
		return perm == 0o400 || perm == 0o440
	}
	return perm&0o077 == 0
}
