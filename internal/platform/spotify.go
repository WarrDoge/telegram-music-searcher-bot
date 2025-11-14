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

var (
	reSpotifyTrack = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?track/([A-Za-z0-9]+)`)
	reSpotifyAlbum = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?album/([A-Za-z0-9]+)`)
	reSpotifyURI   = regexp.MustCompile(`spotify:track:([A-Za-z0-9]+)`)
	reNextData     = regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">(.+?)</script>`)
)

// SpotifyConfig holds Spotify-specific configuration
type SpotifyConfig struct {
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	TextMirrorURL    string
	Debug            bool
}

// Spotify implements the Platform interface for Spotify
type Spotify struct {
	httpClient     *httpclient.Client
	circuitBreaker *httpclient.CircuitBreaker
	cache          Cache
	negativeCache  Cache
	config         *SpotifyConfig
	logger         *zap.Logger
}

// Cache interface for caching song info
type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string, ttl time.Duration)
}

// NewSpotify creates a new Spotify platform implementation
func NewSpotify(
	httpClient *httpclient.Client,
	circuitBreaker *httpclient.CircuitBreaker,
	cache Cache,
	negativeCache Cache,
	config *SpotifyConfig,
	logger *zap.Logger,
) *Spotify {
	return &Spotify{
		httpClient:     httpClient,
		circuitBreaker: circuitBreaker,
		cache:          cache,
		negativeCache:  negativeCache,
		config:         config,
		logger:         logger,
	}
}

// Platform returns the platform name
func (s *Spotify) Platform() string {
	return "Spotify"
}

// ExtractSongInfo extracts song information from a Spotify URL
func (s *Spotify) ExtractSongInfo(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	// Check cache first
	if v, ok := s.cache.Get("info:" + spotifyURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si, nil
		}
	}

	// Check negative cache
	if _, ok := s.negativeCache.Get("info:" + spotifyURL); ok {
		if s.config.Debug {
			s.logger.Debug("Spotify URL in negative cache, skipping", zap.String("url", spotifyURL))
		}
		return nil, fmt.Errorf("URL in negative cache")
	}

	if !reSpotifyTrack.MatchString(spotifyURL) {
		return nil, fmt.Errorf("not a Spotify track URL")
	}

	// Use circuit breaker
	result, err := s.circuitBreaker.Execute(func() (interface{}, error) {
		return s.extractWithFallbacks(ctx, spotifyURL)
	})

	if err != nil {
		s.logger.Error("Spotify circuit breaker error", zap.Error(err))
		s.negativeCache.Set("info:"+spotifyURL, "failed", s.config.NegativeCacheTTL)
		return nil, err
	}

	if result == nil {
		s.negativeCache.Set("info:"+spotifyURL, "failed", s.config.NegativeCacheTTL)
		return nil, fmt.Errorf("no result from extraction")
	}

	si := result.(*SongInfo)
	// Cache successful result
	if b, err := json.Marshal(si); err == nil {
		s.cache.Set("info:"+spotifyURL, string(b), s.config.CacheTTL)
	}
	return si, nil
}

// extractWithFallbacks tries all 6 extraction methods for Spotify
func (s *Spotify) extractWithFallbacks(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	// Layer 1: Try current OEmbed API
	if info, err := s.tryOEmbed(ctx, spotifyURL); err == nil && info != nil {
		return info, nil
	}

	// Layer 2: Try legacy OEmbed endpoint
	if info, err := s.tryLegacyOEmbed(ctx, spotifyURL); err == nil && info != nil {
		return info, nil
	}

	// Layer 3: Try __NEXT_DATA__ extraction
	if info, err := s.extractFromEmbed(ctx, spotifyURL); err == nil && info != nil && info.Artist != "" {
		return info, nil
	}

	// Layer 4: Try JSON-LD structured data
	if info, err := s.tryJSONLD(ctx, spotifyURL); err == nil && info != nil {
		return info, nil
	}

	// Layer 5: Try OpenGraph meta tags
	if info, err := s.fallbackScrape(ctx, spotifyURL); err == nil && info != nil {
		return info, nil
	}

	// Layer 6: Try text mirror fallback
	if info, err := s.tryTextMirror(ctx, spotifyURL); err == nil && info != nil {
		return info, nil
	}

	return nil, fmt.Errorf("all Spotify extraction methods failed")
}

