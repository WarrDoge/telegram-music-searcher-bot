package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"

	httpclient "telegram-music-bot/internal/http"
	"telegram-music-bot/internal/util"
)

// YouTube implements the Platform interface for YouTube/YouTube Music
type YouTube struct {
	httpClient *httpclient.Client
	cache      Cache
	config     *YouTubeConfig
	logger     *zap.Logger
}

// YouTubeConfig holds YouTube-specific configuration
type YouTubeConfig struct {
	CacheTTL time.Duration
	Debug    bool
}

// NewYouTube creates a new YouTube platform implementation
func NewYouTube(
	httpClient *httpclient.Client,
	cache Cache,
	config *YouTubeConfig,
	logger *zap.Logger,
) *YouTube {
	return &YouTube{
		httpClient: httpClient,
		cache:      cache,
		config:     config,
		logger:     logger,
	}
}

// Platform returns the platform name
func (y *YouTube) Platform() string {
	return "YouTube Music"
}

// ExtractSongInfo extracts song information from a YouTube URL
func (y *YouTube) ExtractSongInfo(ctx context.Context, youtubeURL string) (*SongInfo, error) {
	platform := "YouTube Music"
	if strings.Contains(youtubeURL, "youtube.com") && !strings.Contains(youtubeURL, "music.youtube.com") {
		platform = "YouTube"
	}

	if v, ok := y.cache.Get("info:" + youtubeURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si, nil
		}
	}

	// Try OEmbed first
	oembedURL := fmt.Sprintf("https://www.youtube.com/oembed?format=json&url=%s", url.QueryEscape(youtubeURL))
	resp, err := y.httpClient.Fetch(ctx, oembedURL)
	if err == nil {
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

		var data struct {
			Title      string `json:"title"`
			AuthorName string `json:"author_name"`
		}

		if json.NewDecoder(resp.Body).Decode(&data) == nil && data.Title != "" {
			if y.config.Debug {
				y.logger.Debug("YouTube OEmbed response",
					zap.String("platform", platform),
					zap.String("title", data.Title),
					zap.String("author", data.AuthorName))
			}

			title, artist := util.CleanYouTubeInfo(data.Title, data.AuthorName)

			if y.config.Debug {
				y.logger.Debug("Extracted from OEmbed",
					zap.String("platform", platform),
					zap.String("title", title),
					zap.String("artist", artist))
			}

			si := &SongInfo{
				Title:       title,
				Artist:      artist,
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
			if b, err := json.Marshal(si); err == nil {
				y.cache.Set("info:"+youtubeURL, string(b), y.config.CacheTTL)
			}
			return si, nil
		}
	}

	// Fallback to scraping
	resp, err = y.httpClient.Fetch(ctx, youtubeURL)
	if err != nil {
		if y.config.Debug {
			y.logger.Debug("YouTube fetch error", zap.String("platform", platform), zap.Error(err))
		}
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Try to find ytInitialPlayerResponse
	re := regexp.MustCompile(`ytInitialPlayerResponse\s*=\s*(\{.+?\});`)
	matches := re.FindSubmatch(body)
	if len(matches) > 1 {
		var data struct {
			VideoDetails struct {
				Title  string `json:"title"`
				Author string `json:"author"`
			} `json:"videoDetails"`
		}

		if json.Unmarshal(matches[1], &data) == nil && data.VideoDetails.Title != "" {
			title, artist := util.CleanYouTubeInfo(data.VideoDetails.Title, data.VideoDetails.Author)

			si := &SongInfo{
				Title:       title,
				Artist:      artist,
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
			if b, err := json.Marshal(si); err == nil {
				y.cache.Set("info:"+youtubeURL, string(b), y.config.CacheTTL)
			}
			return si, nil
		}
	}

	// Fallback to og:title
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	title := doc.Find("meta[property='og:title']").AttrOr("content", "")
	title = strings.ReplaceAll(title, " - YouTube Music", "")
	title = strings.ReplaceAll(title, " - YouTube", "")

	if strings.Contains(title, " - ") {
		parts := strings.SplitN(title, " - ", 2)
		si := &SongInfo{
			Title:       strings.TrimSpace(parts[1]),
			Artist:      strings.TrimSpace(parts[0]),
			Platform:    platform,
			OriginalURL: youtubeURL,
		}
		if b, err := json.Marshal(si); err == nil {
			y.cache.Set("info:"+youtubeURL, string(b), y.config.CacheTTL)
		}
		return si, nil
	}

	if title != "" {
		si := &SongInfo{
			Title:       strings.TrimSpace(title),
			Artist:      "",
			Platform:    platform,
			OriginalURL: youtubeURL,
		}
		if b, err := json.Marshal(si); err == nil {
			y.cache.Set("info:"+youtubeURL, string(b), y.config.CacheTTL)
		}
		return si, nil
	}

	return nil, fmt.Errorf("could not extract YouTube info")
}

// Search searches for a song on YouTube
func (y *YouTube) Search(ctx context.Context, info *SongInfo) (string, error) {
	key := util.NormalizeQuery(info.Artist, info.Title)
	if v, ok := y.cache.Get("yt:" + key); ok {
		return v, nil
	}

	query := key

	// Try YouTube Music search
	ytMusicURL := fmt.Sprintf("https://music.youtube.com/search?q=%s", url.QueryEscape(query))
	if videoID, err := y.searchViaPage(ctx, ytMusicURL, info); err == nil && videoID != "" {
		u := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
		y.cache.Set("yt:"+key, u, y.config.CacheTTL)
		return u, nil
	}

	// Fallback to regular YouTube
	ytURL := fmt.Sprintf("https://www.youtube.com/results?search_query=%s", url.QueryEscape(query))
	if videoID, err := y.searchViaPage(ctx, ytURL, info); err == nil && videoID != "" {
		u := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
		y.cache.Set("yt:"+key, u, y.config.CacheTTL)
		return u, nil
	}

	return ytMusicURL, nil
}

// searchViaPage searches YouTube by parsing the search results page
func (y *YouTube) searchViaPage(ctx context.Context, searchURL string, info *SongInfo) (string, error) {
	resp, err := y.httpClient.Fetch(ctx, searchURL)
	if err != nil {
		if y.config.Debug {
			y.logger.Debug("YouTube search fetch error", zap.Error(err))
		}
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Find ytInitialData
	re := regexp.MustCompile(`ytInitialData\s*=\s*(\{.+?\});`)
	matches := re.FindSubmatch(body)
	if len(matches) < 2 {
		return "", fmt.Errorf("no ytInitialData found")
	}

	var data interface{}
	if err := json.Unmarshal(matches[1], &data); err != nil {
		if y.config.Debug {
			y.logger.Debug("YouTube JSON parse error", zap.Error(err))
		}
		return "", err
	}

	candidates := collectYouTubeCandidates(data)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no candidates found")
	}

	// Score candidates
	bestScore := -999
	bestID := ""

	for _, c := range candidates {
		score := scoreYouTubeCandidate(c, info.Artist, info.Title)
		if score > bestScore {
			bestScore = score
			bestID = c.ID
		}
	}

	if bestID != "" {
		return bestID, nil
	}

	if len(candidates) > 0 {
		return candidates[0].ID, nil
	}

	return "", fmt.Errorf("no suitable candidate found")
}

// collectYouTubeCandidates collects video candidates from YouTube search data
func collectYouTubeCandidates(v interface{}) []YouTubeCandidate {
	var result []YouTubeCandidate
	collectYouTubeCandidatesRecursive(v, &result)
	return result
}

// collectYouTubeCandidatesRecursive recursively collects video candidates
func collectYouTubeCandidatesRecursive(v interface{}, out *[]YouTubeCandidate) {
	switch x := v.(type) {
	case map[string]interface{}:
		var id, title, channel string
		if vr, ok := x["videoId"].(string); ok {
			id = vr
		}

		if t, ok := x["title"].(map[string]interface{}); ok {
			if runs, ok := t["runs"].([]interface{}); ok && len(runs) > 0 {
				if m, ok := runs[0].(map[string]interface{}); ok {
					if tx, ok := m["text"].(string); ok {
						title = tx
					}
				}
			} else if st, ok := t["simpleText"].(string); ok {
				title = st
			}
		}

		if lb, ok := x["longBylineText"].(map[string]interface{}); ok {
			if runs, ok := lb["runs"].([]interface{}); ok && len(runs) > 0 {
				if m, ok := runs[0].(map[string]interface{}); ok {
					if tx, ok := m["text"].(string); ok {
						channel = tx
					}
				}
			}
		} else if ow, ok := x["ownerText"].(map[string]interface{}); ok {
			if runs, ok := ow["runs"].([]interface{}); ok && len(runs) > 0 {
				if m, ok := runs[0].(map[string]interface{}); ok {
					if tx, ok := m["text"].(string); ok {
						channel = tx
					}
				}
			}
		}

		if id != "" {
			*out = append(*out, YouTubeCandidate{ID: id, Title: title, Channel: channel})
		}

		for _, vv := range x {
			collectYouTubeCandidatesRecursive(vv, out)
		}
	case []interface{}:
		for _, vv := range x {
			collectYouTubeCandidatesRecursive(vv, out)
		}
	}
}

// scoreYouTubeCandidate scores a YouTube candidate based on matching criteria
func scoreYouTubeCandidate(c YouTubeCandidate, wantArtist, wantTitle string) int {
	score := 0
	na := util.NormalizeForMatch(wantArtist)
	nt := util.NormalizeForMatch(wantTitle)
	ct := util.NormalizeForMatch(c.Title)
	cc := util.NormalizeForMatch(c.Channel)

	if strings.Contains(ct, nt) {
		score += 4
	}
	if strings.Contains(ct, na) {
		score += 3
	}
	if strings.Contains(cc, na) {
		score += 2
	}
	if strings.HasSuffix(cc, " - topic") {
		score += 2
	}

	bad := []string{"cover", "lyrics", "lyric", "live", "karaoke", "sped up", "speed up", "nightcore", "8d", "slowed"}
	for _, b := range bad {
		if strings.Contains(ct, b) {
			score -= 2
		}
	}
	if strings.Contains(ct, "remix") && !strings.Contains(nt, "remix") {
		score -= 1
	}

	return score
}

// ToYouTubeMusicURL converts a regular YouTube URL to YouTube Music URL
func ToYouTubeMusicURL(rawURL string) string {
	if id := youtubeVideoIDFromURL(rawURL); id != "" {
		return fmt.Sprintf("https://music.youtube.com/watch?v=%s", id)
	}
	return ""
}

// youtubeVideoIDFromURL extracts the video ID from a YouTube URL
func youtubeVideoIDFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	switch strings.ToLower(u.Host) {
	case "youtu.be":
		return strings.Trim(u.Path, "/")
	default:
		return u.Query().Get("v")
	}
}
