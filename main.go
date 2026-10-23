package main

import (
	"fmt"
	"os"

	"kk-password-scraper/scrapers"
)

const dataDir = "data"

func main() {
	if err := scrapers.Juedge(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "juedge scraper failed: %v\n", err)
		os.Exit(1)
	}
}