// tryOEmbed attempts standard OEmbed API (Layer 1)
func (s *Spotify) tryOEmbed(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	oembedURL := fmt.Sprintf("https://open.spotify.com/oembed?url=%s", url.QueryEscape(spotifyURL))
	resp, err := s.httpClient.Fetch(ctx, oembedURL)
	if err != nil {
		if s.config.Debug {
			s.logger.Debug("Spotify OEmbed error", zap.Error(err))
		}
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		if s.config.Debug {
			s.logger.Debug("Spotify OEmbed decode error", zap.Error(err))
		}
		return nil, err
	}

	if s.config.Debug {
		s.logger.Debug("Spotify OEmbed response",
			zap.String("title", oembed.Title),
			zap.String("artist", oembed.AuthorName),
			zap.String("desc", oembed.Description))
	}

	title := strings.TrimSpace(oembed.Title)
	artist := strings.TrimSpace(oembed.AuthorName)
	title = strings.TrimSuffix(title, " | Spotify")

	// Parse description for missing fields
	if desc := strings.TrimSpace(oembed.Description); desc != "" && (title == "" || artist == "") {
		parts := util.GetMidDotSepRegex().Split(strings.ReplaceAll(desc, "\u00A0", " "), -1)

		if artist == "" && len(parts) >= 2 {
			p0, p1 := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch {
			case title != "" && strings.EqualFold(p0, title):
				artist = p1
			case title != "" && strings.EqualFold(p1, title):
				artist = p0
			case util.IsAlbumish(p0) && !util.IsAlbumish(p1):
				artist = p1
			case util.IsAlbumish(p1) && !util.IsAlbumish(p0):
				artist = p0
			default:
				artist = p1
			}
		}

		if title == "" && len(parts) >= 2 {
			p0, p1 := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch {
			case artist != "" && strings.EqualFold(p0, artist):
				title = p1
			case artist != "" && strings.EqualFold(p1, artist):
				title = p0
			case !util.IsAlbumish(p0) && util.IsAlbumish(p1):
				title = p0
			case !util.IsAlbumish(p1) && util.IsAlbumish(p0):
				title = p1
			default:
				title = p0
			}
		}
	}

	// Try to parse from title if artist missing
	if artist == "" && strings.Contains(strings.ToLower(title), " by ") {
		if idx := strings.Index(strings.ToLower(title), " by "); idx != -1 {
			artist = title[idx+4:]
			title = title[:idx]
		}
	}

	// Try embed page extraction if artist missing
	if artist == "" {
		if s.config.Debug {
			s.logger.Debug("Artist missing from OEmbed, trying embed page")
		}
		if embedInfo, err := s.extractFromEmbed(ctx, spotifyURL); err == nil && embedInfo != nil && embedInfo.Artist != "" {
			artist = embedInfo.Artist
			if title == "" && embedInfo.Title != "" {
				title = embedInfo.Title
			}
		}
	}

	// Fallback to scraping if parsing looks wrong
	if artist == "" || util.IsAlbumish(artist) ||
		(util.LooksLikeArtistList(title) && !util.LooksLikeArtistList(artist)) ||
		strings.EqualFold(title, artist) {
		if s.config.Debug {
			s.logger.Debug("Artist missing or invalid, falling back to scrape", zap.String("artist", artist))
		}
		if scraped, err := s.fallbackScrape(ctx, spotifyURL); err == nil && scraped != nil {
			if scraped.Title != "" && scraped.Artist != "" {
				if b, err := json.Marshal(scraped); err == nil {
					s.cache.Set("info:"+spotifyURL, string(b), s.config.CacheTTL)
				}
				return scraped, nil
			}
			if artist == "" && scraped.Artist != "" {
				artist = scraped.Artist
			}
			if title == "" && scraped.Title != "" {
				title = scraped.Title
			}
		}
	}

	if title == "" {
		if scraped, err := s.fallbackScrape(ctx, spotifyURL); err == nil && scraped != nil {
			if b, err := json.Marshal(scraped); err == nil {
				s.cache.Set("info:"+spotifyURL, string(b), s.config.CacheTTL)
			}
			return scraped, nil
		}
	}

	return &SongInfo{
		Title:       title,
		Artist:      artist,
		Platform:    "Spotify",
		OriginalURL: spotifyURL,
	}, nil
}

