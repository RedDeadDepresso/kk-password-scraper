package scrapers

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
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
	asSinPawchiveURL = "https://pawchive.pw/api/v1/patreon/user/207123644"

	// Patreon *user* id of the creator (from the pawchive URL).
	asSinUserID = "207123644"

	// Patreon campaign id (the filter[campaign_id] value seen in the browser's
	// network tab on the creator's posts page). Leave empty to auto-detect it
	// from the creator's user id via Patreon's API.
	asSinCampaignID = "16399962"
)

// Mega links: file (43 char key), folder (22 char key), plus the legacy formats.
var (
	asSinFileRe   = regexp.MustCompile(`https?://mega\.(?:nz|co\.nz)/(?:file/|#!)([A-Za-z0-9_-]{8})[#!]([A-Za-z0-9_-]{43})`)
	asSinFolderRe = regexp.MustCompile(`https?://mega\.(?:nz|co\.nz)/(?:folder/|#F!)([A-Za-z0-9_-]{8})[#!]([A-Za-z0-9_-]{22})`)

	// Intermediate link, e.g. https://mega.assin.top/d/xxxx
	asSinRedirectRe = regexp.MustCompile(`https?://mega\.assin\.top/d/[A-Za-z0-9_\-./%?=&#]+`)

	// "password：Dohi45QG" (full-width or ASCII colon, optional spaces)
	asSinPasswordRe = regexp.MustCompile(`(?i)^\s*password\s*[：:]\s*(.*?)\s*$`)

	campaignFromUserRe = regexp.MustCompile(`"type"\s*:\s*"campaign"\s*,\s*"id"\s*:\s*"?(\d+)|"id"\s*:\s*"?(\d+)"?\s*,\s*"type"\s*:\s*"campaign"`)
)

type asSinCandidate struct {
	ID       string
	Title    string
	IsFolder bool
	MegaID   string
	MegaKey  string
}

// AsSin scrapes pawchive + mega (directly or via mega.assin.top) + patreon
// and writes <dataDir>/as_sin.json.
func AsSin(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	outPath := filepath.Join(dataDir, "as_sin.json")
	client := &http.Client{Timeout: 30 * time.Second}

	existing, err := loadEntries(outPath)
	if err != nil {
		return err
	}
	stopID := ""
	if len(existing) > 0 && existing[0].ID != "" && existing[0].Password != "" {
		stopID = existing[0].ID
		fmt.Printf("Will stop at already-saved post: %s (%s)\n", existing[0].Title, stopID)
	}

	// 1. Pawchive posts (newest first) with their mega links.
	candidates, err := fetchAsSinPawchive(client, stopID)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("No new posts with mega links. Nothing to do.")
		return nil
	}

	// 2. Patreon passwords (post id -> caption of first media).
	needed := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		needed[c.ID] = true
	}
	campaignID := asSinCampaignID
	if campaignID == "" {
		campaignID, err = resolveAsSinCampaignID(client)
		if err != nil {
			return err
		}
		fmt.Printf("Detected campaign id: %s\n", campaignID)
	}
	passwords, err := fetchAsSinPasswords(client, campaignID, needed)
	if err != nil {
		return err
	}

	// 3. Resolve mega names only for posts that have a password.
	newEntries := make([]JuedgeEntry, 0, len(candidates))
	for _, c := range candidates {
		pw := passwords[c.ID]
		if pw == "" {
			fmt.Printf("Skipping (no password): %s\n", c.Title)
			continue
		}
		var name string
		if c.IsFolder {
			name, err = megaFolderName(client, c.MegaID, c.MegaKey)
			if err == nil {
				name += "E.zip" // folder name + E.zip, e.g. 哈尼娅(青春注脚)E.zip
			}
		} else {
			name, err = megaFilename(client, c.MegaID, c.MegaKey)
		}
		if err != nil {
			fmt.Printf("Skipping (mega lookup failed: %v): %s\n", err, c.Title)
			continue
		}
		newEntries = append(newEntries, JuedgeEntry{ID: c.ID, Title: c.Title, Filename: name, Password: pw})
		fmt.Printf("Added: %s -> %s\n", c.Title, name)
		time.Sleep(300 * time.Millisecond)
	}

	all := append(newEntries, existing...)
	if err := saveEntries(outPath, all); err != nil {
		return err
	}
	fmt.Printf("Saved %d new entries (%d total) to %s\n", len(newEntries), len(all), outPath)
	return nil
}

// ---------- Pawchive ----------

