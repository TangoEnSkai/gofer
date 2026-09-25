// Command gofer is a lightweight, Gemini-powered agent for routine developer toil.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("gofer", version)
		return
	}
	fmt.Fprintln(os.Stderr, "gofer: not implemented yet — see docs/design.md")
	os.Exit(1)
}
