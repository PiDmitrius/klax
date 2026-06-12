// Package term answers the handful of DEC / XTerm queries Ink (the
// React-for-terminals runtime Claude Code uses) emits at startup. Without
// responses the UI hangs forever waiting for a terminal it thinks is broken.
//
// Recognised:
//   - DA1: `ESC [ c` or `ESC [ 0 c`     -> "VT100 with AVO"
//   - DA2: `ESC [ > c` or `ESC [ > 0 c` -> terminal id 0/0/0
//   - DSR cursor position: `ESC [ 6 n`  -> row 1 col 1
//   - XTVERSION: `ESC [ > q`            -> DCS "claudetty"
//   - Window-size report: `ESC [ 18 t`  -> "8 ; rows ; cols t"
package term

import "fmt"

// RespondToDecQueries scans incoming PTY bytes for terminal queries and
// returns the response bytes to write back to the PTY. rows/cols feed the
// window-size report and must match the PTY's actual size. Stateless: callers
// pass each output chunk; sequences split across chunk boundaries are at
// worst ignored (Ink re-queries on its own timer).
func RespondToDecQueries(bytes []byte, rows, cols uint16) []byte {
	var out []byte
	for i := 0; i < len(bytes); i++ {
		if bytes[i] != 0x1b { // ESC
			continue
		}
		if i+1 >= len(bytes) || bytes[i+1] != '[' {
			continue
		}
		j := i + 2
		privateGT := j < len(bytes) && bytes[j] == '>'
		if privateGT {
			j++
		}
		paramStart := j
		for j < len(bytes) && bytes[j] >= 0x30 && bytes[j] <= 0x3f {
			j++
		}
		for j < len(bytes) && bytes[j] >= 0x20 && bytes[j] <= 0x2f {
			j++
		}
		if j >= len(bytes) {
			break
		}
		final := bytes[j]
		params := string(bytes[paramStart:j])

		switch final {
		case 'c':
			if privateGT {
				out = append(out, "\x1b[>0;0;0c"...)
			} else {
				out = append(out, "\x1b[?1;2c"...)
			}
		case 'n':
			if params == "6" {
				out = append(out, "\x1b[1;1R"...)
			}
		case 'q':
			if privateGT {
				out = append(out, "\x1bP>|claudetty\x1b\\"...)
			}
		case 't':
			if params == "18" {
				out = append(out, fmt.Sprintf("\x1b[8;%d;%dt", rows, cols)...)
			}
		}
		i = j
	}
	return out
}

// StripEscapes removes CSI / OSC / DCS escape sequences, leaving only the
// literal payload. Used to make plain-text substring matching (e.g.
// trust-dialog detection) robust against cursor-positioning escapes that
// pad words with `ESC [ 1 C` instead of spaces.
func StripEscapes(bytes []byte) []byte {
	var out []byte
	for i := 0; i < len(bytes); {
		b := bytes[i]
		if b != 0x1b {
			out = append(out, b)
			i++
			continue
		}
		if i+1 >= len(bytes) {
			break
		}
		switch bytes[i+1] {
		case '[':
			i += 2
			for i < len(bytes) && bytes[i] >= 0x30 && bytes[i] <= 0x3f {
				i++
			}
			for i < len(bytes) && bytes[i] >= 0x20 && bytes[i] <= 0x2f {
				i++
			}
			if i < len(bytes) {
				i++ // final byte
			}
		case ']':
			i += 2
			for i < len(bytes) {
				if bytes[i] == 0x07 {
					i++
					break
				}
				if bytes[i] == 0x1b && i+1 < len(bytes) && bytes[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		case 'P', 'X', '^', '_':
			i += 2
			for i < len(bytes) {
				if bytes[i] == 0x1b && i+1 < len(bytes) && bytes[i+1] == '\\' {
					i += 2
					break
				}
				i++
			}
		default:
			i += 2
		}
	}
	return out
}
