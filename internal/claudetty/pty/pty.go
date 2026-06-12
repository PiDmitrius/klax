// Package pty opens a Unix98 pseudo-terminal pair via golang.org/x/sys —
// the only external dependency in this module. Linux-only by design (klax
// and claudetty run on the same Linux hosts).
package pty

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Pair is a master/slave pseudo-terminal pair. The child process gets the
// slave as its controlling terminal; the parent reads and writes the master.
type Pair struct {
	Master *os.File
	Slave  *os.File
}

// Open allocates a pty pair and sets the slave's window size.
func Open(rows, cols uint16) (*Pair, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	// Unlock the slave and find its number.
	if err := ioctlInt(master.Fd(), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("TIOCGPTN: %w", err)
	}

	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open slave: %w", err)
	}

	ws := unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &ws); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("TIOCSWINSZ: %w", err)
	}

	return &Pair{Master: master, Slave: slave}, nil
}

// Close closes both ends. Safe to call more than once.
func (p *Pair) Close() {
	if p.Master != nil {
		p.Master.Close()
		p.Master = nil
	}
	p.CloseSlave()
}

// CloseSlave closes only the slave end — the parent must do this after the
// child has started so that reads on the master return EIO when the child
// exits (otherwise our own open slave fd keeps the pty alive forever).
func (p *Pair) CloseSlave() {
	if p.Slave != nil {
		p.Slave.Close()
		p.Slave = nil
	}
}

func ioctlInt(fd uintptr, req uint, val int) error {
	v := int32(val)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, uintptr(req), uintptr(unsafe.Pointer(&v)))
	if errno != 0 {
		return errno
	}
	return nil
}
