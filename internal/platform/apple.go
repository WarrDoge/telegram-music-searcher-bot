package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"

	httpclient "telegram-music-bot/internal/http"
	"telegram-music-bot/internal/util"
)

// AppleMusic implements the Platform interface for Apple Music
type AppleMusic struct {
	httpClient *httpclient.Client
	cache      Cache
	config     *AppleMusicConfig
	logger     *zap.Logger
}

// AppleMusicConfig holds Apple Music-specific configuration
type AppleMusicConfig struct {
	CacheTTL time.Duration
	Debug    bool
}

// NewAppleMusic creates a new Apple Music platform implementation
func NewAppleMusic(
	httpClient *httpclient.Client,
	cache Cache,
	config *AppleMusicConfig,
	logger *zap.Logger,
) *AppleMusic {
	return &AppleMusic{
		httpClient: httpClient,
		cache:      cache,
		config:     config,
		logger:     logger,
	}
}

// Platform returns the platform name
func (a *AppleMusic) Platform() string {
	return "Apple Music"
}

// ExtractSongInfo extracts song information from an Apple Music URL
func (a *AppleMusic) ExtractSongInfo(ctx context.Context, appleURL string) (*SongInfo, error) {
	if v, ok := a.cache.Get("info:" + appleURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si, nil
		}
	}

	resp, err := a.httpClient.Fetch(ctx, appleURL)
	if err != nil {
		if a.config.Debug {
			a.logger.Debug("Apple Music fetch error", zap.Error(err))
		}
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		if a.config.Debug {
			a.logger.Debug("Apple Music parse error", zap.Error(err))
		}
		return nil, err
	}

	title, artist := extractAppleLD(doc)

	if title == "" {
		title = doc.Find("meta[property='og:title']").AttrOr("content", "")
	}

	// Parse og:title if needed
	if title != "" && artist == "" {
		cleanOgTitle := title
		for fancy := range util.GetFancyQuotes() {
			cleanOgTitle = strings.ReplaceAll(cleanOgTitle, fancy, "")
		}

		// Pattern 1: "Title by Artist on Apple Music"
		if strings.Contains(cleanOgTitle, " by ") && strings.Contains(cleanOgTitle, " on Apple Music") {
			parts := strings.SplitN(cleanOgTitle, " by ", 2)
			title = strings.TrimSpace(parts[0])
			artist = strings.TrimSpace(strings.ReplaceAll(parts[1], " on Apple Music", ""))
		} else if strings.Contains(cleanOgTitle, ",") && util.GetAppleMusicSuffixRegex().MatchString(cleanOgTitle) {
			parts := strings.SplitN(cleanOgTitle, ",", 2)
			if len(parts) == 2 {
				title = strings.TrimSpace(parts[0])
				artist = strings.TrimSpace(util.GetAppleMusicSuffixRegex().ReplaceAllString(parts[1], ""))
			}
		}
	}

	// Clean up
	title, artist = util.CleanTitleArtist(title, artist)

	// Handle "Title by Artist" format
	if strings.Contains(title, " by ") {
		byIndex := strings.Index(title, " by ")
		titlePart := strings.TrimSpace(title[:byIndex])
		artistPart := strings.TrimSpace(title[byIndex+4:])

		if artist == "" || strings.EqualFold(artist, artistPart) {
			title = titlePart
			if artist == "" {
				artist = artistPart
			}
		}
	}

	title = strings.TrimSpace(strings.NewReplacer(
		" - Single", "",
		" - EP", "",
		" - Album", "",
		" on Apple Music", "",
		" в Apple Music", "",
	).Replace(title))

	if title == "" {
		return nil, fmt.Errorf("could not extract title")
	}

	si := &SongInfo{
		Title:       title,
		Artist:      artist,
		Platform:    "Apple Music",
		OriginalURL: appleURL,
	}
	if b, err := json.Marshal(si); err == nil {
		a.cache.Set("info:"+appleURL, string(b), a.config.CacheTTL)
	}
	return si, nil
}

