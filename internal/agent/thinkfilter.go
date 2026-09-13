package agent

import "strings"

// thinkFilter is the streaming counterpart of stripThinkBlocks: it removes
// <think>...</think> reasoning blocks incrementally, as content deltas
// arrive, so live subscribers (CLI, web UI) never see reasoning text. Tag
// fragments split across deltas are held back until they resolve.
type thinkFilter struct {
	inside  bool   // currently within a <think> block
	pending string // held-back bytes that may be a partial tag
}

const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// feed consumes one content chunk and returns the text that is safe to
// emit now (reasoning removed, partial tags withheld).
func (f *thinkFilter) feed(s string) string {
	buf := f.pending + s
	f.pending = ""
	var out strings.Builder
	for len(buf) > 0 {
		if f.inside {
			i := strings.Index(buf, thinkClose)
			if i < 0 {
				f.pending = holdBack(buf, thinkClose)
				return out.String()
			}
			buf = buf[i+len(thinkClose):]
			f.inside = false
			continue
		}
		i := strings.Index(buf, thinkOpen)
		if i < 0 {
			keep := holdBack(buf, thinkOpen)
			out.WriteString(buf[:len(buf)-len(keep)])
			f.pending = keep
			return out.String()
		}
		out.WriteString(buf[:i])
		buf = buf[i+len(thinkOpen):]
		f.inside = true
	}
	return out.String()
}

// holdBack returns the longest suffix of s that is a proper prefix of tag
// (and thus might complete into a tag in the next chunk).
func holdBack(s, tag string) string {
	max := len(tag) - 1
	if max > len(s) {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return s[len(s)-n:]
		}
	}
	return ""
}
