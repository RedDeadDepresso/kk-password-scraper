package scrapers

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	pawchiveURL       = "https://pawchive.pw/api/v1/patreon/user/58585170"
	pawchivePageSize  = 50
	patreonPostsURL   = "https://www.patreon.com/api/posts"
	patreonCampaignID = "8801167"
	megaAPIURL        = "https://g.api.mega.co.nz/cs"
	userAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

// Output format of data/juedge.json
type JuedgeEntry struct {
	Title    string `json:"title"`
	Filename string `json:"filename"`
	Password string `json:"password"`
}

// ---------- Pawchive ----------

type pawchivePost struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

type candidate struct {
	Title   string
	MegaID  string
	MegaKey string
}

// Matches https://mega.nz/file/<id>#<key> and the legacy https://mega.nz/#!<id>!<key>
var megaFileRe = regexp.MustCompile(`https?://mega\.(?:nz|co\.nz)/(?:file/|#!)([A-Za-z0-9_-]{8})[#!]([A-Za-z0-9_-]{43})`)

// ---------- Patreon ----------

type mediaAttributes struct {
	Caption  string                 `json:"caption"`
	FileName string                 `json:"file_name"`
	Metadata map[string]interface{} `json:"metadata"`
}

type includedResource struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Attributes mediaAttributes `json:"attributes"`
}

type relationshipData struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type mediaRelationship struct {
	Data []relationshipData `json:"data"`
}

type postRelationships struct {
	Media            mediaRelationship `json:"media"`
	AttachmentsMedia mediaRelationship `json:"attachments_media"`
}

type postAttributes struct {
	Title string `json:"title"`
}

type postData struct {
	ID            string            `json:"id"`
	Attributes    postAttributes    `json:"attributes"`
	Relationships postRelationships `json:"relationships"`
}

type patreonResponse struct {
	Data     []postData         `json:"data"`
	Included []includedResource `json:"included"`
	Meta     struct {
		Pagination struct {
			Cursors struct {
				Next *string `json:"next"`
			} `json:"cursors"`
		} `json:"pagination"`
	} `json:"meta"`
}

// ---------- Entry point ----------

// Juedge scrapes pawchive + mega + patreon and writes <dataDir>/juedge.json.
func Juedge(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dataDir, "juedge.json")
	client := &http.Client{Timeout: 30 * time.Second}

	existing, err := loadEntries(outPath)
	if err != nil {
		return err
	}

	// Stop marker: title of the first saved post (only if it has a password).
	stopTitle := ""
	if len(existing) > 0 && existing[0].Password != "" {
		stopTitle = normTitle(existing[0].Title)
		fmt.Printf("Will stop at already-saved post: %s\n", existing[0].Title)
	}

	// 1. Pawchive posts (newest first) with their mega links.
	candidates, err := fetchPawchive(client, stopTitle)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No new posts with mega links. Nothing to do.")
		return nil
	}

	// 2. Patreon passwords (post title -> caption of first media).
	needed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		needed[normTitle(c.Title)] = true
	}
	passwords, err := fetchPatreonPasswords(client, needed)
	if err != nil {
		return err
	}

	// 3. Resolve mega filenames only for posts that have a password.
	newEntries := make([]JuedgeEntry, 0, len(candidates))
	for _, c := range candidates {
		pw := passwords[normTitle(c.Title)]
		if pw == "" {
			fmt.Printf("Skipping (no password): %s\n", c.Title)
			continue
		}
		filename, err := megaFilename(client, c.MegaID, c.MegaKey)
		if err != nil {
			fmt.Printf("Skipping (mega lookup failed: %v): %s\n", err, c.Title)
			continue
		}
		newEntries = append(newEntries, JuedgeEntry{Title: c.Title, Filename: filename, Password: pw})
		fmt.Printf("Added: %s -> %s\n", c.Title, filename)
		time.Sleep(300 * time.Millisecond)
	}

	// New posts go in front of the existing ones (newest first).
	all := append(newEntries, existing...)
	if err := saveEntries(outPath, all); err != nil {
		return err
	}
	fmt.Printf("Saved %d new entries (%d total) to %s\n", len(newEntries), len(all), outPath)
	return nil
}

