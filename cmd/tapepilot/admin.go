package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"golang.org/x/term"
	"os"
)

func adminCommand(r *app.Runtime, args []string) error {
	if len(args) != 1 || (args[0] != "bootstrap" && args[0] != "reset") {
		return errors.New("expected admin bootstrap or admin reset")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("administrator credentials require an interactive local terminal")
	}
	fmt.Fprint(os.Stderr, "New administrator password (12–1024 bytes): ")
	first, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	defer clear(first)
	fmt.Fprint(os.Stderr, "Confirm password: ")
	second, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	defer clear(second)
	if !bytes.Equal(first, second) {
		return errors.New("password confirmation does not match")
	}
	if args[0] == "bootstrap" {
		return auth.Bootstrap(context.Background(), r.Store, string(first))
	}
	return auth.Reset(context.Background(), r.Store, string(first))
}
