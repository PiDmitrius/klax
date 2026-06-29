//go:build darwin

package pty

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPair allocates a pty pair on macOS/Darwin. There is no TIOCGPTN-style
// "slave number" ioctl; instead the slave is enabled with grantpt
// (TIOCPTYGRANT) + unlockpt (TIOCPTYUNLK) and its path is fetched with
// TIOCPTYGNAME.
func openPair() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// Canonical macOS order: grantpt, unlockpt, then resolve the slave name.
	if err := unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYGRANT, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYGRANT: %w", err)
	}
	if err := unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYUNLK, 0); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYUNLK: %w", err)
	}
	sname, err := ptsname(master.Fd())
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCPTYGNAME: %w", err)
	}

	slave, err = os.OpenFile(sname, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}

	return master, slave, nil
}

// ptsname returns the slave device path for the given master fd via the
// TIOCPTYGNAME ioctl, which writes a NUL-terminated path into a 128-byte buf.
func ptsname(fd uintptr) (string, error) {
	buf := make([]byte, 128)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	for i, c := range buf {
		if c == 0 {
			return string(buf[:i]), nil
		}
	}
	return "", errors.New("TIOCPTYGNAME result not NUL-terminated")
}
