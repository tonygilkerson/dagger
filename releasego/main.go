package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNothingToBump is returned when a module has no changes to release.
var ErrNothingToBump = errors.New("there was nothing to bump")

type Config struct {
	Command   string
	Modules   []string
	BatchMode bool
	DryRun    bool
	RootDir   string
}

// Load CLI flags & env vars into the struct
func NewConfig() *Config {
	cfg := &Config{}

	// Flags
	flag.BoolVar(&cfg.BatchMode, "batch", false, "Run in batch mode")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "Perform a dry run without applying changes")

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
	cfg.DryRun = cfg.DryRun || os.Getenv("DRY_RUN") == "true"

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
		fmt.Printf("Modules:    %v\n", cfg.Modules)
		fmt.Printf("Batch Mode: %v\n", cfg.BatchMode)
		fmt.Printf("Dry Run:    %v\n", cfg.DryRun)
		fmt.Printf("===================\n\n")
	}

	return cfg
}

// Get the directory of the root git project
// This function assumes the source dir it one level under the root
// For example:
//
//	/path/to/dagger/releasego/main.go will return /path/to/dagger
func getRootProjDir() string {

	_, currentFilePath, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintf(os.Stderr, "Error: could not get current file path\n")
		os.Exit(1)
	}
	// First filepath.Dir strips main.go (gets releasego)
	// Second filepath.Dir strips releasego (gets dagger)
	return filepath.Dir(filepath.Dir(currentFilePath))
}

// Run shell commands and stream output directly to stdout/stderr
func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// Execute a function while redirecting standard output to a custom writer (like MultiWriter)
func runWithCapturedOutput(w io.Writer, fn func() error) error {
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	r, pipeW, _ := os.Pipe()
	os.Stdout = pipeW
	os.Stderr = pipeW

	outDone := make(chan struct{})
	go func() {
		io.Copy(w, r)
		close(outDone)
	}()

	err := fn()

	pipeW.Close()
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	<-outDone

	return err
}


// Check if a directory exists
func dirExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

// Check if a file exists
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
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

// check if a module has changed relative to its git tag.
func hasModuleChanged(module, currentTag string) (bool, error) {
	// Check if the git tag exists
	err := exec.Command("git", "rev-parse", "--verify", currentTag).Run()
	if err != nil {
		// Tag doesn't exist yet -> module has changed/new
		return true, nil
	}

	// Tag exists: check if files in module directory changed since that tag
	err = exec.Command("git", "diff", "--quiet", currentTag, "HEAD", "--", module).Run()
	if err != nil {
		// Exit code 1 means differences exist
		return true, nil
	}
	return false, nil
}

func (m *Config) runPrepareDaggerCommand() error {
	cmd := exec.Command("dagger", "call", "--auto-apply", "--progress=dots", "--module="+m.Modules[0], "prepare")

	// Buffer to record the output for string matching
	var buf bytes.Buffer

	// Stream to stdout/stderr in real-time while also saving to buf
	cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &buf)

	if err := cmd.Run(); err != nil {
		output := buf.String()

		// Check if the output contains our bypass phrase
		if strings.Contains(output, "there was nothing to bump") {
			return ErrNothingToBump
		}

		return fmt.Errorf("dagger prepare failed: %w", err)
	}

	return nil
}

