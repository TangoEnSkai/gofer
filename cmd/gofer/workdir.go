package main

import (
	"fmt"
	"os"

	"github.com/TangoEnSkai/gofer/internal/config"
)

// checkWorkdir refuses agent work when the current directory is on the
// configured deny list, so its contents are never sent to the model.
func checkWorkdir(cfg config.Config) error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	denied, err := cfg.IsDenied(dir)
	if err != nil {
		return fmt.Errorf("check deny_dirs for %s: %w", dir, err)
	}
	if denied {
		return fmt.Errorf("refusing to run in %s: it is inside a deny_dirs entry of the config", dir)
	}
	return nil
}
