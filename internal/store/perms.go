package store

import (
	"errors"
	"io/fs"
	"log"
	"os"
)

// secureDatabase makes the database file private to its owner when it is new,
// and closes it to everyone outside the owner's group when it is not. The
// database holds the key that signs session cookies, so a read of it, or of a
// backup made from it, is a way to be any user.
//
// A new file is created here, 0600 whatever the umask, because SQLite would
// create it with the umask's mode and the WAL and shared-memory files it
// writes beside it copy the database's. An existing file keeps what the
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
		return f.Close()
	case err != nil:
		return nil
	}
	return closeToWorld(path, st.Mode().Perm(), true)
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

func closeToWorld(path string, perm fs.FileMode, say bool) error {
	if perm&0o007 == 0 {
		return nil
	}
	if err := os.Chmod(path, perm&^0o007); err != nil {
		return err
	}
	if say {
		log.Printf("store: %s was readable by other users (%04o); now %04o. It holds the key that signs session cookies.",
			path, perm, perm&^0o007)
	}
	return nil
}
