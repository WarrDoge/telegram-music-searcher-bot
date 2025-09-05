package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ---------------------------
// Config & Models
// ---------------------------

type Config struct {
	TelegramToken      string
	Debug              bool
	SpotifyDDGFallback bool // optional, default false
}

type SongInfo struct {
	Title       string
	Artist      string
	Album       string
	Platform    string
	OriginalURL string
}

type MusicLinks struct {
	Spotify      string
	YouTubeMusic string
	AppleMusic   string
}

type MusicBot struct {
	config      *Config
	telegramBot *tgbotapi.BotAPI
	httpClient  *http.Client
}

type OEmbedResponse struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// ---------------------------
// Globals & Regex
// ---------------------------

var (
	fetchSem = make(chan struct{}, 8)

	reFirstURL     = regexp.MustCompile(`https?://[^\s]+`)
	reSpotifyTrack = regexp.MustCompile(`/track/([A-Za-z0-9]+)`)
	reSpotifyURI   = regexp.MustCompile(`spotify:track:([A-Za-z0-9]+)`)

	reParenBlock = regexp.MustCompile(`\s*[\(\[][^)\]]*[\)\]]`)
	reFeat       = regexp.MustCompile(`(?i)\s*(feat\.?|featuring)\s+[^-–—·,]+`)
)

// ---------------------------
// Bot Lifecycle
// ---------------------------

func NewMusicBot(config *Config) (*MusicBot, error) {
	bot, err := tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}
	bot.Debug = config.Debug
	log.Printf("Authorized on account %s (debug=%v)", bot.Self.UserName, bot.Debug)

	return &MusicBot{
		config:      config,
		telegramBot: bot,
		httpClient:  &http.Client{Timeout: 12 * time.Second},
	}, nil
}

func (mb *MusicBot) Run() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := mb.telegramBot.GetUpdatesChan(u)
	for update := range updates {
		if update.Message == nil {
			continue
		}
		go mb.handleMessage(update.Message)
	}
}

// ---------------------------
// Message Handling
// ---------------------------

func (mb *MusicBot) handleMessage(m *tgbotapi.Message) {
	chatID := m.Chat.ID
	replyTo := m.MessageID

	if m.IsCommand() {
		switch m.Command() {
		case "start":
			mb.sendReply(chatID, replyTo, md2("🎵 *Welcome to Music Link Converter!*\n\nSend me a Spotify, YouTube/YouTube Music, or Apple Music link, and I'll find it on all platforms for you.\n\n_No API keys required_"))
			return
		case "help":
			mb.sendReply(chatID, replyTo, md2("📖 *How to use:*\n\n1. Send a music link from Spotify, YouTube/YouTube Music, or Apple Music\n2. I'll search for the same song on all platforms\n3. Get links for all three services!\n\n*Supported formats:*\n• Spotify: `https://open.spotify.com/track/...`\n• YouTube Music: `https://music.youtube.com/watch?v=...`\n• YouTube: `https://www.youtube.com/watch?v=...` or `https://youtu.be/...`\n• Apple Music: `https://music.apple.com/...`"))
			return
		}
	}

	info := mb.extractSongInfo(m.Text)
	if info == nil {
		mb.sendReply(chatID, replyTo, md2("❌ Please send a valid *music link* from Spotify, YouTube/YouTube Music, or Apple Music."))
		return
	}

	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}

	mb.sendReply(chatID, replyTo, md2(fmt.Sprintf("🔍 Found: *%s* by *%s*\n\nSearching other platforms…", info.Title, artist)))

	links := mb.findOnAllPlatforms(info)
	if links.Spotify == "" && links.YouTubeMusic == "" && links.AppleMusic == "" {
		mb.sendReply(chatID, replyTo, md2("😕 I couldn't find matches on other platforms. It might be a regional or rare release."))
		return
	}

	mb.sendLinks(chatID, replyTo, info, links)
}

// ---------------------------
// Telegram helpers
// ---------------------------

