//go:build darwin

package sandboxrun

import (
	"os"
	"syscall"
)

// fingerprintOf extracts a directory's mtime, ctime and identity from
// the raw stat; see dirFingerprint for what each field defends against
// and os.FileInfo does not expose ctime, inode or device.
func fingerprintOf(info os.FileInfo) dirFingerprint {
	mt := info.ModTime()
	fp := dirFingerprint{mtimeSec: mt.Unix(), mtimeNsec: int64(mt.Nanosecond())}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		fp.ctimeSec, fp.ctimeNsec = int64(st.Ctimespec.Sec), int64(st.Ctimespec.Nsec)
		fp.ino, fp.dev = int64(st.Ino), int64(st.Dev)
	} else {
		// Unreachable on darwin; degrade to mtime-only, which only
		// costs extra re-reads.
		fp.ctimeSec, fp.ctimeNsec = fp.mtimeSec, fp.mtimeNsec
	}
	return fp
}
