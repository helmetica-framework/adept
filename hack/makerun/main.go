package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
)

// This is a helper program to run the controller for local development.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Println("Running controller")
	cmd := exec.CommandContext(ctx, "go", "run", "main.go", "controller")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}
