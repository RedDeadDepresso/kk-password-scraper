package main

import (
	"fmt"
	"os"

	"kk-password-scraper/scrapers"
)

const dataDir = "data"

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
	if failed {
		os.Exit(1)
	}
}