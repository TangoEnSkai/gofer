package main

import "testing"

func TestVersion(t *testing.T) {
	old := version
	version = "v1.2.3" // as set by -ldflags "-X main.version=v1.2.3"
	t.Cleanup(func() { version = old })

	for _, args := range [][]string{{"version"}, {"--version"}} {
		stdout, stderr, code := execute(t, testApp(t), args...)
		if code != 0 || stdout != "gofer v1.2.3\n" || stderr != "" {
			t.Errorf("gofer %v: code %d, stdout %q, stderr %q; want 0, %q, empty", args, code, stdout, stderr, "gofer v1.2.3\n")
		}
	}
}
