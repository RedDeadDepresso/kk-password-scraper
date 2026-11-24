package scrapers

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const makisePawchiveURL = "https://pawchive.pw/api/v1/fanbox/user/77965060"

type makisePost struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// Content looks like:
//
//	<p><a href="https://mega.nz/file/..."></a></p><p></p><p>password:</p><p>PASSWORD</p>
var (
	makisePasswordRe = regexp.MustCompile(`(?is)password\s*[:：]?\s*(?:</p>\s*<p[^>]*>)?(.*?)</p>`)
	htmlTagRe        = regexp.MustCompile(`<[^>]*>`)
)

// makisePassword pulls the password out of a post's HTML content ("" if none).
func makisePassword(content string) string {
	m := makisePasswordRe.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	pw := htmlTagRe.ReplaceAllString(m[1], "")
	return strings.TrimSpace(html.UnescapeString(pw))
}

// Makise scrapes pawchive (fanbox) + mega and writes <dataDir>/makise.json.
func Makise(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dataDir, "makise.json")
	client := &http.Client{Timeout: 30 * time.Second}

	existing, err := loadEntries(outPath)
	if err != nil {
		return err
	}

	// Stop marker: id of the first saved post (only if it has a password).
	stopID := ""
	if len(existing) > 0 && existing[0].ID != "" && existing[0].Password != "" {
		stopID = existing[0].ID
		fmt.Printf("Will stop at already-saved post: %s (%s)\n", existing[0].Title, stopID)
	}

	var newEntries []JuedgeEntry
scan:
	for offset := 0; ; offset += pawchivePageSize {
		body, err := getBody(client, fmt.Sprintf("%s?o=%d", makisePawchiveURL, offset))
		if err != nil {
			return err
		}
		var posts []makisePost
		if err := json.Unmarshal(body, &posts); err != nil {
			return fmt.Errorf("pawchive offset %d: %w", offset, err)
		}
		if len(posts) == 0 {
			fmt.Println("Pawchive: reached end of posts.")
			break
		}

		for _, p := range posts {
			if stopID != "" && p.ID == stopID {
				fmt.Printf("Pawchive: hit saved post, stopping (%d new entries).\n", len(newEntries))
				break scan
			}
			m := megaFileRe.FindStringSubmatch(p.Content)
			if m == nil {
				fmt.Printf("Skipping (no mega file link): %s\n", p.Title)
				continue
			}
			pw := makisePassword(p.Content)
			if pw == "" {
				fmt.Printf("Skipping (no password): %s\n", p.Title)
				continue
			}
			filename, err := megaFilename(client, m[1], m[2])
			if err != nil {
				fmt.Printf("Skipping (mega lookup failed: %v): %s\n", err, p.Title)
				continue
			}
			newEntries = append(newEntries, JuedgeEntry{ID: p.ID, Title: p.Title, Filename: filename, Password: pw})
			fmt.Printf("Added: %s -> %s\n", p.Title, filename)
			time.Sleep(300 * time.Millisecond)
		}
		fmt.Printf("Pawchive: offset %d done (%d new entries so far)\n", offset, len(newEntries))
		time.Sleep(500 * time.Millisecond)
	}

	if len(newEntries) == 0 {
		fmt.Println("No new entries. Nothing to do.")
		return nil
	}

	// New posts go in front of the existing ones (newest first).
	all := append(newEntries, existing...)
	if err := saveEntries(outPath, all); err != nil {
		return err
	}
	fmt.Printf("Saved %d new entries (%d total) to %s\n", len(newEntries), len(all), outPath)
	return nil
}