// ---------- JSON file ----------

func loadEntries(path string) ([]JuedgeEntry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []JuedgeEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return []JuedgeEntry{}, nil
	}
	var entries []JuedgeEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return entries, nil
}

func saveEntries(path string, entries []JuedgeEntry) error {
	if entries == nil {
		entries = []JuedgeEntry{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(entries); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// ---------- Helpers ----------

// normTitle collapses whitespace so titles match across pawchive and patreon.
func normTitle(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func getBody(client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("GET %s: status %d: %s", u, resp.StatusCode, snippet)
	}
	return body, nil
}

// ---------- Pawchive ----------

func fetchPawchive(client *http.Client, stopTitle string) ([]candidate, error) {
	var out []candidate
	for offset := 0; ; offset += pawchivePageSize {
		body, err := getBody(client, fmt.Sprintf("%s?o=%d", pawchiveURL, offset))
		if err != nil {
			return nil, err
		}
		var posts []pawchivePost
		if err := json.Unmarshal(body, &posts); err != nil {
			return nil, fmt.Errorf("pawchive offset %d: %w", offset, err)
		}
		if len(posts) == 0 {
			fmt.Println("Pawchive: reached end of posts.")
			return out, nil
		}

		for _, p := range posts {
			if stopTitle != "" && normTitle(p.Title) == stopTitle {
				fmt.Printf("Pawchive: hit saved post, stopping (%d new candidates).\n", len(out))
				return out, nil
			}
			m := megaFileRe.FindStringSubmatch(p.Content)
			if m == nil {
				fmt.Printf("Skipping (no mega file link): %s\n", p.Title)
				continue
			}
			out = append(out, candidate{Title: p.Title, MegaID: m[1], MegaKey: m[2]})
		}
		fmt.Printf("Pawchive: offset %d done (%d candidates so far)\n", offset, len(out))
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------- Mega ----------

// megaFilename decrypts the node attributes of a public mega file link.
func megaFilename(client *http.Client, id, keyB64 string) (string, error) {
	key, err := base64.RawURLEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("invalid mega key")
	}
	// AES key = first 16 bytes XOR last 16 bytes of the 32 byte file key.
	aesKey := make([]byte, 16)
	for i := range aesKey {
		aesKey[i] = key[i] ^ key[i+16]
	}

	at, err := megaAttrs(client, id)
	if err != nil {
		return "", err
	}
	enc, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(at, "="))
	if err != nil || len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return "", fmt.Errorf("invalid encrypted attributes")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(enc))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plain, enc)

	plain = bytes.TrimRight(plain, "\x00")
	if !bytes.HasPrefix(plain, []byte("MEGA")) {
		return "", fmt.Errorf("decryption failed (bad key?)")
	}
	var attr struct {
		Name string `json:"n"`
	}
	if err := json.Unmarshal(plain[4:], &attr); err != nil || attr.Name == "" {
		return "", fmt.Errorf("could not parse attributes")
	}
	return attr.Name, nil
}

// megaAttrs returns the encrypted "at" attribute string for a public file handle.
func megaAttrs(client *http.Client, id string) (string, error) {
	payload := fmt.Sprintf(`[{"a":"g","p":%q}]`, id)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, megaAPIURL, strings.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		// Errors come back as a bare integer or as [integer].
		var code int
		if json.Unmarshal(body, &code) == nil {
			if code == -3 || code == -4 { // EAGAIN / ERATELIMIT: retry
				lastErr = fmt.Errorf("mega error %d", code)
				continue
			}
			return "", fmt.Errorf("mega error %d", code)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil || len(arr) == 0 {
			return "", fmt.Errorf("unexpected mega response")
		}
		if json.Unmarshal(arr[0], &code) == nil {
			if code == -3 || code == -4 {
				lastErr = fmt.Errorf("mega error %d", code)
				continue
			}
			return "", fmt.Errorf("mega error %d (-9 = link removed)", code)
		}
		var info struct {
			At string `json:"at"`
		}
		if err := json.Unmarshal(arr[0], &info); err != nil || info.At == "" {
			return "", fmt.Errorf("mega response missing attributes (folder link?)")
		}
		return info.At, nil
	}
	return "", lastErr
}

// ---------- Patreon ----------

func patreonQuery() url.Values {
	q := url.Values{}
	q.Set("include", "campaign,access_rules,access_rules.tier.null,attachments_media,audio,audio_preview.null,custom_thumbnail_media.null,drop,images,media,native_video_insights,primary_image,poll.choices,poll.current_user_responses.user,poll.current_user_responses.choice,poll.current_user_responses.poll,shows.null,user,user_defined_tags,video.null,content_unlock_options.product_variant.null,content_unlock_options.reward.null,content_unlock_options.product_variant.collection.null,livestream,livestream.state,livestream.display,rss_synced_feed,post_new_comment_identity,post_new_comment_identity.avatar,post_new_comment_identity.community_profile,post_new_comment_identity.identity_badges,authors,authors.avatar,authors.community_profile")
	q.Set("fields[campaign]", "currency,show_audio_post_download_links,avatar_photo_url,avatar_photo_image_urls,is_nsfw,is_monthly,name,url,patron_count,primary_theme_color,show_free_membership_cta")
	q.Set("fields[post]", "attachments_preview_metadata,change_visibility_at,comment_count,commenter_count,content_json_string,created_at,edited_at,current_user_can_comment,current_user_can_delete,current_user_can_edit,current_user_can_report,current_user_can_view,current_user_comment_disallowed_reason,current_user_has_liked,current_user_has_reshared,embed,image,insights_last_updated_at,is_paid,is_preview_blurred,has_custom_thumbnail,like_count,reshare_count,meta_image_url,min_cents_pledged_to_view,monetization_ineligibility_reason,post_file,post_metadata,published_at,patreon_url,post_type,pledge_url,preview_asset_type,thumbnail,thumbnail_url,teaser_text_json_string,content_teaser_text,cleaned_teaser_text,title,upgrade_url,url,was_posted_by_campaign_owner,has_ti_violation,moderation_status,post_level_suspension_removal_date,pls_one_liners_by_category,video,video_preview,view_count,content_unlock_options,is_new_to_current_user,watch_state,authors,is_fan_giftable,is_header_media_free")
	q.Set("fields[media]", "id,image_urls,display,download_url,metadata,file_name,state,caption")
	q.Set("filter[campaign_id]", patreonCampaignID)
	q.Set("filter[contains_exclusive_posts]", "true")
	q.Set("filter[is_draft]", "false")
	q.Set("filter[include_lives]", "true")
	q.Set("filter[include_drops]", "true")
	q.Set("sort", "-published_at")
	q.Set("json-api-use-default-includes", "false")
	q.Set("json-api-version", "1.0")
	return q
}

// postPassword returns the caption of the post's first media item ("" if none).
func postPassword(post postData, mediaMap map[string]mediaAttributes) string {
	refs := post.Relationships.Media.Data
	if len(refs) == 0 {
		refs = post.Relationships.AttachmentsMedia.Data
	}
	if len(refs) == 0 {
		return ""
	}
	attr, ok := mediaMap[refs[0].ID]
	if !ok {
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

// fetchPatreonPasswords pages through patreon posts until every needed title
// has been seen (or pagination ends). Returns normalized title -> password.
func fetchPatreonPasswords(client *http.Client, needed map[string]bool) (map[string]string, error) {
	passwords := make(map[string]string)
	seen := make(map[string]bool)
	q := patreonQuery()

	for page := 1; ; page++ {
		body, err := getBody(client, patreonPostsURL+"?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("patreon page %d: %w", page, err)
		}
		var result patreonResponse
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
			key := normTitle(post.Attributes.Title)
			if seen[key] {
				continue
			}
			seen[key] = true
			if pw := postPassword(post, mediaMap); pw != "" {
				passwords[key] = pw
			}
		}
		fmt.Printf("Patreon: page %d done (%d posts)\n", page, len(result.Data))

		allFound := true
		for t := range needed {
			if !seen[t] {
				allFound = false
				break
			}
		}
		next := result.Meta.Pagination.Cursors.Next
		if allFound || next == nil || *next == "" {
			return passwords, nil
		}
		q.Set("page[cursor]", *next)
		time.Sleep(500 * time.Millisecond)
	}
}
