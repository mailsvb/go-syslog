package writer

import "time"

// TimestampLayout is the receive timestamp written in front of every message.
const TimestampLayout = "2006-01-02T15:04:05.000Z07:00"

// trimTrailing removes trailing LF, CR and NUL bytes that many senders append.
func trimTrailing(b []byte) []byte {
	for len(b) > 0 {
		switch b[len(b)-1] {
		case '\n', '\r', 0:
			b = b[:len(b)-1]
		default:
			return b
		}
	}
	return b
}

func isControl(c byte) bool {
	return (c < 0x20 && c != '\t') || c == 0x7f
}

// needsEscape reports whether b contains control characters (other than TAB)
// that would break the one-message-per-line file format.
func needsEscape(b []byte) bool {
	for _, c := range b {
		if isControl(c) {
			return true
		}
	}
	return false
}

// appendEscaped appends b to dst and replaces control characters with
// rsyslog-style octal escapes, e.g. LF becomes "#012".
func appendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		if isControl(c) {
			dst = append(dst, '#', '0'+(c>>6), '0'+((c>>3)&7), '0'+(c&7))
			continue
		}
		dst = append(dst, c)
	}
	return dst
}

// appendTimestamp appends t in UTC using TimestampLayout.
func appendTimestamp(dst []byte, t time.Time) []byte {
	return t.UTC().AppendFormat(dst, TimestampLayout)
}

// FormatLine returns the complete line written for a message. The writer uses
// an allocation-free version of the same logic; this one is for tests and tools.
func FormatLine(t time.Time, msg []byte) []byte {
	msg = trimTrailing(msg)
	out := appendTimestamp(make([]byte, 0, len(TimestampLayout)+len(msg)+2), t)
	out = append(out, ' ')
	out = appendEscaped(out, msg)
	return append(out, '\n')
}
