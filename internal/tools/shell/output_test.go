package shell

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHeadTailKeepsEverythingThatFits(t *testing.T) {
	b := newHeadTail(10)
	for _, s := range []string{"abc", "defg", "hij"} {
		fmt.Fprint(b, s)
	}
	if got := b.String(); got != "abcdefghij" {
		t.Errorf("String() = %q, want all input", got)
	}
}

// Random write sizes, including writes larger than the tail, must yield the
// exact head and tail of the full stream.
func TestHeadTailMatchesFullStream(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for iter := range 200 {
		limit := 1 + r.IntN(64)
		b := newHeadTail(limit)
		var full bytes.Buffer
		for range r.IntN(40) {
			p := make([]byte, r.IntN(3*limit))
			for i := range p {
				p[i] = 'a' + byte(r.IntN(26))
			}
			full.Write(p)
			if n, err := b.Write(p); n != len(p) || err != nil {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
		all := full.String()
		want := all
		if len(all) > limit {
			h, tl := limit/2, limit-limit/2
			want = fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s", all[:h], len(all)-limit, all[len(all)-tl:])
		}
		if got := b.String(); got != want {
			t.Fatalf("iter %d (limit %d, %d bytes): got %q, want %q", iter, limit, len(all), got, want)
		}
	}
}

func TestHeadTailDoesNotSplitRunes(t *testing.T) {
	b := newHeadTail(8)
	fmt.Fprint(b, strings.Repeat("한", 10)) // 3 bytes each
	got := b.String()
	if !utf8.ValidString(got) {
		t.Fatalf("String() is not valid UTF-8: %q", got)
	}
	// Head keeps 1 whole rune of 4 bytes, tail 1 of 4; 8 runes are omitted.
	want := "한\n\n[... 24 bytes omitted ...]\n\n한"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
