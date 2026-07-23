package main

import (
	"fmt"
	"os"
)

func main() {
	// Populate the struct
	cfg := NewConfig()

	// Call the right method based on the command
	switch cfg.Command {
	case "prepare":
		if err := cfg.Prepare(); err != nil {
			fmt.Fprintf(os.Stderr, "Error during prepare: %v\n", err)
			os.Exit(1)
		}
	case "approve":
		if err := cfg.Approve(); err != nil {
			fmt.Fprintf(os.Stderr, "Error during approve: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("Unknown or missing command: '%s'\n", cfg.Command)
		os.Exit(1)
	}
}
