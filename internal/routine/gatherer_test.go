package routine

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type namedGatherer struct {
	fakeGatherer
	name string
}

func (g *namedGatherer) Name() string { return g.name }

func TestRegistry(t *testing.T) {
	var reg Registry // zero value is usable
	if _, ok := reg.Get("x"); ok {
		t.Fatal("Get on empty registry succeeded")
	}
	for _, name := range []string{"github.my_open_prs", "test.fake", "local"} {
		if err := reg.Register(&namedGatherer{name: name}); err != nil {
			t.Fatalf("Register(%q): %v", name, err)
		}
	}
	if g, ok := reg.Get("test.fake"); !ok || g.Name() != "test.fake" {
		t.Errorf("Get(test.fake) = %v, %v", g, ok)
	}
	if got, want := reg.Names(), []string{"github.my_open_prs", "local", "test.fake"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}

	for _, tt := range []struct {
		g       Gatherer
		wantErr string
	}{
		{nil, "nil gatherer"},
		{&namedGatherer{name: "local"}, "already registered"},
		{&namedGatherer{name: ""}, "invalid gatherer name"},
		{&namedGatherer{name: "GitHub.PRs"}, "invalid gatherer name"},
		{&namedGatherer{name: "gh prs"}, "invalid gatherer name"},
		{&namedGatherer{name: "gh."}, "invalid gatherer name"},
	} {
		if err := reg.Register(tt.g); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("Register(%v) error = %v, want %q", tt.g, err, tt.wantErr)
		}
	}
}

func TestDepsNow(t *testing.T) {
	fixed := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	if got := (Deps{Clock: func() time.Time { return fixed }}).Now(); !got.Equal(fixed) {
		t.Errorf("Now = %v, want %v", got, fixed)
	}
	if got := (Deps{}).Now(); time.Since(got) > time.Minute {
		t.Errorf("zero Deps.Now = %v, want about now", got)
	}
}