// Search searches for a song on Apple Music
func (a *AppleMusic) Search(ctx context.Context, info *SongInfo) (string, error) {
	key := util.NormalizeQuery(info.Artist, info.Title)
	if v, ok := a.cache.Get("am:" + key); ok {
		return v, nil
	}

	query := key
	searchURL := fmt.Sprintf("https://music.apple.com/search?term=%s", url.QueryEscape(query))

	// Try iTunes Search API
	country := "US"
	apiURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&media=music&entity=song&limit=10&country=%s",
		url.QueryEscape(query), country)

	resp, err := a.httpClient.Fetch(ctx, apiURL)
	if err == nil {
		defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

		var data struct {
			Results []struct {
				TrackViewURL string `json:"trackViewUrl"`
				TrackName    string `json:"trackName"`
				ArtistName   string `json:"artistName"`
			} `json:"results"`
		}

		if json.NewDecoder(resp.Body).Decode(&data) == nil && len(data.Results) > 0 {
			wantT := util.NormalizeForMatch(info.Title)
			wantA := util.NormalizeForMatch(info.Artist)

			bestScore := -999
			bestURL := ""

			for _, result := range data.Results {
				if result.TrackViewURL == "" {
					continue
				}

				t := util.NormalizeForMatch(result.TrackName)
				ar := util.NormalizeForMatch(result.ArtistName)

				score := 0
				if wantT != "" && strings.Contains(t, wantT) {
					score += 3
				}
				if wantA != "" && strings.Contains(ar, wantA) {
					score += 3
				}

				if score > bestScore {
					bestScore = score
					bestURL = strings.Replace(result.TrackViewURL, "itunes.apple.com", "music.apple.com", 1)
				}
			}

			if bestScore >= 3 && bestURL != "" {
				a.cache.Set("am:"+key, bestURL, a.config.CacheTTL)
				return bestURL, nil
			}
		}
	}

	return searchURL, nil
}

// extractAppleLD extracts Apple Music JSON-LD data
func extractAppleLD(doc *goquery.Document) (title, artist string) {
	doc.Find("script[type='application/ld+json']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			return true
		}

		var any interface{}
		if json.Unmarshal([]byte(raw), &any) != nil {
			return true
		}

		var objects []map[string]interface{}
		switch v := any.(type) {
		case map[string]interface{}:
			objects = append(objects, v)
		case []interface{}:
			for _, it := range v {
				if m, ok := it.(map[string]interface{}); ok {
					objects = append(objects, m)
				}
			}
		default:
			return true
		}

		for _, obj := range objects {
			typeStr, _ := obj["@type"].(string)
			if typeStr == "MusicRecording" || typeStr == "MusicAlbum" ||
				typeStr == "CreativeWork" || typeStr == "MusicComposition" {
				if n, ok := obj["name"].(string); ok && n != "" {
					title = n
				}

				// Check for byArtist at current level
				if by, ok := obj["byArtist"]; ok {
					artist = extractArtistName(by)
				}

				// Check nested audio object for MusicRecording
				if artist == "" {
					if audioObj, ok := obj["audio"].(map[string]interface{}); ok {
						if by, ok := audioObj["byArtist"]; ok {
							artist = extractArtistName(by)
						}
						if title == "" {
							if n, ok := audioObj["name"].(string); ok && n != "" {
								title = n
							}
						}
					}
				}

				if title != "" && artist != "" {
					return false
				}
			}
		}
		return true
	})
	return
}

// extractArtistName extracts artist name from various JSON-LD formats
func extractArtistName(by interface{}) string {
	switch v := by.(type) {
	case string:
		return v
	case map[string]interface{}:
		if name, ok := v["name"].(string); ok {
			return name
		}
	case []interface{}:
		if len(v) > 0 {
			if m, ok := v[0].(map[string]interface{}); ok {
				if name, ok := m["name"].(string); ok {
					return name
				}
			}
		}
	}
	return ""
}
