//go:build linux

package pty

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPair allocates a Unix98 pty pair on Linux: open /dev/ptmx, unlock the
// slave (TIOCSPTLCK), read its number (TIOCGPTN), then open /dev/pts/N.
func openPair() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	if err := ioctlInt(master.Fd(), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}

	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}

	return master, slave, nil
}

func ioctlInt(fd uintptr, req uint, val int) error {
	v := int32(val)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(&v)))
	if errno != 0 {
		return errno
	}
	return nil
}
