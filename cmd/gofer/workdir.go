package main

import (
	"fmt"
	"os"

	goferapp "github.com/TangoEnSkai/gofer/internal/app"
	"github.com/TangoEnSkai/gofer/internal/config"
)

// checkWorkdir refuses agent work when the current directory is on the
// configured deny list, so its contents are never sent to the model.
func checkWorkdir(cfg config.Config) error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	return goferapp.CheckWorkdir(cfg, dir)
}