// findMegaLink looks for a mega file/folder link in text.
func findMegaLink(text string) (id, key string, folder, ok bool) {
	if m := asSinFolderRe.FindStringSubmatch(text); m != nil {
		return m[1], m[2], true, true
	}
	if m := asSinFileRe.FindStringSubmatch(text); m != nil {
		return m[1], m[2], false, true
	}
	return "", "", false, false
}

func fetchAsSinPawchive(client *http.Client, stopID string) ([]asSinCandidate, error) {
	var out []asSinCandidate
	for offset := 0; ; offset += pawchivePageSize {
		body, err := getBody(client, fmt.Sprintf("%s?o=%d", asSinPawchiveURL, offset))
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
			if stopID != "" && p.ID == stopID {
				fmt.Printf("Pawchive: hit saved post, stopping (%d new candidates).\n", len(out))
				return out, nil
			}
			content := html.UnescapeString(p.Content)
			id, key, folder, ok := findMegaLink(content)
			if !ok {
				// Maybe an intermediate mega.assin.top link instead.
				if link := asSinRedirectRe.FindString(content); link != "" {
					id, key, folder, ok = resolveAssinLink(client, link)
					time.Sleep(300 * time.Millisecond)
				}
			}
			if !ok {
				fmt.Printf("Skipping (no mega link): %s\n", p.Title)
				continue
			}
			out = append(out, asSinCandidate{ID: p.ID, Title: p.Title, IsFolder: folder, MegaID: id, MegaKey: key})
		}
		fmt.Printf("Pawchive: offset %d done (%d candidates so far)\n", offset, len(out))
		time.Sleep(500 * time.Millisecond)
	}
}

// resolveAssinLink requests a mega.assin.top/d/* page and pulls the mega link
// out of the returned html.
func resolveAssinLink(client *http.Client, link string) (id, key string, folder, ok bool) {
	body, err := getBody(client, link)
	if err != nil {
		fmt.Printf("assin link failed (%s): %v\n", link, err)
		return "", "", false, false
	}
	id, key, folder, ok = findMegaLink(html.UnescapeString(string(body)))
	if !ok {
		fmt.Printf("assin link had no mega link: %s\n", link)
	}
	return
}

// ---------- Patreon ----------

// resolveAsSinCampaignID looks up the campaign of the creator's user id.
func resolveAsSinCampaignID(client *http.Client) (string, error) {
	u := "https://www.patreon.com/api/user/" + asSinUserID + "?include=campaign&json-api-version=1.0"
	body, err := getPatreon(client, u)
	if err != nil {
		return "", fmt.Errorf("could not detect campaign id (set asSinCampaignID in as_sin.go): %w", err)
	}
	if m := campaignFromUserRe.FindSubmatch(body); m != nil {
		for _, g := range m[1:] {
			if len(g) > 0 {
				return string(g), nil
			}
		}
	}
	return "", fmt.Errorf("campaign id not found (set asSinCampaignID in as_sin.go)")
}

// parseAsSinPassword turns "password：Dohi45QG" into "Dohi45QG".
func parseAsSinPassword(caption string) string {
	caption = strings.TrimSpace(caption)
	if m := asSinPasswordRe.FindStringSubmatch(caption); m != nil {
		return m[1]
	}
	return caption
}

// teaser_text_json_string is a ProseMirror-style doc whose image node carries
// the password in its caption, e.g. {"type":"image","attrs":{"caption":"password:xxxx"}}.
type asSinNode struct {
	Type    string `json:"type"`
	Attrs   struct {
		Caption string `json:"caption"`
	} `json:"attrs"`
	Content []asSinNode `json:"content"`
}

type asSinPost struct {
	ID         string `json:"id"`
	Attributes struct {
		Teaser string `json:"teaser_text_json_string"`
	} `json:"attributes"`
}

type asSinResponse struct {
	Data []asSinPost `json:"data"`
	Meta struct {
		Pagination struct {
			Cursors struct {
				Next *string `json:"next"`
			} `json:"cursors"`
		} `json:"pagination"`
	} `json:"meta"`
}

// teaserCaption returns the first non-empty image caption in the teaser doc.
func teaserCaption(teaser string) string {
	if strings.TrimSpace(teaser) == "" {
		return ""
	}
	var root asSinNode
	if err := json.Unmarshal([]byte(teaser), &root); err != nil {
		return ""
	}
	var walk func(n asSinNode) string
	walk = func(n asSinNode) string {
		if n.Type == "image" && strings.TrimSpace(n.Attrs.Caption) != "" {
			return n.Attrs.Caption
		}
		for _, c := range n.Content {
			if s := walk(c); s != "" {
				return s
			}
		}
		return ""
	}
	return walk(root)
}

