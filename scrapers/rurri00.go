package scrapers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	rurriVanity    = "Rurri00Koikatsu"
	rurriPageURL   = "https://www.patreon.com/cw/" + rurriVanity
	rurriStopTitle = "🔑 information about the p@sswoгd system. 🔑"

	// Patreon campaign id (the filter[campaign_id] value seen in the browser's
	// network tab on the creator page). Leave empty to try auto-detecting it
	// from the creator page, which Patreon's bot protection may block.
	rurriCampaignID = ""

	// Optional: the full https://www.patreon.com/api/posts?... request URL that
	// the browser makes when you open https://www.patreon.com/cw/Rurri00Koikatsu/posts
	// (DevTools > Network > filter "posts"). When set, its query parameters are
	// used as-is for every page and rurriCampaignID is not needed.
	rurriAPIURL = ""
)

// Same output shape as juedge.json: {title, filename, password}.
type rurriEntry = JuedgeEntry

type rurriAttributes struct {
	Title                      string   `json:"title"`
	AttachmentsPreviewMetadata []json.RawMessage `json:"attachments_preview_metadata"`
	PostMetadata               struct {
		ImageOrder []json.RawMessage `json:"image_order"`
	} `json:"post_metadata"`
}

type rurriRelationships struct {
	Media  mediaRelationship `json:"media"`
	Images mediaRelationship `json:"images"`
}

type rurriPost struct {
	ID            string             `json:"id"`
	Attributes    rurriAttributes    `json:"attributes"`
	Relationships rurriRelationships `json:"relationships"`
}

type rurriResponse struct {
	Data     []rurriPost        `json:"data"`
	Included []includedResource `json:"included"`
	Meta     struct {
		Pagination struct {
			Cursors struct {
				Next *string `json:"next"`
			} `json:"cursors"`
		} `json:"pagination"`
	} `json:"meta"`
}

var campaignIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`"campaign_id"\s*:\s*"?(\d+)`),
	regexp.MustCompile(`api/campaigns/(\d+)`),
	regexp.MustCompile(`"campaign"\s*:\s*\{\s*"data"\s*:\s*\{\s*"id"\s*:\s*"(\d+)"`),
}

// Rurri00 scrapes patreon.com/cw/Rurri00Koikatsu and writes <dataDir>/rurri00.json.
func Rurri00(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dataDir, "rurri00.json")
	client := &http.Client{Timeout: 30 * time.Second}

	existing, err := loadEntries(outPath)
	if err != nil {
		return err
	}
	stopFirst := ""
	if len(existing) > 0 {
		stopFirst = normTitle(existing[0].Title)
		fmt.Printf("Will stop at already-saved post: %s\n", existing[0].Title)
	}

	var q url.Values
	if rurriAPIURL != "" {
		u, err := url.Parse(rurriAPIURL)
		if err != nil {
			return fmt.Errorf("invalid rurriAPIURL: %w", err)
		}
		q = u.Query()
		q.Del("page[cursor]")
	} else {
		campaignID := rurriCampaignID
		if campaignID == "" {
			campaignID, err = resolveCampaignID(client)
			if err != nil {
				return err
			}
			fmt.Printf("Detected campaign id: %s\n", campaignID)
		}
		q = patreonQuery()
		q.Set("filter[campaign_id]", campaignID)
	}

	newEntries, err := fetchRurriEntries(client, q, stopFirst)
	if err != nil {
		return err
	}
	if len(newEntries) == 0 {
		fmt.Println("No new entries. Nothing to do.")
		return nil
	}

	// New entries go in front of the existing ones (newest first).
	all := append(newEntries, existing...)
	if err := saveEntries(outPath, all); err != nil {
		return err
	}
	fmt.Printf("Saved %d new entries (%d total) to %s\n", len(newEntries), len(all), outPath)
	return nil
}