func (mb *MusicBot) sendReply(chatID int64, replyToID int, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = replyToID
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	msg.DisableWebPagePreview = true

	if _, err := mb.telegramBot.Send(msg); err != nil {
		log.Printf("Error sending message with MarkdownV2 (retrying plain): %v | text=%q", err, text)
		msg.ParseMode = ""
		_, _ = mb.telegramBot.Send(msg)
	}
}

func (mb *MusicBot) sendLinks(chatID int64, replyToID int, info *SongInfo, links *MusicLinks) {
	title := md2(info.Title)
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}
	artist = md2(artist)

	text := fmt.Sprintf("✅ *%s* by *%s*\n🎧 Pick a platform:", title, artist)

	// Row 1: platform buttons
	var row1 []tgbotapi.InlineKeyboardButton
	if links.Spotify != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("Spotify", links.Spotify))
	}
	if links.YouTubeMusic != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("YouTube Music", links.YouTubeMusic))
	}
	if links.AppleMusic != "" {
		row1 = append(row1, tgbotapi.NewInlineKeyboardButtonURL("Apple Music", links.AppleMusic))
	}

	// Row 2: Source button (avoid raw URL in Markdown text)
	var rows [][]tgbotapi.InlineKeyboardButton
	if len(row1) > 0 {
		rows = append(rows, row1)
	}
	if info.OriginalURL != "" && info.Platform != "" {
		source := fmt.Sprintf("Source: %s", info.Platform)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonURL(source, info.OriginalURL)})
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = replyToID
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	msg.DisableWebPagePreview = true
	if len(rows) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	if _, err := mb.telegramBot.Send(msg); err != nil {
		log.Printf("Error sending links: %v", err)
	}
}

func md2(s string) string { return tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, s) }

// ---------------------------
// URL & HTTP helpers
// ---------------------------

func firstURL(s string) string {
	m := reFirstURL.FindString(s)
	return strings.TrimRight(m, ".,);!?]}>\"'")
}

func (mb *MusicBot) fetch(target string) (*http.Response, error) {
	fetchSem <- struct{}{}
	defer func() { <-fetchSem }()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MusicLinkBot/1.0)")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,uk;q=0.8,ru;q=0.7")

	resp, err := mb.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if mb.config.Debug {
			log.Printf("HTTP %d for %s", resp.StatusCode, target)
		}
		resp.Body.Close()
		return nil, fmt.Errorf("non-200 %d for %s", resp.StatusCode, target)
	}
	return resp, nil
}

func joinApple(base, href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	u, err := url.Parse(base)
	if err != nil {
		return "https://music.apple.com" + href
	}
	return u.Scheme + "://" + u.Host + href
}

// ---------------------------
// Normalization helpers
// ---------------------------

func normalizeQuery(artist, title string) string {
	clean := func(s string) string {
		s = reParenBlock.ReplaceAllString(s, "")
		s = reFeat.ReplaceAllString(s, "")
		repls := []string{
			" - single", "", " - ep", "", " - album", "",
			" – single", "", " – ep", "",
			" remastered", "", " - remaster", "", " remaster", "",
			" - radio edit", "", " radio edit", "",
			" official video", "", " lyric video", "", " lyrics", "",
		}
		ls := strings.ToLower(s)
		for i := 0; i < len(repls); i += 2 {
			ls = strings.ReplaceAll(ls, repls[i], repls[i+1])
		}
		ls = strings.Join(strings.Fields(ls), " ")
		return ls
	}
	a := clean(artist)
	t := clean(title)
	return strings.TrimSpace(a + " " + t)
}

func normalizeForMatch(s string) string {
	s = strings.ToLower(s)
	s = reParenBlock.ReplaceAllString(s, "")
	s = strings.NewReplacer("-", " ", "—", " ", "–", " ", "·", " ", ".", " ", ",", " ", "!", " ", "?", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// ---------------------------
// Platform parsers
// ---------------------------

func (mb *MusicBot) getSpotifyInfo(spotifyURL string) *SongInfo {
	if !reSpotifyTrack.MatchString(spotifyURL) {
		return nil
	}
	oembedURL := fmt.Sprintf("https://open.spotify.com/oembed?url=%s", url.QueryEscape(spotifyURL))
	resp, err := mb.fetch(oembedURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Spotify OEmbed error: %v", err)
		}
		return mb.fallbackSpotifyScrape(spotifyURL)
	}
	defer resp.Body.Close()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		if mb.config.Debug {
			log.Printf("Spotify OEmbed decode error: %v", err)
		}
		return mb.fallbackSpotifyScrape(spotifyURL)
	}
	parts := strings.Split(oembed.Title, " · ")
	if len(parts) >= 2 {
		return &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: "Spotify", OriginalURL: spotifyURL}
	}
	return &SongInfo{Title: strings.TrimSpace(oembed.Title), Artist: "Unknown Artist", Platform: "Spotify", OriginalURL: spotifyURL}
}

