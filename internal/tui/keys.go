package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// keyMsgToBytes converts a Bubble Tea KeyMsg to the raw byte sequence that
// would be sent to a terminal. Used in raw mode to forward keystrokes to PTY.
//
// The conversion must be complete: every key event that Bubble Tea can emit
// needs a corresponding byte sequence so that interactive programs (vim, htop,
// claude, etc.) receive exactly what they would on a real terminal.
func keyMsgToBytes(msg tea.KeyMsg) []byte {
	var data []byte

	switch msg.Type {
	case tea.KeyEscape:
		data = []byte{0x1b}
	case tea.KeyEnter:
		data = []byte{0x0d}
	case tea.KeyTab:
		data = []byte{0x09}
	case tea.KeyBackspace:
		data = []byte{0x7f}
	case tea.KeySpace:
		data = []byte{' '}

	// Arrow keys
	case tea.KeyUp:
		data = []byte{0x1b, '[', 'A'}
	case tea.KeyDown:
		data = []byte{0x1b, '[', 'B'}
	case tea.KeyRight:
		data = []byte{0x1b, '[', 'C'}
	case tea.KeyLeft:
		data = []byte{0x1b, '[', 'D'}

	// Shift + arrow keys  (CSI 1;2 <letter>)
	case tea.KeyShiftUp:
		data = []byte{0x1b, '[', '1', ';', '2', 'A'}
	case tea.KeyShiftDown:
		data = []byte{0x1b, '[', '1', ';', '2', 'B'}
	case tea.KeyShiftRight:
		data = []byte{0x1b, '[', '1', ';', '2', 'C'}
	case tea.KeyShiftLeft:
		data = []byte{0x1b, '[', '1', ';', '2', 'D'}

	// Ctrl + arrow keys  (CSI 1;5 <letter>)
	case tea.KeyCtrlUp:
		data = []byte{0x1b, '[', '1', ';', '5', 'A'}
	case tea.KeyCtrlDown:
		data = []byte{0x1b, '[', '1', ';', '5', 'B'}
	case tea.KeyCtrlRight:
		data = []byte{0x1b, '[', '1', ';', '5', 'C'}
	case tea.KeyCtrlLeft:
		data = []byte{0x1b, '[', '1', ';', '5', 'D'}

	// Ctrl+Shift + arrow keys  (CSI 1;6 <letter>)
	case tea.KeyCtrlShiftUp:
		data = []byte{0x1b, '[', '1', ';', '6', 'A'}
	case tea.KeyCtrlShiftDown:
		data = []byte{0x1b, '[', '1', ';', '6', 'B'}
	case tea.KeyCtrlShiftRight:
		data = []byte{0x1b, '[', '1', ';', '6', 'C'}
	case tea.KeyCtrlShiftLeft:
		data = []byte{0x1b, '[', '1', ';', '6', 'D'}

	// Navigation keys
	case tea.KeyHome:
		data = []byte{0x1b, '[', 'H'}
	case tea.KeyEnd:
		data = []byte{0x1b, '[', 'F'}
	case tea.KeyPgUp:
		data = []byte{0x1b, '[', '5', '~'}
	case tea.KeyPgDown:
		data = []byte{0x1b, '[', '6', '~'}
	case tea.KeyInsert:
		data = []byte{0x1b, '[', '2', '~'}
	case tea.KeyDelete:
		data = []byte{0x1b, '[', '3', '~'}

	// Shift + navigation  (modifier 2)
	case tea.KeyShiftHome:
		data = []byte{0x1b, '[', '1', ';', '2', 'H'}
	case tea.KeyShiftEnd:
		data = []byte{0x1b, '[', '1', ';', '2', 'F'}

	// Ctrl + navigation  (modifier 5)
	case tea.KeyCtrlHome:
		data = []byte{0x1b, '[', '1', ';', '5', 'H'}
	case tea.KeyCtrlEnd:
		data = []byte{0x1b, '[', '1', ';', '5', 'F'}
	case tea.KeyCtrlPgUp:
		data = []byte{0x1b, '[', '5', ';', '5', '~'}
	case tea.KeyCtrlPgDown:
		data = []byte{0x1b, '[', '6', ';', '5', '~'}

	// Ctrl+Shift + navigation  (modifier 6)
	case tea.KeyCtrlShiftHome:
		data = []byte{0x1b, '[', '1', ';', '6', 'H'}
	case tea.KeyCtrlShiftEnd:
		data = []byte{0x1b, '[', '1', ';', '6', 'F'}

	// Shift+Tab (backtab)
	case tea.KeyShiftTab:
		data = []byte{0x1b, '[', 'Z'}

	// Function keys F1–F12
	case tea.KeyF1:
		data = []byte{0x1b, 'O', 'P'}
	case tea.KeyF2:
		data = []byte{0x1b, 'O', 'Q'}
	case tea.KeyF3:
		data = []byte{0x1b, 'O', 'R'}
	case tea.KeyF4:
		data = []byte{0x1b, 'O', 'S'}
	case tea.KeyF5:
		data = []byte{0x1b, '[', '1', '5', '~'}
	case tea.KeyF6:
		data = []byte{0x1b, '[', '1', '7', '~'}
	case tea.KeyF7:
		data = []byte{0x1b, '[', '1', '8', '~'}
	case tea.KeyF8:
		data = []byte{0x1b, '[', '1', '9', '~'}
	case tea.KeyF9:
		data = []byte{0x1b, '[', '2', '0', '~'}
	case tea.KeyF10:
		data = []byte{0x1b, '[', '2', '1', '~'}
	case tea.KeyF11:
		data = []byte{0x1b, '[', '2', '3', '~'}
	case tea.KeyF12:
		data = []byte{0x1b, '[', '2', '4', '~'}

	// Function keys F13–F20 (xterm extended)
	case tea.KeyF13:
		data = []byte{0x1b, '[', '1', ';', '2', 'P'}
	case tea.KeyF14:
		data = []byte{0x1b, '[', '1', ';', '2', 'Q'}
	case tea.KeyF15:
		data = []byte{0x1b, '[', '1', ';', '2', 'R'}
	case tea.KeyF16:
		data = []byte{0x1b, '[', '1', ';', '2', 'S'}
	case tea.KeyF17:
		data = []byte{0x1b, '[', '1', '5', ';', '2', '~'}
	case tea.KeyF18:
		data = []byte{0x1b, '[', '1', '7', ';', '2', '~'}
	case tea.KeyF19:
		data = []byte{0x1b, '[', '1', '8', ';', '2', '~'}
	case tea.KeyF20:
		data = []byte{0x1b, '[', '1', '9', ';', '2', '~'}

	// Ctrl key combinations (ASCII control codes)
	case tea.KeyCtrlA:
		data = []byte{0x01}
	case tea.KeyCtrlB:
		data = []byte{0x02}
	case tea.KeyCtrlC:
		data = []byte{0x03}
	case tea.KeyCtrlD:
		data = []byte{0x04}
	case tea.KeyCtrlE:
		data = []byte{0x05}
	case tea.KeyCtrlF:
		data = []byte{0x06}
	case tea.KeyCtrlG:
		data = []byte{0x07}
	case tea.KeyCtrlH:
		data = []byte{0x08}
	// KeyCtrlI = KeyTab (0x09), handled above.
	case tea.KeyCtrlJ:
		data = []byte{0x0a}
	case tea.KeyCtrlK:
		data = []byte{0x0b}
	case tea.KeyCtrlL:
		data = []byte{0x0c}
	// KeyCtrlM = KeyEnter (0x0d), handled above.
	case tea.KeyCtrlN:
		data = []byte{0x0e}
	// KeyCtrlO = 0x0f — reserved as escape hatch, never forwarded.
	case tea.KeyCtrlP:
		data = []byte{0x10}
	case tea.KeyCtrlQ:
		data = []byte{0x11}
	case tea.KeyCtrlR:
		data = []byte{0x12}
	case tea.KeyCtrlS:
		data = []byte{0x13}
	case tea.KeyCtrlT:
		data = []byte{0x14}
	case tea.KeyCtrlU:
		data = []byte{0x15}
	case tea.KeyCtrlV:
		data = []byte{0x16}
	case tea.KeyCtrlW:
		data = []byte{0x17}
	case tea.KeyCtrlX:
		data = []byte{0x18}
	case tea.KeyCtrlY:
		data = []byte{0x19}
	case tea.KeyCtrlZ:
		data = []byte{0x1a}

	// Additional Ctrl combos that Bubble Tea exposes.
	// Note: KeyCtrlOpenBracket (Ctrl+[) = KeyEscape, already handled above.
	// Note: KeyCtrlQuestionMark (Ctrl+?) = KeyBackspace, already handled above.
	case tea.KeyCtrlBackslash: // Ctrl+\  = FS
		data = []byte{0x1c}
	case tea.KeyCtrlCloseBracket: // Ctrl+] = GS
		data = []byte{0x1d}
	case tea.KeyCtrlCaret: // Ctrl+^ = RS
		data = []byte{0x1e}
	case tea.KeyCtrlUnderscore: // Ctrl+_ = US
		data = []byte{0x1f}

	default:
		// Regular character input — convert runes to UTF-8 bytes.
		if len(msg.Runes) > 0 {
			data = []byte(string(msg.Runes))
		}
	}

	if len(data) == 0 {
		return nil
	}

	// Alt modifier: terminals send Alt+key as ESC prefix followed by the
	// key bytes. Prepend 0x1b unless the sequence already starts with ESC
	// (e.g. arrow keys, function keys) to avoid double-ESC.
	if msg.Alt && (len(data) == 0 || data[0] != 0x1b) {
		data = append([]byte{0x1b}, data...)
	}

	// Bracketed paste: if Bubble Tea flagged this KeyMsg as pasted text,
	// wrap it in the standard bracketed-paste envelope so that programs
	// that enabled bracketedPasteMode receive the paste correctly.
	if msg.Paste {
		bp := make([]byte, 0, len(data)+12)
		bp = append(bp, "\x1b[200~"...)
		bp = append(bp, data...)
		bp = append(bp, "\x1b[201~"...)
		data = bp
	}

	return data
}

