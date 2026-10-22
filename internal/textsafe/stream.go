package textsafe

// maxCarry caps how much of an unfinished escape sequence a Stream holds
// back, so a sequence that never ends can't swallow the text after it.
const maxCarry = 256

// Stream cleans text that arrives in deltas. Clean on each delta alone would
// leave a fragment such as "[31m" when an escape sequence is split across two
// deltas; a Stream holds back an incomplete escape sequence at the end of a
// delta and finishes it with the next one. It is not safe for concurrent use.
type Stream struct{ carry []rune }

// Write returns the cleaned text of the delta, keeping any unfinished escape
// sequence at its end for the next call.
func (s *Stream) Write(delta string) string {
	rs := append(s.carry, []rune(delta)...)
	s.carry = nil
	if cut := incompleteTail(rs); cut >= 0 && len(rs)-cut <= maxCarry {
		s.carry = append([]rune(nil), rs[cut:]...)
		rs = rs[:cut]
	}
	return Clean(string(rs))
}

// Flush returns the cleaned text of whatever is still held back and resets
// the Stream. Call it when the text ends (a tool call, the end of the turn).
func (s *Stream) Flush() string {
	rs := s.carry
	s.carry = nil
	return Clean(string(rs))
}

// incompleteTail returns the index where an escape sequence that is still
// open at the end of rs starts, or -1.
func incompleteTail(rs []rune) int {
	n := len(rs)
	for i := 0; i < n; i++ {
		switch r := rs[i]; {
		case r == 0x1b:
			if i+1 >= n {
				return i
			}
			switch c := rs[i+1]; {
			case c == '[':
				if csiOpen(rs, i+2) {
					return i
				}
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				if stringOpen(rs, i+2) {
					return i
				}
			default:
				j := i + 1
				for j < n && rs[j] >= 0x20 && rs[j] <= 0x2f {
					j++
				}
				if j >= n {
					return i
				}
			}
			i = skipEscape(rs, i)
		case r == 0x9b:
			if csiOpen(rs, i+1) {
				return i
			}
			i = skipCSI(rs, i+1)
		case r == 0x90 || r == 0x98 || r == 0x9d || r == 0x9e || r == 0x9f:
			if stringOpen(rs, i+1) {
				return i
			}
			if end, ok := skipString(rs, i+1); ok {
				i = end
			}
		}
	}
	return -1
}

// csiOpen reports whether a CSI whose parameters start at rs[j] has no final
// byte yet.
func csiOpen(rs []rune, j int) bool {
	for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x3f {
		j++
	}
	return j >= len(rs)
}

// stringOpen reports whether a string sequence (OSC, DCS, ...) starting at
// rs[j] has no terminator and no line end yet. A trailing ESC may be the
// start of the ST terminator, so it counts as open too.
func stringOpen(rs []rune, j int) bool {
	for ; j < len(rs); j++ {
		switch rs[j] {
		case 0x07, 0x9c, '\n':
			return false
		case 0x1b:
			return j+1 >= len(rs)
		}
	}
	return true
}