func (mb *MusicBot) fallbackSpotifyScrape(spotifyURL string) *SongInfo {
	resp, err := mb.fetch(spotifyURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Spotify page fetch error: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Spotify HTML parse error: %v", err)
		}
		return nil
	}
	title := doc.Find("meta[property='og:title']").AttrOr("content", "")
	description := doc.Find("meta[property='og:description']").AttrOr("content", "")

	if title != "" {
		title = strings.ReplaceAll(title, " - song and lyrics by ", " - ")
		title = strings.ReplaceAll(title, " - song by ", " - ")
		parts := strings.Split(title, " - ")
		if len(parts) >= 2 {
			return &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: "Spotify", OriginalURL: spotifyURL}
		}
	}
	if description != "" {
		parts := strings.Split(description, " · ")
		if len(parts) >= 2 {
			return &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: "Spotify", OriginalURL: spotifyURL}
		}
	}
	return nil
}

func (mb *MusicBot) getYouTubeInfo(youtubeURL string, platform string) *SongInfo {
	resp, err := mb.fetch(youtubeURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube fetch error: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube parse error: %v", err)
		}
		return nil
	}

	js := firstJSONInScriptContaining(doc, "ytInitialPlayerResponse")
	if js != "" {
		var pr struct {
			VideoDetails struct {
				Title  string `json:"title"`
				Author string `json:"author"`
			} `json:"videoDetails"`
		}
		if json.Unmarshal([]byte(js), &pr) == nil && pr.VideoDetails.Title != "" {
			return &SongInfo{
				Title:       strings.TrimSpace(pr.VideoDetails.Title),
				Artist:      strings.TrimSpace(pr.VideoDetails.Author),
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
		}
	}

	title := doc.Find("meta[property='og:title']").AttrOr("content", "")
	videoDetails := doc.Find("meta[property='og:video:tag']").AttrOr("content", "")
	title = strings.ReplaceAll(title, " - YouTube Music", "")
	title = strings.ReplaceAll(title, " - YouTube", "")

	if strings.Contains(title, " - ") {
		parts := strings.SplitN(title, " - ", 2)
		if len(parts) == 2 {
			return &SongInfo{Title: strings.TrimSpace(parts[1]), Artist: strings.TrimSpace(parts[0]), Platform: platform, OriginalURL: youtubeURL}
		}
	} else if strings.Contains(title, " · ") {
		parts := strings.SplitN(title, " · ", 2)
		if len(parts) == 2 {
			return &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: platform, OriginalURL: youtubeURL}
		}
	} else if title != "" {
		return &SongInfo{Title: strings.TrimSpace(title), Artist: strings.TrimSpace(videoDetails), Platform: platform, OriginalURL: youtubeURL}
	}
	return nil
}

func (mb *MusicBot) getAppleMusicInfo(appleURL string) *SongInfo {
	resp, err := mb.fetch(appleURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Apple fetch error: %v", err)
		}
		return nil
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Apple parse error: %v", err)
		}
		return nil
	}

	title, artist := appleLD(doc)
	if title == "" {
		title = doc.Find("meta[property='og:title']").AttrOr("content", "")
	}
	if title != "" && artist == "" {
		if strings.Contains(title, " by ") && strings.Contains(title, " on Apple Music") {
			parts := strings.SplitN(title, " by ", 2)
			title = strings.TrimSpace(parts[0])
			artist = strings.TrimSuffix(strings.TrimSpace(parts[1]), " on Apple Music")
		}
		if artist == "" && strings.Contains(title, ",") && strings.Contains(title, " в Apple Music") {
			parts := strings.SplitN(title, ",", 2)
			title = strings.TrimSpace(parts[0])
			artist = strings.TrimSuffix(strings.TrimSpace(parts[1]), " в Apple Music")
		}
	}
	title = strings.ReplaceAll(title, " - Single", "")
	title = strings.ReplaceAll(title, " - EP", "")
	title = strings.ReplaceAll(title, " on Apple Music", "")
	title = strings.ReplaceAll(title, " в Apple Music", "")

	if title == "" {
		return nil
	}
	return &SongInfo{Title: strings.TrimSpace(title), Artist: strings.TrimSpace(artist), Platform: "Apple Music", OriginalURL: appleURL}
}

