package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)


type Config struct {
	Command   string
	Modules    []string
	BatchMode bool
}

// Load CLI flags & env vars into the struct
func NewConfig() *Config {
	cfg := &Config{}

	// Flags
	flag.BoolVar(&cfg.BatchMode, "batch", false, "Run in batch mode")

	// Customize the -h / --help output
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <command> [module...]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  prepare    Prepare target module(s)\n")
		fmt.Fprintf(os.Stderr, "  approve    Approve target module(s)\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	// Environment variable fallback
	cfg.BatchMode = cfg.BatchMode || os.Getenv("BATCH_MODE") == "true"

	// Grab positional args: args[0] = Command, args[1:] = Modules
	args := flag.Args()

	if len(args) > 0 {
		cfg.Command = args[0]
	}
	if len(args) > 1 {
		// Capture all remaining arguments as modules
		cfg.Modules = args[1:]
	}

	// Output summary
	if cfg.Command != "" {
		fmt.Printf("===================\n")
		fmt.Printf("Arguments and Flags\n")
		fmt.Printf("===================\n")
		fmt.Printf("Command:    %s\n", cfg.Command)
		fmt.Printf("Modules:    %v\n", cfg.Modules) // Displays as [mod1 mod2 ...]
		fmt.Printf("Batch Mode: %v\n", cfg.BatchMode)
		fmt.Printf("===================\n\n")
	}

	return cfg
}

// Run shell commands and stream output directly to stdout/stderr
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// Check if a directory exists
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

// Prompt the user
func confirmContinue(nextAction string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("\nContinue to '%s'? [y/N]: ", nextAction)

	input, err := reader.ReadString('\n')
	if err != nil {
		return false
	}

	input = strings.ToLower(strings.TrimSpace(input))
	return input == "y" || input == "yes"
}

func (m *Config) Prepare() error {
	
	// Enforce that exactly one module is present
	if len(m.Modules) != 1 {
		return fmt.Errorf("prepare requires exactly 1 module, but got %d", len(m.Modules))
	}

	fmt.Printf("Preparing module '%s'...\n", m.Modules[0])
	
	// git fetch --tags
	fmt.Println("Fetching git tags...")
	if err := runCommand("git", "fetch", "--tags"); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}
	
	// Run module tests only if tests subdirectory exists
	testsDir := fmt.Sprintf("%s/tests", m.Modules[0])
	if dirExists(testsDir) {
		fmt.Printf("Running tests for %s...\n", m.Modules[0])
		if err := runCommand("dagger", "-m", testsDir, "checks"); err != nil {
			return fmt.Errorf("dagger tests failed: %w", err)
		}
		} else {
			fmt.Printf("No tests directory found for '%s', skipping tests.\n", m.Modules[0])
		}
		
		// Run dagger prepare
		if err := runCommand("dagger", "call", "--auto-apply", "--progress=dots", "--module="+m.Modules[0], "prepare"); err != nil {
			return fmt.Errorf("dagger prepare failed: %w", err)
		}
		
		// Read VERSION file
		versionFile := fmt.Sprintf("%s/VERSION", m.Modules[0])
		versionBytes, err := os.ReadFile(versionFile)
		if err != nil {
			return fmt.Errorf("could not read version file at %s: %w", versionFile, err)
		}
		version := strings.TrimSpace(string(versionBytes))
		
		// Skip prompt if in batch mode
		if m.BatchMode {
			fmt.Println("Skip prompt, running in batch mode")
			return nil
		}
		
	// 7. Prompt user to continue to approve
	fmt.Printf("Please review the local changes, especially %s/releases/%s.md\n", m.Modules[0], version)
	if confirmContinue("approve") {
		return m.Approve()
	}
	
	return nil
}

func (m *Config) Approve() error {
	// Enforce exactly one module
	if len(m.Modules) != 1 {
		return fmt.Errorf("approve requires exactly 1 module, but got %d", len(m.Modules))
	}
	
	fmt.Printf("Approve module '%s'...\n", m.Modules[0])

	
	// Read VERSION file
	versionFile := fmt.Sprintf("%s/VERSION", m.Modules[0])
	versionBytes, err := os.ReadFile(versionFile)
	if err != nil {
		return fmt.Errorf("could not read version file at %s: %w", versionFile, err)
	}
	version := strings.TrimSpace(string(versionBytes))

	changelogPath := fmt.Sprintf("%s/CHANGELOG.md", m.Modules[0])
	notesPath := fmt.Sprintf("%s/releases/v%s.md", m.Modules[0], version)
	releaseTag := fmt.Sprintf("%s/v%s", m.Modules[0], version)

	// Stage release materials
	fmt.Printf("Staging release files for %s...\n", m.Modules[0])
	if err := runCommand("git", "add", versionFile, changelogPath, notesPath); err != nil {
		return fmt.Errorf("git add failed: %w", err)
	}

	// Create signed commit
	commitMsg := fmt.Sprintf("chore(release): prepare for %s", releaseTag)
	fmt.Println("Creating signed commit...")
	if err := runCommand("git", "commit", "-S", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit failed: %w", err)
	}

	// Create annotated and signed tag
	tagMsg := fmt.Sprintf("Official release %s", releaseTag)
	fmt.Printf("Creating signed tag %s...\n", releaseTag)
	if err := runCommand("git", "tag", "-s", "-a", "-m", tagMsg, releaseTag); err != nil {
		return fmt.Errorf("git tag failed: %w", err)
	}

	// Skip prompt if in batch mode
	if m.BatchMode {
		fmt.Println("Skip prompt, running in batch mode")
		return nil
	}

	// 7. Prompt user to continue to publish step
	fmt.Printf("Please review the local changes, especially %s\n", notesPath)
	if confirmContinue("publish") {
		return m.Publish() // Assuming you have a Publish method ready for the next step!
	}

	return nil
}

// Publish handles publishing the approved modules.
func (m *Config) Publish() error {
	// 1. Enforce that at least one module is present
	if len(m.Modules) == 0 {
		return fmt.Errorf("publish requires at least 1 module")
	}

	fmt.Printf("Publishing module(s): %s...\n", strings.Join(m.Modules, ", "))

	// TODO: Add your publishing logic here (e.g., git push, release tags, artifact uploads)

	return nil
}
