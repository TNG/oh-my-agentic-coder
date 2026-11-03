//go:build !darwin && !linux

package sandboxrun

import "os"

// fingerprintOf degrades to mtime only on platforms without the raw
// stat fields; the sandbox backends do not run there anyway.
func fingerprintOf(info os.FileInfo) dirFingerprint {
	mt := info.ModTime()
	return dirFingerprint{
		mtimeSec: mt.Unix(), mtimeNsec: int64(mt.Nanosecond()),
		ctimeSec: mt.Unix(), ctimeNsec: int64(mt.Nanosecond()),
	}
}