func appleLD(doc *goquery.Document) (title, artist string) {
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
			t, _ := obj["@type"].(string)
			if t == "MusicRecording" || t == "MusicAlbum" || t == "CreativeWork" {
				if n, ok := obj["name"].(string); ok && n != "" {
					title = n
				}
				switch by := obj["byArtist"].(type) {
				case map[string]interface{}:
					if n, ok := by["name"].(string); ok {
						artist = n
					}
				case []interface{}:
					for _, it := range by {
						if m, ok := it.(map[string]interface{}); ok {
							if n, ok := m["name"].(string); ok && n != "" {
								artist = n
								break
							}
						}
					}
				}
				if title != "" {
					return false
				}
			}
		}
		return true
	})
	return
}

// ---------------------------
// Search
// ---------------------------

func (mb *MusicBot) findOnAllPlatforms(info *SongInfo) *MusicLinks {
	links := &MusicLinks{}
	set := func(p *string, v string) {
		if v != "" {
			*p = v
		}
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if info.Platform == "Spotify" {
			set(&links.Spotify, info.OriginalURL)
		} else {
			set(&links.Spotify, mb.searchSpotify(info))
		}
	}()
	go func() {
		defer wg.Done()
		if info.Platform == "YouTube Music" || info.Platform == "YouTube" {
			set(&links.YouTubeMusic, info.OriginalURL)
		} else {
			set(&links.YouTubeMusic, mb.searchYouTube(info))
		}
	}()
	go func() {
		defer wg.Done()
		if info.Platform == "Apple Music" {
			set(&links.AppleMusic, info.OriginalURL)
		} else {
			set(&links.AppleMusic, mb.searchAppleMusic(info))
		}
	}()

	wg.Wait()
	return links
}

func (mb *MusicBot) searchYouTube(info *SongInfo) string {
	query := normalizeQuery(info.Artist, info.Title)
	searchURL := fmt.Sprintf("https://music.youtube.com/search?q=%s", url.QueryEscape(query))

	resp, err := mb.fetch(searchURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YT Music search fetch failed: %v", err)
		}
		return searchURL
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YT Music search parse failed: %v", err)
		}
		return searchURL
	}

	jsonStr := firstJSONInScriptContaining(doc, "ytInitialData")
	if jsonStr == "" {
		return searchURL
	}
	var any map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &any); err != nil {
		if mb.config.Debug {
			log.Printf("ytInitialData unmarshal failed: %v", err)
		}
		return searchURL
	}
	if vid := findPreferredYouTubeVideoID(any, info.Artist, info.Title); vid != "" {
		return "https://music.youtube.com/watch?v=" + vid
	}
	if vid := extractFirstYouTubeVideoID(any); vid != "" {
		return "https://music.youtube.com/watch?v=" + vid
	}
	return searchURL
}

