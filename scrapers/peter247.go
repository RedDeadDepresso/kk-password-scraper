package scrapers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const peterPawchiveURL = "https://pawchive.pw/api/v1/patreon/user/23896694"

// Attachment extensions that count as an archive.
var peterArchiveExts = map[string]bool{"7z": true, "rar": true, "zip": true}

type peterPost struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Attachments []struct {
		Name string `json:"name"`
	} `json:"attachments"`
}

// Peter247 scrapes pawchive and writes <dataDir>/peter247.json.
//
// Peter247's passwords can't be scraped, so they are filled in by hand in
// data/peter247.json. Scraping stops at the newest already-saved post and the
// new entries are put in front of the existing ones, which are left untouched,
// so manual passwords are never overwritten. New posts are added with an empty
// password for you to fill in (the Pages workflow ignores empty passwords).
func Peter247(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dataDir, "peter247.json")
	client := &http.Client{Timeout: 30 * time.Second}

	existing, err := loadEntries(outPath)
	if err != nil {
		return err
	}
	// Existing passwords keyed by post id + filename (falls back to post id).
	pwByKey := make(map[string]string)
	pwByID := make(map[string]string)
	for _, e := range existing {
		if e.Password == "" {
			continue
		}
		pwByKey[e.ID+"\x00"+e.Filename] = e.Password
		if _, ok := pwByID[e.ID]; !ok {
			pwByID[e.ID] = e.Password
		}
	}

	// Stop marker: id of the newest saved post (with or without a password,
	// since new posts are usually waiting for a manual password).
	stopID := ""
	if len(existing) > 0 && existing[0].ID != "" {
		stopID = existing[0].ID
		fmt.Printf("Will stop at already-saved post: %s (%s)\n", existing[0].Title, stopID)
	}

	var fetched []JuedgeEntry
	seen := make(map[string]bool)
	hitStop := false
scan:
	for offset := 0; ; offset += pawchivePageSize {
		body, err := getBody(client, fmt.Sprintf("%s?o=%d", peterPawchiveURL, offset))
		if err != nil {
			return err // don't write anything on failure
		}
		var posts []peterPost
		if err := json.Unmarshal(body, &posts); err != nil {
			return fmt.Errorf("pawchive offset %d: %w", offset, err)
		}
		if len(posts) == 0 {
			fmt.Println("Pawchive: reached end of posts.")
			break
		}
		for _, p := range posts {
			if stopID != "" && p.ID == stopID {
				fmt.Printf("Pawchive: hit saved post, stopping (%d new entries).\n", len(fetched))
				hitStop = true
				break scan
			}
			for _, a := range p.Attachments {
				name := strings.TrimSpace(a.Name)
				dot := strings.LastIndex(name, ".")
				if dot < 0 || !peterArchiveExts[strings.ToLower(name[dot+1:])] {
					continue
				}
				pw, ok := pwByKey[p.ID+"\x00"+name]
				if !ok {
					pw = pwByID[p.ID]
				}
				fetched = append(fetched, JuedgeEntry{ID: p.ID, Title: p.Title, Filename: name, Password: pw})
				seen[p.ID] = true
				break // one archive per post
			}
		}
		fmt.Printf("Pawchive: offset %d done (%d entries so far)\n", offset, len(fetched))
		time.Sleep(500 * time.Millisecond)
	}

	var all []JuedgeEntry
	if hitStop {
		// New posts go in front of the existing ones (newest first).
		all = append(fetched, existing...)
	} else {
		// Saved post wasn't found (first run, or it was removed from pawchive):
		// full scan was done, so merge and keep saved entries pawchive no longer
		// returns, so manual work is never lost.
		all = fetched
		for _, e := range existing {
			if !seen[e.ID] {
				all = append(all, e)
			}
		}
	}

	if hitStop && len(fetched) == 0 {
		fmt.Println("No new entries. Nothing to do.")
		return nil
	}

	missing := 0
	for _, e := range all {
		if e.Password == "" {
			missing++
		}
	}
	if err := saveEntries(outPath, all); err != nil {
		return err
	}
	fmt.Printf("Saved %d entries to %s (%d still need a password)\n", len(all), outPath, missing)
	return nil
}