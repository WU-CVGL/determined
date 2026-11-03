package internal

import "syscall"

// codeServerStatID returns the device, inode and status change time in nanoseconds of the file
// whose Stat_t is st.
func codeServerStatID(st *syscall.Stat_t) (dev, inode uint64, ctime int64) {
	return uint64(st.Dev), st.Ino, st.Ctimespec.Nano()
}