// Spotify: JSON/regex/anchors; optional DDG fallback via env
func (mb *MusicBot) searchSpotify(info *SongInfo) string {
	query := normalizeQuery(info.Artist, info.Title)
	searchURL := fmt.Sprintf("https://open.spotify.com/search/%s", url.QueryEscape(query))

	resp, err := mb.fetch(searchURL)
	if err == nil {
		defer resp.Body.Close()
		doc, err2 := goquery.NewDocumentFromReader(resp.Body)
		if err2 == nil {
			// look for spotify:track:ID in scripts
			foundID := ""
			doc.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
				t := s.Text()
				if m := reSpotifyURI.FindStringSubmatch(t); len(m) == 2 {
					foundID = m[1]
					return false
				}
				return true
			})
			if foundID != "" {
				return "https://open.spotify.com/track/" + foundID
			}
			// anchors
			var track string
			doc.Find("a").EachWithBreak(func(_ int, a *goquery.Selection) bool {
				href, _ := a.Attr("href")
				if m := reSpotifyTrack.FindStringSubmatch(href); len(m) == 2 {
					if strings.HasPrefix(href, "http") {
						track = href
					} else {
						track = "https://open.spotify.com" + href
					}
					return false
				}
				return true
			})
			if track != "" {
				return track
			}
		}
	}

	// optional: DDG fallback (off by default)
	if mb.config.SpotifyDDGFallback {
		if u := mb.searchSpotifyViaDDG(info.Artist, info.Title); u != "" {
			return u
		}
	}

	return searchURL
}

// Optional DuckDuckGo HTML fallback (disabled unless SpotifyDDGFallback=true)
func (mb *MusicBot) searchSpotifyViaDDG(artist, title string) string {
	q := "site:open.spotify.com/track " + normalizeQuery(artist, title)
	ddg := "https://duckduckgo.com/html/?q=" + url.QueryEscape(q)

	resp, err := mb.fetch(ddg)
	if err != nil {
		if mb.config.Debug {
			log.Printf("DDG fetch failed: %v", err)
		}
		return ""
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("DDG parse failed: %v", err)
		}
		return ""
	}

	found := ""
	doc.Find("a").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		href, _ := a.Attr("href")
		if strings.HasPrefix(href, "https://open.spotify.com/track/") {
			found = href
			return false
		}
		return true
	})
	return found
}

func (mb *MusicBot) searchAppleMusic(info *SongInfo) string {
	if info.Platform == "Apple Music" && info.OriginalURL != "" {
		return info.OriginalURL
	}
	query := normalizeQuery(info.Artist, info.Title)
	searchURL := fmt.Sprintf("https://music.apple.com/search?term=%s", url.QueryEscape(query))

	resp, err := mb.fetch(searchURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Apple search fetch failed: %v", err)
		}
		return searchURL
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Apple search parse failed: %v", err)
		}
		return searchURL
	}

	var found string
	doc.Find("a").EachWithBreak(func(_ int, a *goquery.Selection) bool {
		href, _ := a.Attr("href")
		if href == "" {
			return true
		}
		if strings.Contains(href, "/song/") || (strings.Contains(href, "/album/") && strings.Contains(href, "?i=")) {
			found = joinApple(searchURL, href)
			return false
		}
		return true
	})
	if found != "" {
		return found
	}

	// iTunes Search API with scoring to avoid wrong tracks
	api := fmt.Sprintf("https://itunes.apple.com/search?term=%s&media=music&entity=song&limit=10", url.QueryEscape(query))
	resp2, err := mb.fetch(api)
	if err == nil {
		defer resp2.Body.Close()
		var res struct {
			Results []struct {
				TrackViewURL string `json:"trackViewUrl"`
				TrackName    string `json:"trackName"`
				ArtistName   string `json:"artistName"`
			} `json:"results"`
		}
		if err := json.NewDecoder(resp2.Body).Decode(&res); err == nil {
			wantT := normalizeForMatch(info.Title)
			wantA := normalizeForMatch(info.Artist)
			best := ""
			bestScore := -999
			for _, r := range res.Results {
				if r.TrackViewURL == "" {
					continue
				}
				t := normalizeForMatch(r.TrackName)
				a := normalizeForMatch(r.ArtistName)
				score := 0
				if wantT != "" && strings.Contains(t, wantT) {
					score += 3
				}
				if wantA != "" && strings.Contains(a, wantA) {
					score += 3
				}
				if score > bestScore {
					bestScore = score
					best = r.TrackViewURL
				}
			}
			// only accept if decent match; otherwise keep search URL
			if bestScore >= 3 {
				return best
			}
		}
	}

	return searchURL
}

// ---------------------------
// JSON helpers
// ---------------------------

