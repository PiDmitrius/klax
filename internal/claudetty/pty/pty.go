// Package pty opens a Unix98 pseudo-terminal pair via golang.org/x/sys —
// the only external dependency in this module. The platform-specific
// allocation lives in pty_linux.go / pty_darwin.go; everything else here is
// shared across Unix targets.
package pty

import (
	"fmt"
	"os"

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
	master, slave, err := openPair()
	if err != nil {
		return nil, err
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
