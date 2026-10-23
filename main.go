package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"kk-password-scraper/scrapers"
)

const dataDir = "data"

// ---------- git ----------

func git(args ...string) (string, error) {
	out, err := exec.Command("git", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// commitChanges stages the data folder, commits and pushes if anything changed.
func commitChanges(message string) error {
	if out, err := git("add", dataDir); err != nil {
		return fmt.Errorf("git add failed: %v: %s", err, out)
	}

	// `git commit` exits non-zero when there's nothing to commit,
	// so check first to avoid treating that as a failure.
	status, err := git("status", "--porcelain", dataDir)
	if err != nil {
		return fmt.Errorf("git status failed: %v: %s", err, status)
	}
	if status == "" {
		fmt.Println("Nothing to commit")
		return nil
	}

	if out, err := git("commit", "-m", message); err != nil {
		return fmt.Errorf("git commit failed: %v: %s", err, out)
	}
	fmt.Println("Changes committed")

	if out, err := git("push"); err != nil {
		return fmt.Errorf("git push failed: %v: %s", err, out)
	}
	fmt.Println("Changes pushed")
	return nil
}

// ---------- main ----------

func main() {
	jobs := []struct {
		name string
		run  func(string) error
	}{
		{"juedge", scrapers.Juedge},
		{"rurri00", scrapers.Rurri00},
	}

	failed := false
	for _, j := range jobs {
		fmt.Printf("=== %s ===\n", j.name)
		if err := j.run(dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "%s scraper failed: %v\n", j.name, err)
			failed = true
		}
	}

	// Commit whatever succeeded, even if another scraper failed.
	if err := commitChanges("Update password data"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if failed {
		os.Exit(1)
	}
}