func (m *Config) PrepareAll() error {

	// Ensure no module arguments were accidentally passed
	if len(m.Modules) > 0 {
		return fmt.Errorf("prepare-all does not accept module arguments, but got: %v", m.Modules)
	}

	// Fetch git tags
	fmt.Println("Fetching git tags...")
	if err := runCommand("git", "fetch", "--tags"); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}

	// Used to hold a list of modules that were prepared
	var preparedModules []string

	// Scan directory for submodules
	dirs, err := os.ReadDir(m.RootDir)
	if err != nil {
		return fmt.Errorf("failed to read root directory: %w", err)
	}

	for _, dir := range dirs {
		// Only inspect directories
		if !dir.IsDir() {
			continue
		}

		mod := dir.Name()

		// Skip reserved or hidden folders
		if mod == "bin" || mod == ".dagger" || mod == "releasego" || strings.HasPrefix(mod, ".") {
			continue
		}

		versionFile := fmt.Sprintf("%s/%s", m.RootDir, mod) + "/VERSION"

		// Require VERSION file inside module folder
		if !fileExists(versionFile) {
			return fmt.Errorf("version file missing for module '%s' at path: %s", mod, versionFile)
		}		

		// Read version and construct expected tag
		versionBytes, err := os.ReadFile(versionFile)
		if err != nil {
			return fmt.Errorf("error reading %s: %w", versionFile, err)
		}
		version := strings.TrimSpace(string(versionBytes))
		currentTag := fmt.Sprintf("%s/v%s", mod, version)

		// Filter out unchanged modules prior to execution
		changed, err := hasModuleChanged(mod, currentTag)
		if err != nil {
			return fmt.Errorf("[%s] error checking change status: %w", mod, err)
		}
		if !changed {
			continue
		}

		////////////////////////////////####
		// Prepare Module
		////////////////////////////////####
		fmt.Printf("\n----------------------------------------\n")
		fmt.Printf("Prepare module: %s\n", mod)
		fmt.Printf("----------------------------------------\n")

		if m.DryRun {
			fmt.Printf("[DRY-RUN] Would have prepared module: %s\n", mod)
			preparedModules = append(preparedModules, mod)
			continue
		}

		// Instantiate a sub-config for the single module run in batch mode
		subCfg := &Config{
			Command:   "prepare",
			Modules:   []string{mod},
			BatchMode: true,
			DryRun:    m.DryRun,
		}

		// Execute Prepare for the module
		prepErr := subCfg.Prepare()

		switch {
		case prepErr == nil:
			preparedModules = append(preparedModules, mod)

		case errors.Is(prepErr, ErrNothingToBump):
			fmt.Printf("[%s] Skipping '%s': No changes detected, there was nothing to bump.\n", mod, mod)

		default:
			// Real failure: abort pipeline execution immediately
			return fmt.Errorf("[%s] ERROR: prepare failed: %w", mod, prepErr)
		}
	}

	////////////////////////////////####
	// Summary & Confirmation
	////////////////////////////////####
	fmt.Printf("\n===================\n")
	fmt.Printf("Summary of Prepared Modules:\n")
	fmt.Printf("===================\n")

	if len(preparedModules) == 0 {
		fmt.Println("No modules were bumped.")
		return nil
	}

	for _, mod := range preparedModules {
		verBytes, _ := os.ReadFile(fmt.Sprintf("%s/VERSION", mod))
		fmt.Printf("  - %s: %s\n", mod, strings.TrimSpace(string(verBytes)))
	}

	if m.BatchMode {
		fmt.Println("\nBatch mode enabled, skipping prompt.")
		return nil
	}

	// DEVTODO
	// Ask user to continue to approve-all
	// if confirmContinue("approve-all") {
	// 	approveCfg := &Config{
	// 		Command:   "approve-all",
	// 		Modules:   preparedModules,
	// 		BatchMode: m.BatchMode,
	// 		DryRun:    m.DryRun,
	// 	}
	// 	return approveCfg.ApproveAll()
	// }

	return nil
}

func (m *Config) Prepare() error {

	// Enforce that exactly one module is present
	if len(m.Modules) != 1 {
		return fmt.Errorf("prepare requires exactly 1 module, but got %d", len(m.Modules))
	}

	// Make sure the module dir exits
	if !dirExists(fmt.Sprintf("%s/%s", m.RootDir, m.Modules[0])) {
		return fmt.Errorf("module directory does not exist: %s/%s", m.RootDir, m.Modules[0])
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
	if err := m.runPrepareDaggerCommand(); err != nil {
		return err
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

	// Prompt user to continue to approve
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

func main() {

	// Run from project root
	rootProjDir := getRootProjDir()
	os.Chdir(rootProjDir)
	fmt.Println("Start release...")
	fmt.Printf("Ensure we are running from project root, change working directory to=[%s]\n", rootProjDir)

	// Initialize a new configuration struct
	cfg := NewConfig()
	cfg.RootDir = rootProjDir

	// Call the right method based on the command
	switch cfg.Command {
	case "prepare-all":
		if err := cfg.PrepareAll(); err != nil {
			fmt.Fprintf(os.Stderr, "Error during prepare-all: %v\n", err)
			os.Exit(1)
		}
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
