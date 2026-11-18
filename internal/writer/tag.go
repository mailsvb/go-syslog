package writer

// Per-tag files for senders such as a Docker host, where many sources share
// one IP and are only told apart by the syslog TAG (RFC 3164) or APP-NAME
// (RFC 5424). The functions here run on the reader threads for every message
// of a split sender, so they do not allocate and look at most at the first
// tagScanLimit bytes.

const (
	// tagScanLimit bounds how far into a message parseTag looks.
	tagScanLimit = 512
	// MaxTagLen is the longest tag used as a directory name; longer tags are cut.
	MaxTagLen = 64
	// Untagged is the directory for messages of a split sender without a usable tag.
	Untagged = "_untagged"
)

// parseTag returns the position [start, end) of the syslog tag in b, or
// (0, 0) if there is none. It understands RFC 5424 (APP-NAME) and RFC 3164
// with or without HOSTNAME, which covers Docker's "unix" (default),
// "rfc3164", "rfc5424" and "rfc5424micro" syslog formats.
func parseTag(b []byte) (start, end int) {
	if len(b) > tagScanLimit {
		b = b[:tagScanLimit]
	}
	// PRI: "<" 1-3 digits ">"
	if len(b) < 3 || b[0] != '<' {
		return 0, 0
	}
	i := 1
	for i < len(b) && i <= 3 && isDigit(b[i]) {
		i++
	}
	if i == 1 || i >= len(b) || b[i] != '>' {
		return 0, 0
	}
	i++
	// RFC 5424: VERSION (1-3 digits, no leading zero) SP
	if i < len(b) && isDigit(b[i]) && b[i] != '0' {
		j := i + 1
		for j < len(b) && j-i < 3 && isDigit(b[j]) {
			j++
		}
		if j < len(b) && b[j] == ' ' {
			return rfc5424Tag(b, j+1)
		}
	}
	return rfc3164Tag(b, i)
}

// rfc5424Tag returns APP-NAME, the third field after the version:
// TIMESTAMP SP HOSTNAME SP APP-NAME SP ...
func rfc5424Tag(b []byte, i int) (int, int) {
	for f := 0; f < 2; f++ { // skip TIMESTAMP and HOSTNAME
		for i < len(b) && b[i] != ' ' {
			i++
		}
		i++ // the SP
		if i >= len(b) {
			return 0, 0
		}
	}
	start := i
	for i < len(b) && b[i] != ' ' {
		i++
	}
	if i >= len(b) || i == start || (i-start == 1 && b[start] == '-') {
		return 0, 0 // unterminated, empty or NILVALUE
	}
	return start, i
}

// rfc3164Tag handles "TIMESTAMP [HOSTNAME] TAG[PID]: MSG". The timestamp is
// optional; without one only a token that looks like a tag is accepted.
func rfc3164Tag(b []byte, i int) (int, int) {
	if !isStamp(b[i:]) {
		start, end, tagLike := tagToken(b, i)
		if !tagLike {
			return 0, 0
		}
		return start, end
	}
	i += len("Jan _2 15:04:05 ")
	start, end, tagLike := tagToken(b, i)
	if tagLike || end == 0 {
		return start, end
	}
	// The first token was HOSTNAME; the tag follows it.
	start, end, _ = tagToken(b, end+1)
	return start, end
}

// tagToken scans the token starting at i. It ends at '[', at ':' that is
// followed by SP or ends the input, or at SP. tagLike is true if it ended
// with '[' or ':' (TAG[PID]: or TAG:), which a HOSTNAME never does. An empty
// or unterminated token returns end == 0.
func tagToken(b []byte, i int) (start, end int, tagLike bool) {
	start = i
	for ; i < len(b); i++ {
		switch b[i] {
		case ' ':
			if i == start {
				return 0, 0, false
			}
			return start, i, false
		case '[':
			if i == start {
				return 0, 0, false
			}
			return start, i, true
		case ':':
			if i+1 == len(b) || b[i+1] == ' ' {
				if i == start {
					return 0, 0, false
				}
				return start, i, true
			}
			// "::" in an IPv6 HOSTNAME, keep scanning
		}
	}
	return 0, 0, false
}

// isStamp reports whether b starts with an RFC 3164 timestamp
// "Mmm dd hh:mm:ss " (the day is space padded).
func isStamp(b []byte) bool {
	return len(b) > 16 &&
		isUpper(b[0]) && isLower(b[1]) && isLower(b[2]) && b[3] == ' ' &&
		(b[4] == ' ' || isDigit(b[4])) && isDigit(b[5]) && b[6] == ' ' &&
		isDigit(b[7]) && isDigit(b[8]) && b[9] == ':' &&
		isDigit(b[10]) && isDigit(b[11]) && b[12] == ':' &&
		isDigit(b[13]) && isDigit(b[14]) && b[15] == ' '
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }

// tagByte maps a tag byte to the byte used in the directory name: letters,
// digits, '.', '_' and '-' are kept, everything else becomes '_'. This keeps
// path separators and control characters from the network out of paths.
func tagByte(c byte) byte {
	if isDigit(c) || isUpper(c) || isLower(c) || c == '.' || c == '_' || c == '-' {
		return c
	}
	return '_'
}

// isUntaggedName reports whether the (already cut) raw tag cannot be used as
// a directory name. These names contain only allowed bytes, so sanitizing
// cannot produce them from anything else.
func isUntaggedName(raw []byte) bool {
	switch string(raw) {
	case "", ".", "..", "-":
		return true
	}
	return false
}

// sanitizeTag turns a raw tag into a safe directory name.
func sanitizeTag(raw []byte) string {
	raw = raw[:min(len(raw), MaxTagLen)]
	if isUntaggedName(raw) {
		return Untagged
	}
	out := make([]byte, len(raw))
	for i, c := range raw {
		out[i] = tagByte(c)
	}
	return string(out)
}

// hashTag continues the FNV-1a hash h over the sanitized form of raw, so
// that all raw tags which end up in the same file go to the same shard.
func hashTag(h uint32, raw []byte) uint32 {
	raw = raw[:min(len(raw), MaxTagLen)]
	if isUntaggedName(raw) {
		for i := 0; i < len(Untagged); i++ {
			h = (h ^ uint32(Untagged[i])) * fnvPrime
		}
		return h
	}
	for _, c := range raw {
		h = (h ^ uint32(tagByte(c))) * fnvPrime
	}
	return h
}