// tryLegacyOEmbed attempts legacy OEmbed endpoint (Layer 2)
func (s *Spotify) tryLegacyOEmbed(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	legacyURL := fmt.Sprintf("https://embed.spotify.com/oembed/?url=%s", url.QueryEscape(spotifyURL))
	resp, err := s.httpClient.Fetch(ctx, legacyURL)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		return nil, err
	}

	title := strings.TrimSpace(oembed.Title)
	artist := strings.TrimSpace(oembed.AuthorName)

	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	return nil, fmt.Errorf("incomplete data from legacy oEmbed")
}

// tryJSONLD attempts JSON-LD structured data extraction (Layer 4)
func (s *Spotify) tryJSONLD(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	resp, err := s.httpClient.Fetch(ctx, spotifyURL)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	var title, artist string
	doc.Find("script[type='application/ld+json']").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw := strings.TrimSpace(s.Text())
		if raw == "" {
			return true
		}

		var data map[string]interface{}
		if json.Unmarshal([]byte(raw), &data) != nil {
			return true
		}

		typeStr, _ := data["@type"].(string)
		if typeStr == "MusicRecording" || typeStr == "MusicAlbum" {
			if n, ok := data["name"].(string); ok {
				title = n
			}

			if by, ok := data["byArtist"].(map[string]interface{}); ok {
				if name, ok := by["name"].(string); ok {
					artist = name
				}
			}

			if title != "" && artist != "" {
				return false
			}
		}

		return true
	})

	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	return nil, fmt.Errorf("no JSON-LD data found")
}

