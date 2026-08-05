package sessfiles

import (
	"os"
	"syscall"
)

// fileIdentity returns (device, inode, ctime) from a stat result; ok=false when unavailable.
// ctime is what makes the identity check as strong as git's index: an unprivileged writer can
// restore mtime after an in-place rewrite, but not ctime.
func fileIdentity(fi os.FileInfo) (dev, ino uint64, ctimeNS int64, ok bool) {
	st, okCast := fi.Sys().(*syscall.Stat_t)
	if !okCast {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), st.Ino, st.Ctim.Nano(), true
}