func firstJSONInScriptContaining(doc *goquery.Document, needle string) string {
	var out string
	doc.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		t := s.Text()
		if strings.Contains(t, needle) {
			start := strings.Index(t, "{")
			end := strings.LastIndex(t, "}")
			if start != -1 && end != -1 && end > start {
				out = t[start : end+1]
				return false
			}
		}
		return true
	})
	return out
}

type ytCandidate struct {
	ID      string
	Title   string
	Channel string
}

func collectYouTubeCandidates(v interface{}, out *[]ytCandidate) {
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
			*out = append(*out, ytCandidate{ID: id, Title: title, Channel: channel})
		}
		for _, vv := range x {
			collectYouTubeCandidates(vv, out)
		}
	case []interface{}:
		for _, vv := range x {
			collectYouTubeCandidates(vv, out)
		}
	}
}

func scoreYouTubeCandidate(c ytCandidate, wantArtist, wantTitle string) int {
	score := 0
	na := normalizeForMatch(wantArtist)
	nt := normalizeForMatch(wantTitle)
	ct := normalizeForMatch(c.Title)
	cc := normalizeForMatch(c.Channel)

	if strings.Contains(ct, nt) {
		score += 3
	}
	if strings.Contains(ct, na) {
		score += 2
	}
	if strings.Contains(cc, na) {
		score += 2
	}
	if strings.Contains(ct, "cover") || strings.Contains(ct, "lyrics") || strings.Contains(ct, "lyric") {
		score -= 2
	}
	if strings.Contains(ct, "live") && !strings.Contains(ct, "studio") {
		score -= 1
	}
	return score
}

func findPreferredYouTubeVideoID(v interface{}, artist, title string) string {
	var cands []ytCandidate
	collectYouTubeCandidates(v, &cands)
	bestScore := -999
	bestID := ""
	for _, c := range cands {
		if c.ID == "" {
			continue
		}
		s := scoreYouTubeCandidate(c, artist, title)
		if s > bestScore {
			bestScore = s
			bestID = c.ID
		}
	}
	return bestID
}

func extractFirstYouTubeVideoID(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		if idRaw, ok := x["videoId"]; ok {
			if s, ok := idRaw.(string); ok && len(s) >= 8 {
				return s
			}
		}
		for _, vv := range x {
			if id := extractFirstYouTubeVideoID(vv); id != "" {
				return id
			}
		}
	case []interface{}:
		for _, vv := range x {
			if id := extractFirstYouTubeVideoID(vv); id != "" {
				return id
			}
		}
	}
	return ""
}

// ---------------------------
// Extraction entry
// ---------------------------

func (mb *MusicBot) extractSongInfo(text string) *SongInfo {
	text = strings.TrimSpace(text)
	urlStr := firstURL(text)
	if urlStr == "" {
		return nil
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Host)

	switch {
	case strings.HasSuffix(host, "open.spotify.com"):
		if reSpotifyTrack.MatchString(strings.ToLower(u.Path)) {
			return mb.getSpotifyInfo(urlStr)
		}
		return nil
	case strings.HasSuffix(host, "music.youtube.com"):
		return mb.getYouTubeInfo(urlStr, "YouTube Music")
	case strings.HasSuffix(host, "youtube.com") || strings.HasSuffix(host, "youtu.be"):
		return mb.getYouTubeInfo(urlStr, "YouTube")
	case strings.HasSuffix(host, "music.apple.com") || strings.HasSuffix(host, "itunes.apple.com"):
		return mb.getAppleMusicInfo(urlStr)
	default:
		return nil
	}
}

// ---------------------------
// main
// ---------------------------

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "1" || v == "true" || v == "yes" || v == "y" {
		return true
	}
	if v == "0" || v == "false" || v == "no" || v == "n" {
		return false
	}
	return def
}

func main() {
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	config := &Config{
		TelegramToken:      telegramToken,
		Debug:              envBool("BOT_DEBUG", false),
		SpotifyDDGFallback: envBool("SPOTIFY_DDG_FALLBACK", false),
	}

	bot, err := NewMusicBot(config)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("🎵 Music Link Converter Bot is running...")
	log.Println("No API keys required — using web scraping!")
	bot.Run()
}