func resolveCampaignID(client *http.Client) (string, error) {
	body, err := getBody(client, rurriPageURL)
	if err != nil {
		return "", fmt.Errorf("could not load creator page to detect campaign id (set rurriCampaignID in rurri00.go): %w", err)
	}
	for _, re := range campaignIDPatterns {
		if m := re.FindSubmatch(body); m != nil {
			return string(m[1]), nil
		}
	}
	return "", fmt.Errorf("campaign id not found on creator page (set rurriCampaignID in rurri00.go)")
}

func fetchRurriEntries(client *http.Client, q url.Values, stopFirst string) ([]rurriEntry, error) {
	stopSystem := normTitle(rurriStopTitle)

	var out []rurriEntry
	for page := 1; ; page++ {
		body, err := getBody(client, patreonPostsURL+"?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("patreon page %d: %w", page, err)
		}
		var result rurriResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("patreon page %d: %w", page, err)
		}

		mediaMap := make(map[string]mediaAttributes)
		for _, item := range result.Included {
			if item.Type == "media" {
				mediaMap[item.ID] = item.Attributes
			}
		}

		for _, post := range result.Data {
			title := normTitle(post.Attributes.Title)
			if title == stopSystem {
				fmt.Printf("Hit password-system post, stopping (%d new entries).\n", len(out))
				return out, nil
			}
			if stopFirst != "" && title == stopFirst {
				fmt.Printf("Hit saved post, stopping (%d new entries).\n", len(out))
				return out, nil
			}

			var files []string
			for _, raw := range post.Attributes.AttachmentsPreviewMetadata {
				if name := attachmentName(raw); name != "" {
					files = append(files, name)
				} else {
					fmt.Printf("Unrecognized attachment item in %q: %s\n", post.Attributes.Title, string(raw))
				}
			}
			if len(files) == 0 {
				fmt.Printf("Skipping (no attachments): %s\n", post.Attributes.Title)
				continue
			}
			password := lastScreenshotCaption(post, mediaMap)
			if password == "" {
				fmt.Printf("Skipping (no password): %s\n", post.Attributes.Title)
				continue
			}
			for _, f := range files {
				f = strings.TrimSpace(f)
				if f == "" {
					continue
				}
				out = append(out, rurriEntry{Title: post.Attributes.Title, Filename: f, Password: password})
			}
		}
		fmt.Printf("Patreon: page %d done (%d posts, %d entries so far)\n", page, len(result.Data), len(out))

		next := result.Meta.Pagination.Cursors.Next
		if next == nil || *next == "" {
			fmt.Println("Reached end of pagination.")
			return out, nil
		}
		q.Set("page[cursor]", *next)
		time.Sleep(500 * time.Millisecond)
	}
}

// attachmentName extracts a filename from an attachments_preview_metadata item,
// which may be a plain string or an object with a name-like field.
func attachmentName(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj map[string]interface{}
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, k := range []string{"file_name", "filename", "name", "title", "original_file_name"} {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// lastScreenshotCaption returns the caption of the post's last image ("" if none).
func lastScreenshotCaption(post rurriPost, mediaMap map[string]mediaAttributes) string {
	// Prefer the display order Patreon stores in post_metadata.image_order,
	// then fall back to the media / images relationships.
	var ids []string
	for _, raw := range post.Attributes.PostMetadata.ImageOrder {
		if id := strings.Trim(string(raw), `" `); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		refs := post.Relationships.Media.Data
		if len(refs) == 0 {
			refs = post.Relationships.Images.Data
		}
		for _, r := range refs {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		fmt.Printf("  [debug] post %s: no screenshots found\n", post.ID)
		return ""
	}
	lastID := ids[len(ids)-1]
	attr, ok := mediaMap[lastID]
	if !ok {
		fmt.Printf("  [debug] post %s: last image %s not in included media\n", post.ID, lastID)
		return ""
	}
	pw := attr.Caption
	if pw == "" && attr.Metadata != nil {
		if v, ok := attr.Metadata["caption"].(string); ok {
			pw = v
		}
	}
	return strings.TrimSpace(pw)
}