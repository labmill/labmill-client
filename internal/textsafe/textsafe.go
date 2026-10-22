// Package textsafe makes untrusted text (from Claude or the cluster) safe to
// show: it removes everything that could change how text is displayed
// rather than what it says.
package textsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean returns s with: C0 control characters removed except '\n' (a '\t'
// becomes four spaces, a "\r\n" becomes "\n", a lone '\r' is removed); DEL
// and C1 controls (U+0080-U+009F) removed, the C1 introducers (U+009B CSI;
// U+009D OSC, U+0090 DCS, U+0098, U+009E, U+009F) together with their whole
// sequence like their ESC forms; ANSI/VT escape sequences introduced by ESC
// removed whole (CSI "[...final", OSC "]...BEL or ESC\", or a single
// following character); every Unicode format
// character (category Cf: bidi controls, U+200B, U+2060-U+2064, U+FEFF,
// soft hyphen U+00AD, U+206A-U+206F, U+FFF9-U+FFFB, tag characters
// U+E0000-U+E007F, ...) and the line/paragraph separators U+2028/U+2029
// removed, EXCEPT U+200C and U+200D (ZWNJ/ZWJ), which are kept because emoji
// sequences and Persian/Indic text need them; invalid UTF-8 replaced with
// U+FFFD. Ordinary text, including non-Latin scripts and emoji, is unchanged.
//
// Callers that stream text should clean the accumulated text, or use a
// Stream (stream.go), rather than Clean each delta alone, so a sequence split
// across deltas is still removed whole.
//
// The output is safe for a terminal or for plain-text display only. It is NOT
// HTML-safe: '<', '&' and quotes are untouched, so a web page must insert it
// with textContent, never innerHTML.
func Clean(s string) string {
	if s == "" {
		return s
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r == 0x1b:
			i = skipEscape(rs, i)
		case r == 0x9b: // C1 CSI: removed whole if terminated, else only the introducer
			if end := skipCSI(rs, i+1); end > i && rs[end] >= 0x40 && rs[end] <= 0x7e {
				i = end
			}
		case r == 0x90 || r == 0x98 || r == 0x9d || r == 0x9e || r == 0x9f: // C1 DCS, SOS, OSC, PM, APC
			if end, ok := skipString(rs, i+1); ok {
				i = end
			}
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// control characters (including a lone '\r') are dropped
		case r == 0x200c || r == 0x200d:
			b.WriteRune(r) // ZWNJ/ZWJ: needed by emoji sequences and Persian/Indic text
		case r == 0x2028 || r == 0x2029 || unicode.Is(unicode.Cf, r):
			// format characters (bidi, zero-width, tags, ...) and separators: dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipEscape returns the index of the last rune of the escape sequence that
// starts with the ESC at rs[i].
func skipEscape(rs []rune, i int) int {
	if i+1 >= len(rs) {
		return i
	}
	switch c := rs[i+1]; {
	case c == '[':
		return skipCSI(rs, i+2)
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		end, _ := skipString(rs, i+2)
		return end
	default:
		// nF sequence (ESC, intermediates 0x20-0x2f, final byte 0x30-0x7e)
		// or a two-character sequence. Anything else (a newline, another
		// ESC, non-ASCII text) is not a final byte: only the ESC and the
		// intermediates are dropped and the following text is kept.
		j := i + 1
		for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2f {
			j++
		}
		if j < len(rs) && rs[j] >= 0x30 && rs[j] <= 0x7e {
			return j
		}
		return j - 1
	}
}

// skipCSI consumes parameter/intermediate bytes and the final byte, starting
// at rs[j]; it returns the index of the last consumed rune.
func skipCSI(rs []rune, j int) int {
	for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x3f {
		j++
	}
	if j < len(rs) && rs[j] >= 0x40 && rs[j] <= 0x7e {
		return j
	}
	return j - 1 // malformed: stop before the offending rune
}

// skipString consumes a string sequence (OSC, DCS, ...) starting at rs[j]
// until BEL, ST (ESC \ or U+009C) or the end of the line, whichever comes
// first. An unterminated sequence therefore cannot swallow later lines. It
// returns the index of the last rune to skip and whether a terminator was
// found.
func skipString(rs []rune, j int) (int, bool) {
	for ; j < len(rs); j++ {
		switch {
		case rs[j] == 0x07 || rs[j] == 0x9c:
			return j, true
		case rs[j] == 0x1b:
			if j+1 < len(rs) && rs[j+1] == '\\' {
				return j + 1, true
			}
			return j - 1, false // a new escape sequence begins; let the caller see it
		case rs[j] == '\n':
			return j - 1, false
		}
	}
	return len(rs) - 1, false
}
