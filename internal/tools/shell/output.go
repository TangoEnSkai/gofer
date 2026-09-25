package shell

import (
	"fmt"
	"unicode/utf8"
)

// headTail is an io.Writer that keeps the first and the last bytes written to
// it, so memory stays bounded however much a command prints.
type headTail struct {
	head    []byte
	headMax int
	tail    []byte // ring buffer once it holds tailMax bytes
	tailMax int
	pos     int // oldest byte in tail
	total   int64
}

// newHeadTail keeps up to limit bytes, split evenly between head and tail.
func newHeadTail(limit int) *headTail {
	return &headTail{headMax: limit / 2, tailMax: limit - limit/2}
}

func (b *headTail) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if room := b.headMax - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if b.tailMax == 0 || len(p) == 0 {
		return n, nil
	}
	if len(p) >= b.tailMax {
		b.tail = append(b.tail[:0], p[len(p)-b.tailMax:]...)
		b.pos = 0
		return n, nil
	}
	if room := b.tailMax - len(b.tail); room > 0 {
		k := min(room, len(p))
		b.tail = append(b.tail, p[:k]...)
		p = p[k:]
	}
	for len(p) > 0 {
		k := copy(b.tail[b.pos:], p)
		b.pos = (b.pos + k) % b.tailMax
		p = p[k:]
	}
	return n, nil
}

// String returns everything written if it fit, or the head and tail around a
// marker giving the number of bytes omitted. Cuts never split a UTF-8 rune.
func (b *headTail) String() string {
	tail := make([]byte, 0, len(b.tail))
	tail = append(tail, b.tail[b.pos:]...)
	tail = append(tail, b.tail[:b.pos]...)
	if int64(len(b.head)+len(tail)) == b.total {
		return string(b.head) + string(tail)
	}
	head := trimIncompleteEnd(b.head)
	tail = trimIncompleteStart(tail)
	omitted := b.total - int64(len(head)+len(tail))
	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s", head, omitted, tail)
}

// trimIncompleteEnd drops a rune cut off at the end of p.
func trimIncompleteEnd(p []byte) []byte {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return p[:i]
			}
			break
		}
	}
	return p
}

// trimIncompleteStart drops the continuation bytes of a rune cut off at the
// start of p.
func trimIncompleteStart(p []byte) []byte {
	for i := 0; i < len(p) && i < utf8.UTFMax; i++ {
		if utf8.RuneStart(p[i]) {
			return p[i:]
		}
	}
	return p
}
