package store

import (
	"errors"
	"io/fs"
	"log"
	"os"
)

// chmodFile is os.Chmod, a variable so a test can make it fail.
var chmodFile = os.Chmod

// secureDatabase makes the database file private to its owner when it is new,
// and closes it to everyone outside the owner's group when it is not. The
// database holds the key that signs session cookies, so a read of it, or of a
// backup made from it, is a way to be any user.
//
// A new file is created here and set to 0600 explicitly, because creating it
// applies the umask (and a strict one would leave it unwritable), SQLite would
// give it the umask's mode, and the WAL and shared-memory files it writes beside
// it copy the database's. An existing file keeps what the
// operator gave its group (a backup user, say) and loses what it gave the
// world, with a log line: refusing to start over a mode would be a worse
// surprise than fixing it.
func secureDatabase(path string) error {
	if path == "" || path == ":memory:" {
		return nil
	}
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil // let SQLite report whatever is wrong with the path
		}
		defer f.Close()
		return f.Chmod(0o600)
	case err != nil:
		return nil
	}
	closeToWorld(path, st.Mode().Perm(), true)
	return nil
}

// secureSidecars does the same for the -wal and -shm files, which exist while
// the database is open and copy its mode when SQLite creates them.
func secureSidecars(path string) {
	for _, suffix := range []string{"-wal", "-shm"} {
		if st, err := os.Stat(path + suffix); err == nil {
			closeToWorld(path+suffix, st.Mode().Perm(), false)
		}
	}
}

// closeToWorld removes the world bits from path. A mode that cannot be changed
// (a file owned by someone else, a filesystem without modes) is reported and
// left: refusing to start over it would be a worse surprise than the mode.
func closeToWorld(path string, perm fs.FileMode, say bool) {
	if perm&0o007 == 0 {
		return
	}
	if err := chmodFile(path, perm&^0o007); err != nil {
		log.Printf("store: %s is readable by other users (%04o) and could not be changed: %v. "+
			"It holds the key that signs session cookies.", path, perm, err)
		return
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o007 != 0 {
		log.Printf("store: %s is still readable by other users after chmod (%04o); "+
			"this filesystem does not keep modes. It holds the key that signs session cookies.", path, st.Mode().Perm())
		return
	}
	if say {
		log.Printf("store: %s was readable by other users (%04o); now %04o. It holds the key that signs session cookies.",
			path, perm, perm&^0o007)
	}
}