// mouseToPTYBytes encodes a Bubble Tea MouseMsg the way the child program asked
// for it. paneX and paneY are 1-based coordinates relative to the pane's content
// area; proto is the child's tracking mode (a vterm.Mouse* constant) and sgr
// reports whether it enabled SGR extended encoding (\x1b[?1006h).
//
// Returns nil when the event is not reportable under that protocol — the child
// asked for fewer events than the terminal sends (X10 reports presses only,
// \x1b[?1000h has no motion reports), or the coordinates do not fit the legacy
// encoding.
//
// Encoding matters as much as the mode. A child that enabled \x1b[?1000h alone
// expects the legacy X10 form and cannot parse an SGR report; handing it the
// wrong one is how mouse events end up echoed as literal text in an input line.
//
//	SGR (\x1b[?1006h): \x1b[< Cb ; Cx ; Cy {M|m}   M = press/motion, m = release
//	legacy (X10):      \x1b[M  Cb+32 Cx+32 Cy+32   release reported as button 3
//
// Cb encodes the button plus modifier bits, with bit 32 set for motion.
func mouseToPTYBytes(msg tea.MouseMsg, paneX, paneY int, proto string, sgr bool) []byte {
	if proto == vterm.MouseOff {
		return nil
	}

	release := msg.Action == tea.MouseActionRelease
	motion := msg.Action == tea.MouseActionMotion

	var btn int
	wheel := false
	extended := false // buttons 8/9, which the legacy encoding cannot express
	switch msg.Button {
	case tea.MouseButtonLeft:
		btn = 0
	case tea.MouseButtonMiddle:
		btn = 1
	case tea.MouseButtonRight:
		btn = 2
	case tea.MouseButtonWheelUp:
		btn, wheel = 64, true
	case tea.MouseButtonWheelDown:
		btn, wheel = 65, true
	case tea.MouseButtonWheelLeft:
		btn, wheel = 66, true
	case tea.MouseButtonWheelRight:
		btn, wheel = 67, true
	case tea.MouseButtonBackward:
		btn, extended = 128, true
	case tea.MouseButtonForward:
		btn, extended = 129, true
	case tea.MouseButtonNone:
		btn = 3 // used for release in some protocols
	default:
		return nil
	}

	// Report only what this tracking mode covers. Sending more than the child
	// subscribed to is the same class of bug as sending the wrong encoding: it
	// asked a specific question and gets answers it has no parser for.
	switch proto {
	case vterm.MouseX10:
		// \x1b[?9h reports button presses of buttons 1-3 and nothing else.
		if release || motion || wheel || extended {
			return nil
		}
	case vterm.MouseNormal:
		// \x1b[?1000h reports presses and releases, never motion.
		if motion {
			return nil
		}
	}

	// Modifier bits. X10 mode has no room for them.
	if proto != vterm.MouseX10 {
		if msg.Shift {
			btn |= 4
		}
		if msg.Alt {
			btn |= 8
		}
		if msg.Ctrl {
			btn |= 16
		}
	}
	if motion {
		btn |= 32
	}

	if sgr {
		suffix := 'M'
		if release {
			suffix = 'm'
		}
		return []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", btn, paneX, paneY, suffix))
	}

	// Legacy X10 encoding. Every field is a single byte biased by 32, so a
	// release cannot name its button (button 3 means "some button came up") and
	// nothing past column/row 223 or button code 223 is expressible. Drop what
	// does not fit rather than emit a report that decodes to the wrong cell.
	if extended {
		return nil
	}
	if release {
		btn = 3 | (btn &^ 0x03) // keep the modifier/motion bits, replace the button
	}
	if btn > 223 || paneX > 223 || paneY > 223 {
		return nil
	}
	return []byte{0x1b, '[', 'M', byte(32 + btn), byte(32 + paneX), byte(32 + paneY)}
}
