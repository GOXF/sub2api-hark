package gateway

import (
	"path"
	"regexp"
	"strings"
)

// localFilePath matches the absolute paths clients put next to an attached
// file. Hark receives the uploaded bytes separately and cannot read the
// client's filesystem, so only the basename is useful in the message.
var localFilePath = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|/(?:users|home|tmp|var|opt|mnt|root|private)/)[^\s"'<>]+`)

func stripLocalPaths(text string) string {
	matches := localFilePath.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		prefix := text[:start]
		if i := strings.LastIndexAny(prefix, " \t\r\n\"'<>"); i >= 0 {
			prefix = prefix[i+1:]
		}
		if strings.Contains(prefix, "://") {
			continue // leave URLs intact
		}
		if prefix != "" {
			if c := prefix[len(prefix)-1]; c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				continue // "https://" drive-letter lookalike, not a local path
			}
		}
		out.WriteString(text[last:start])
		raw := text[start:end]
		trimmed := strings.TrimRight(raw, ".,;:!?，。；：！？)]}")
		base := path.Base(strings.ReplaceAll(trimmed, `\`, "/"))
		if base == "." || base == "/" || base == "" {
			out.WriteString("[附件]")
		} else {
			out.WriteString(base)
		}
		out.WriteString(raw[len(trimmed):])
		last = end
	}
	out.WriteString(text[last:])
	return out.String()
}