// tryTextMirror attempts text mirror fallback (Layer 6)
func (s *Spotify) tryTextMirror(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	if s.config.TextMirrorURL == "" {
		return nil, fmt.Errorf("text mirror URL not configured")
	}

	mirrorURL := s.config.TextMirrorURL + spotifyURL
	if s.config.Debug {
		s.logger.Debug("Fetching via text mirror",
			zap.String("original_url", spotifyURL),
			zap.String("mirror_url", mirrorURL))
	}

	resp, err := s.httpClient.Fetch(ctx, mirrorURL)
	if err != nil {
		if s.config.Debug {
			s.logger.Debug("Text mirror fetch failed", zap.Error(err))
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
		return nil, err
	}

	ogTitle := doc.Find("meta[property='og:title']").AttrOr("content", "")
	ogDesc := doc.Find("meta[property='og:description']").AttrOr("content", "")

	title, artist := util.SplitFromOgTitle(ogTitle, ogDesc)
	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	return nil, fmt.Errorf("could not extract from text mirror")
}

// extractFromEmbed extracts from Spotify embed page - __NEXT_DATA__ extraction (Layer 3)
func (s *Spotify) extractFromEmbed(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	match := reSpotifyTrack.FindStringSubmatch(spotifyURL)
	if len(match) < 2 {
		return nil, fmt.Errorf("no track ID found in URL")
	}

	trackID := match[1]
	embedURL := fmt.Sprintf("https://open.spotify.com/embed/track/%s", trackID)

	resp, err := s.httpClient.Fetch(ctx, embedURL)
	if err != nil {
		if s.config.Debug {
			s.logger.Debug("Spotify Embed fetch error", zap.Error(err))
		}
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Find __NEXT_DATA__ JSON
	matches := reNextData.FindSubmatch(body)
	if len(matches) < 2 {
		return nil, fmt.Errorf("no __NEXT_DATA__ found")
	}

	var data struct {
		Props struct {
			PageProps struct {
				State struct {
					Data struct {
						Entity struct {
							Name    string `json:"name"`
							Title   string `json:"title"`
							Artists []struct {
								Name string `json:"name"`
							} `json:"artists"`
						} `json:"entity"`
					} `json:"data"`
				} `json:"state"`
			} `json:"pageProps"`
		} `json:"props"`
	}

	if err := json.Unmarshal(matches[1], &data); err != nil {
		if s.config.Debug {
			s.logger.Debug("Spotify Embed JSON parse error", zap.Error(err))
		}
		return nil, err
	}

	entity := data.Props.PageProps.State.Data.Entity
	title := entity.Name
	if title == "" {
		title = entity.Title
	}

	var artist string
	if len(entity.Artists) > 0 {
		artist = entity.Artists[0].Name
	}

	if s.config.Debug {
		s.logger.Debug("Spotify Embed extracted", zap.String("title", title), zap.String("artist", artist))
	}

	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	return nil, fmt.Errorf("incomplete data from embed")
}

// fallbackScrape attempts to scrape OpenGraph tags (Layer 5)
func (s *Spotify) fallbackScrape(ctx context.Context, spotifyURL string) (*SongInfo, error) {
	resp, err := s.httpClient.Fetch(ctx, spotifyURL)
	if err != nil {
		if s.config.Debug {
			s.logger.Debug("Spotify scrape fetch error", zap.Error(err))
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
		if s.config.Debug {
			s.logger.Debug("Spotify scrape parse error", zap.Error(err))
		}
		return nil, err
	}

	ogTitle := doc.Find("meta[property='og:title']").AttrOr("content", "")
	ogDesc := doc.Find("meta[property='og:description']").AttrOr("content", "")

	if s.config.Debug {
		s.logger.Debug("Spotify scrape",
			zap.String("og:title", ogTitle),
			zap.String("og:description", ogDesc))
	}

	// Try robust split from og:title
	ti, ar := util.SplitFromOgTitle(ogTitle, ogDesc)
	if s.config.Debug {
		s.logger.Debug("splitFromOgTitle result", zap.String("title", ti), zap.String("artist", ar))
	}
	if ti != "" && ar != "" {
		return &SongInfo{Title: ti, Artist: ar, Platform: "Spotify", OriginalURL: spotifyURL}, nil
	}

	// Fallback to simpler parsing
	cleanTitle := strings.TrimSuffix(ogTitle, " | Spotify")
	cleanTitle = strings.ReplaceAll(cleanTitle, " — ", " - ")
	cleanTitle = strings.ReplaceAll(cleanTitle, " – ", " - ")

	if idx := strings.Index(strings.ToLower(cleanTitle), " by "); idx != -1 {
		return &SongInfo{
			Title:       strings.TrimSpace(cleanTitle[:idx]),
			Artist:      strings.TrimSpace(cleanTitle[idx+4:]),
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	if strings.Contains(cleanTitle, " - ") {
		parts := strings.SplitN(cleanTitle, " - ", 2)
		return &SongInfo{
			Title:       strings.TrimSpace(parts[0]),
			Artist:      strings.TrimSpace(parts[1]),
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}, nil
	}

	// Last resort: salvage artist from description
	if ogDesc != "" {
		parts := util.GetMidDotSepRegex().Split(strings.ReplaceAll(ogDesc, "\u00A0", " "), -1)
		if len(parts) > 0 {
			artistGuess := strings.TrimSpace(parts[0])
			if artistGuess != "" {
				return &SongInfo{
					Title:       cleanTitle,
					Artist:      artistGuess,
					Platform:    "Spotify",
					OriginalURL: spotifyURL,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("could not extract from scraping")
}

// Search searches for a song on Spotify
func (s *Spotify) Search(ctx context.Context, info *SongInfo) (string, error) {
	key := util.NormalizeQuery(info.Artist, info.Title)
	if v, ok := s.cache.Get("sp:" + key); ok {
		return v, nil
	}

	query := key
	searchURL := fmt.Sprintf("https://open.spotify.com/search/%s", url.QueryEscape(query))

	// Try with standard HTTP first
	if resp, err := s.httpClient.Fetch(ctx, searchURL); err == nil {
		func() {
			defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err == nil {
				// Look for spotify:track: URI
				if m := reSpotifyURI.FindSubmatch(body); len(m) == 2 {
					u := fmt.Sprintf("https://open.spotify.com/track/%s", m[1])
					s.cache.Set("sp:"+key, u, s.config.CacheTTL)
					return
				}

				// Look for track links
				if m := reSpotifyTrack.FindSubmatch(body); len(m) == 2 {
					u := canonicalSpotifyTrack(string(body))
					if u != "" {
						s.cache.Set("sp:"+key, u, s.config.CacheTTL)
						return
					}
				}
			}
		}()
	}

	// DuckDuckGo fallback
	if u, err := s.searchViaDDG(ctx, info.Artist, info.Title); err == nil && u != "" {
		s.cache.Set("sp:"+key, u, s.config.CacheTTL)
		return u, nil
	}

	return searchURL, nil
}

// searchViaDDG searches for a Spotify track via DuckDuckGo
func (s *Spotify) searchViaDDG(ctx context.Context, artist, title string) (string, error) {
	t := util.NormalizeForMatch(title)
	a := util.NormalizeForMatch(artist)

	// Pass 1: Exact (quoted) search
	parts := []string{"site:open.spotify.com/track"}
	if t != "" {
		parts = append(parts, fmt.Sprintf(`"%s"`, t))
	}
	if a != "" {
		parts = append(parts, fmt.Sprintf(`"%s"`, a))
	}

	q := strings.Join(parts, " ")
	ddgURL := fmt.Sprintf("https://duckduckgo.com/html/?q=%s", url.QueryEscape(q))

	if result, err := s.searchDDGPage(ctx, ddgURL); err == nil && result != "" {
		return result, nil
	}

	// Pass 2: Loose search
	if t == "" && a == "" {
		return "", fmt.Errorf("no search terms")
	}

	q2 := fmt.Sprintf("site:open.spotify.com/track %s %s", t, a)
	ddgURL2 := fmt.Sprintf("https://duckduckgo.com/html/?q=%s", url.QueryEscape(strings.TrimSpace(q2)))

	return s.searchDDGPage(ctx, ddgURL2)
}

// searchDDGPage parses a DuckDuckGo search page for Spotify links
func (s *Spotify) searchDDGPage(ctx context.Context, ddgURL string) (string, error) {
	resp, err := s.httpClient.Fetch(ctx, ddgURL)
	if err != nil {
		if s.config.Debug {
			s.logger.Debug("DDG fetch error", zap.Error(err))
		}
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	var result string
	doc.Find("a").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		href, exists := sel.Attr("href")
		if !exists {
			return true
		}

		resolved := resolveDDGLink(href)
		if track := canonicalSpotifyTrack(resolved); track != "" {
			result = track
			return false
		}

		// Try album to track conversion
		if reSpotifyAlbum.MatchString(resolved) {
			if track, err := s.tryAlbumToTrack(ctx, resolved); err == nil && track != "" {
				result = track
				return false
			}
		}

		return true
	})

	if result != "" {
		return result, nil
	}

	return "", fmt.Errorf("no Spotify track found in DDG results")
}

// tryAlbumToTrack tries to extract a track from an album URL
func (s *Spotify) tryAlbumToTrack(ctx context.Context, albumURL string) (string, error) {
	resp, err := s.httpClient.Fetch(ctx, albumURL)
	if err != nil {
		return "", err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// Look for spotify:track: URI
	if m := reSpotifyURI.FindSubmatch(body); len(m) == 2 {
		return fmt.Sprintf("https://open.spotify.com/track/%s", m[1]), nil
	}

	// Look for track links
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}

	var track string
	doc.Find("a").EachWithBreak(func(_ int, sel *goquery.Selection) bool {
		href, _ := sel.Attr("href")
		if href != "" {
			if u := canonicalSpotifyTrack(href); u != "" {
				track = u
				return false
			}
		}
		return true
	})

	if track != "" {
		return track, nil
	}

	return "", fmt.Errorf("no track found in album")
}

// canonicalSpotifyTrack extracts and formats a canonical Spotify track URL
func canonicalSpotifyTrack(u string) string {
	if m := reSpotifyTrack.FindStringSubmatch(u); len(m) == 2 {
		return fmt.Sprintf("https://open.spotify.com/track/%s", m[1])
	}
	return ""
}

// resolveDDGLink resolves a DuckDuckGo redirect link to the actual URL
func resolveDDGLink(href string) string {
	if href == "" {
		return ""
	}

	u := href
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	} else if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "/track/") {
		u = "https://duckduckgo.com" + u
	}

	parsed, err := url.Parse(u)
	if err == nil {
		if strings.Contains(parsed.Host, "duckduckgo.com") && strings.HasPrefix(parsed.Path, "/l/") {
			if v := parsed.Query().Get("uddg"); v != "" {
				if dec, err := url.QueryUnescape(v); err == nil {
					return dec
				}
				return v
			}
		}
	}

	return u
}
