package secretfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod: WriteFile applies the umask.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func TestUnitOpenChecksModeByLocation(t *testing.T) {
	credentials, outside := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name   string
		dir    string // CREDENTIALS_DIRECTORY; "-" leaves it unset
		inside bool
		perm   fs.FileMode
		want   error
	}{
		{"credential 0400", credentials, true, 0o400, nil},
		{"credential 0440 (systemd ACL mask)", credentials, true, 0o440, nil},
		{"credential group write", credentials, true, 0o460, ErrPermissions},
		{"credential other read", credentials, true, 0o444, ErrPermissions},
		{"credential owner write", credentials, true, 0o640, ErrPermissions},
		{"credential owner read-write", credentials, true, 0o600, ErrPermissions},
		{"0440 outside the credentials directory", credentials, false, 0o440, ErrPermissions},
		{"0600 outside the credentials directory", credentials, false, 0o600, nil},
		{"no CREDENTIALS_DIRECTORY: 0400", "-", true, 0o400, nil},
		{"no CREDENTIALS_DIRECTORY: 0600", "-", true, 0o600, nil},
		{"no CREDENTIALS_DIRECTORY: 0440", "-", true, 0o440, ErrPermissions},
		{"relative CREDENTIALS_DIRECTORY: 0440", "relative", true, 0o440, ErrPermissions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dir == "-" {
				t.Setenv(credentialsDirectory, "")
			} else {
				t.Setenv(credentialsDirectory, tc.dir)
			}
			dir := outside
			if tc.inside {
				dir = credentials
			}
			path := filepath.Join(dir, "key")
			write(t, path, tc.perm)
			f, info, err := Open(path)
			if !errors.Is(err, tc.want) || (err == nil) != (f != nil) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if f != nil {
				_ = f.Close()
				if info.Mode().Perm() != tc.perm {
					t.Fatalf("mode %v", info.Mode())
				}
			}
		})
	}
}

func TestUnitOpenRefusesEscapesSymlinksAndNonFiles(t *testing.T) {
	parent := t.TempDir()
	credentials := filepath.Join(parent, "credentials")
	if err := os.Mkdir(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(credentialsDirectory, credentials)
	// ".." leaves the directory: the strict rule applies and 0440 is refused.
	escaped := filepath.Join(parent, "key")
	write(t, escaped, 0o440)
	if _, _, err := Open(credentials + "/../key"); !errors.Is(err, ErrPermissions) {
		t.Fatalf("escape through ..: %v", err)
	}
	// A symlink in the directory to a group-readable file elsewhere is refused.
	if err := os.Symlink(escaped, filepath.Join(credentials, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(filepath.Join(credentials, "link")); !errors.Is(err, ErrPermissions) {
		t.Fatalf("symlink: %v", err)
	}
	// A nested entry is not a credential.
	nested := filepath.Join(credentials, "sub")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nested, "key"), 0o440)
	if _, _, err := Open(filepath.Join(nested, "key")); !errors.Is(err, ErrPermissions) {
		t.Fatalf("nested: %v", err)
	}
	// A cleaned path inside the directory is a credential.
	write(t, filepath.Join(credentials, "key"), 0o440)
	f, _, err := Open(nested + "/../key")
	if err != nil {
		t.Fatalf("cleaned credential path: %v", err)
	}
	_ = f.Close()
	if _, _, err := Open(nested); !errors.Is(err, ErrPermissions) {
		t.Fatalf("directory: %v", err)
	}
	if _, _, err := Open(filepath.Join(credentials, "absent")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("absent credential: %v", err)
	}
	if _, _, err := Open(filepath.Join(parent, "absent")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("absent file: %v", err)
	}
}