func fetchAsSinPasswords(client *http.Client, campaignID string, needed map[string]bool) (map[string]string, error) {
	passwords := make(map[string]string)
	seen := make(map[string]bool)
	q := patreonQuery()
	q.Set("filter[campaign_id]", campaignID)

	for page := 1; ; page++ {
		body, err := getPatreon(client, patreonPostsURL+"?"+q.Encode())
		if err != nil {
			return nil, fmt.Errorf("patreon page %d: %w", page, err)
		}
		var result asSinResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("patreon page %d: %w", page, err)
		}

		for _, post := range result.Data {
			if seen[post.ID] {
				continue
			}
			seen[post.ID] = true
			if pw := parseAsSinPassword(teaserCaption(post.Attributes.Teaser)); pw != "" {
				passwords[post.ID] = pw
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

// ---------- Mega folder ----------

// megaFolderName returns the name of the root node of a public mega folder.
func megaFolderName(client *http.Client, folderID, keyB64 string) (string, error) {
	folderKey, err := base64.RawURLEncoding.DecodeString(keyB64)
	if err != nil || len(folderKey) != 16 {
		return "", fmt.Errorf("invalid mega folder key")
	}
	block, err := aes.NewCipher(folderKey)
	if err != nil {
		return "", err
	}

	root, err := megaFolderRoot(client, folderID)
	if err != nil {
		return "", err
	}

	// k looks like "<handle>:<base64 key>[/<handle>:<key>...]"
	k := strings.SplitN(root.K, "/", 2)[0]
	if i := strings.LastIndex(k, ":"); i >= 0 {
		k = k[i+1:]
	}
	encKey, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k, "="))
	if err != nil || len(encKey) == 0 || len(encKey)%aes.BlockSize != 0 {
		return "", fmt.Errorf("invalid folder node key")
	}
	nodeKey := make([]byte, len(encKey))
	for i := 0; i < len(encKey); i += aes.BlockSize { // AES-ECB
		block.Decrypt(nodeKey[i:i+aes.BlockSize], encKey[i:i+aes.BlockSize])
	}
	if len(nodeKey) != 16 {
		return "", fmt.Errorf("unexpected folder node key length %d", len(nodeKey))
	}

	enc, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(root.A, "="))
	if err != nil || len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return "", fmt.Errorf("invalid encrypted attributes")
	}
	nb, err := aes.NewCipher(nodeKey)
	if err != nil {
		return "", err
	}
	plain := make([]byte, len(enc))
	cipher.NewCBCDecrypter(nb, make([]byte, aes.BlockSize)).CryptBlocks(plain, enc)
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

type megaNode struct {
	H string `json:"h"`
	P string `json:"p"`
	T int    `json:"t"`
	A string `json:"a"`
	K string `json:"k"`
}

func megaFolderRoot(client *http.Client, folderID string) (megaNode, error) {
	payload := `[{"a":"f","c":1,"r":1}]`
	endpoint := megaAPIURL + "?id=1&n=" + url.QueryEscape(folderID)
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(payload))
		if err != nil {
			return megaNode{}, err
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

		var code int
		if json.Unmarshal(body, &code) == nil {
			if code == -3 || code == -4 {
				lastErr = fmt.Errorf("mega error %d", code)
				continue
			}
			return megaNode{}, fmt.Errorf("mega error %d", code)
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(body, &arr); err != nil || len(arr) == 0 {
			return megaNode{}, fmt.Errorf("unexpected mega response")
		}
		if json.Unmarshal(arr[0], &code) == nil {
			if code == -3 || code == -4 {
				lastErr = fmt.Errorf("mega error %d", code)
				continue
			}
			return megaNode{}, fmt.Errorf("mega error %d (-9 = link removed)", code)
		}
		var res struct {
			F []megaNode `json:"f"`
		}
		if err := json.Unmarshal(arr[0], &res); err != nil || len(res.F) == 0 {
			return megaNode{}, fmt.Errorf("mega folder has no nodes")
		}
		for _, n := range res.F {
			if n.T == 1 && n.H == folderID {
				return n, nil
			}
		}
		for _, n := range res.F {
			if n.T == 1 {
				return n, nil
			}
		}
		return megaNode{}, fmt.Errorf("mega folder root not found")
	}
	return megaNode{}, lastErr
}