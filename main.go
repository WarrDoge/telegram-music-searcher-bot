package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
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
	SpotifyDDGFallback bool // optional, default false (we will still fall back to DDG if primary fails)
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

	// caches
	queryCache *simpleCache // normalized query -> platform URL (only successful WATCH/TRACK/SONG urls, never search pages)
	urlCache   *simpleCache // original URL -> JSON-encoded SongInfo
}

type OEmbedResponse struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// ---------------------------
// Globals & Regex
// ---------------------------

var (
	fetchSem = make(chan struct{}, 8)  // cap parallel HTTP fetches
	msgSem   = make(chan struct{}, 32) // cap parallel message handlers

	reFirstURL = regexp.MustCompile(`https?://[^\s]+`)
	// support optional /intl-xx/ prefix that Spotify uses
	reSpotifyTrack = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?track/([A-Za-z0-9]+)`) // path matcher
	reSpotifyURI   = regexp.MustCompile(`spotify:track:([A-Za-z0-9]+)`)                   // embedded URIs in scripts

	reParenBlock = regexp.MustCompile(`\s*[\(\[][^)\]]*[\)\]]`)
	reFeat       = regexp.MustCompile(`(?i)\s*(feat\.?|featuring)\s+[^-–—·,]+`)
)

// seed RNG for backoff jitter
func init() { rand.Seed(time.Now().UnixNano()) }

// ---------------------------
// HTTP transport
// ---------------------------

var defaultTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	ForceAttemptHTTP2:     true,
}

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

	client := &http.Client{Transport: defaultTransport}

	return &MusicBot{
		config:      config,
		telegramBot: bot,
		httpClient:  client,
		queryCache:  newSimpleCache(),
		urlCache:    newSimpleCache(),
	}, nil
}

func (mb *MusicBot) Run(ctx context.Context) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := mb.telegramBot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			log.Println("Shutting down bot loop…")
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message == nil {
				continue
			}
			msgSem <- struct{}{}
			go func(m *tgbotapi.Message) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("panic in handleMessage: %v", r)
					}
					<-msgSem
				}()
				mb.handleMessage(m)
			}(update.Message)
		}
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
	for _, chunk := range chunkText(text, 3500) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		msg.ReplyToMessageID = replyToID
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		msg.DisableWebPagePreview = true

		if _, err := mb.telegramBot.Send(msg); err != nil {
			log.Printf("Error sending message with MarkdownV2 (retrying plain): %v | textLen=%d", err, len(chunk))
			msg.ParseMode = ""
			_, _ = mb.telegramBot.Send(msg)
		}
	}
}

func chunkText(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > 0 {
		if len(s) <= max {
			out = append(out, s)
			break
		}
		// try split on last newline within window
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/2 { // too early or no newline; hard split
			cut = max
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

func (mb *MusicBot) sendLinks(chatID int64, replyToID int, info *SongInfo, links *MusicLinks) {
	title := md2(info.Title)
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}
	artist = md2(artist)

	text := fmt.Sprintf("✅ *%s* by *%s*\n🎧 Pick a platform:", title, artist)

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

func (mb *MusicBot) fetchCtx(ctx context.Context, target string) (*http.Response, error) {
	fetchSem <- struct{}{}
	defer func() { <-fetchSem }()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; MusicLinkBot/1.0)")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,uk;q=0.8,ru;q=0.7")
		req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")

		resp, err := mb.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d for %s", resp.StatusCode, target)
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("status %d for %s", resp.StatusCode, target)
		}
		time.Sleep(time.Duration(150+rand.Intn(200)) * time.Millisecond)
	}
	return nil, lastErr
}

func (mb *MusicBot) fetch(target string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return mb.fetchCtx(ctx, target)
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
	s = strings.NewReplacer(
		"-", " ", "—", " ", "–", " ", "·", " ", ".", " ", ",", " ",
		"!", " ", "?", " ", "/", " ", "&", " and ", "'", " ", "’", " ",
	).Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// ---------------------------
// Platform parsers
// ---------------------------

func (mb *MusicBot) getSpotifyInfo(spotifyURL string) *SongInfo {
	if v, ok := mb.urlCache.Get("info:" + spotifyURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si
		}
	}
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
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		if mb.config.Debug {
			log.Printf("Spotify OEmbed decode error: %v", err)
		}
		return mb.fallbackSpotifyScrape(spotifyURL)
	}
	parts := strings.Split(oembed.Title, " · ")
	var si *SongInfo
	if len(parts) >= 2 {
		si = &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: "Spotify", OriginalURL: spotifyURL}
	} else {
		si = &SongInfo{Title: strings.TrimSpace(oembed.Title), Artist: "Unknown Artist", Platform: "Spotify", OriginalURL: spotifyURL}
	}
	if b, err := json.Marshal(si); err == nil {
		mb.urlCache.Set("info:"+spotifyURL, string(b), 24*time.Hour)
	}
	return si
}

func (mb *MusicBot) fallbackSpotifyScrape(spotifyURL string) *SongInfo {
	resp, err := mb.fetch(spotifyURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Spotify page fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

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

func (mb *MusicBot) youTubeOEmbedInfo(youtubeURL, platform string) *SongInfo {
	o := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape(youtubeURL)
	resp, err := mb.fetch(o)
	if err != nil {
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	var r struct {
		Title  string `json:"title"`
		Author string `json:"author_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) == nil && r.Title != "" {
		artist := strings.TrimSuffix(r.Author, " - Topic")
		return &SongInfo{Title: strings.TrimSpace(r.Title), Artist: strings.TrimSpace(artist), Platform: platform, OriginalURL: youtubeURL}
	}
	return nil
}

func (mb *MusicBot) getYouTubeInfo(youtubeURL string, platform string) *SongInfo {
	if v, ok := mb.urlCache.Get("info:" + youtubeURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si
		}
	}
	if s := mb.youTubeOEmbedInfo(youtubeURL, platform); s != nil {
		if b, err := json.Marshal(s); err == nil {
			mb.urlCache.Set("info:"+youtubeURL, string(b), 24*time.Hour)
		}
		return s
	}

	resp, err := mb.fetch(youtubeURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube parse error: %v", err)
		}
		return nil
	}

	js := jsonObjectFromScriptContaining(doc, "ytInitialPlayerResponse")
	if js != "" {
		var pr struct {
			VideoDetails struct {
				Title  string `json:"title"`
				Author string `json:"author"`
			} `json:"videoDetails"`
		}
		if json.Unmarshal([]byte(js), &pr) == nil && pr.VideoDetails.Title != "" {
			out := &SongInfo{
				Title:       strings.TrimSpace(pr.VideoDetails.Title),
				Artist:      strings.TrimSpace(strings.TrimSuffix(pr.VideoDetails.Author, " - Topic")),
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
			if b, err := json.Marshal(out); err == nil {
				mb.urlCache.Set("info:"+youtubeURL, string(b), 24*time.Hour)
			}
			return out
		}
	}

	title := doc.Find("meta[property='og:title']").AttrOr("content", "")
	videoDetails := doc.Find("meta[property='og:video:tag']").AttrOr("content", "")
	title = strings.ReplaceAll(title, " - YouTube Music", "")
	title = strings.ReplaceAll(title, " - YouTube", "")

	if strings.Contains(title, " - ") {
		parts := strings.SplitN(title, " - ", 2)
		if len(parts) == 2 {
			out := &SongInfo{Title: strings.TrimSpace(parts[1]), Artist: strings.TrimSpace(parts[0]), Platform: platform, OriginalURL: youtubeURL}
			if b, err := json.Marshal(out); err == nil {
				mb.urlCache.Set("info:"+youtubeURL, string(b), 24*time.Hour)
			}
			return out
		}
	} else if strings.Contains(title, " · ") {
		parts := strings.SplitN(title, " · ", 2)
		if len(parts) == 2 {
			out := &SongInfo{Title: strings.TrimSpace(parts[0]), Artist: strings.TrimSpace(parts[1]), Platform: platform, OriginalURL: youtubeURL}
			if b, err := json.Marshal(out); err == nil {
				mb.urlCache.Set("info:"+youtubeURL, string(b), 24*time.Hour)
			}
			return out
		}
	} else if title != "" {
		out := &SongInfo{Title: strings.TrimSpace(title), Artist: strings.TrimSpace(videoDetails), Platform: platform, OriginalURL: youtubeURL}
		if b, err := json.Marshal(out); err == nil {
			mb.urlCache.Set("info:"+youtubeURL, string(b), 24*time.Hour)
		}
		return out
	}
	return nil
}

func (mb *MusicBot) getAppleMusicInfo(appleURL string) *SongInfo {
	if v, ok := mb.urlCache.Get("info:" + appleURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si
		}
	}

	resp, err := mb.fetch(appleURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("Apple fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

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
	title = strings.ReplaceAll(title, " - Album", "")
	title = strings.ReplaceAll(title, " on Apple Music", "")
	title = strings.ReplaceAll(title, " в Apple Music", "")

	if title == "" {
		return nil
	}
	out := &SongInfo{Title: strings.TrimSpace(title), Artist: strings.TrimSpace(artist), Platform: "Apple Music", OriginalURL: appleURL}
	if b, err := json.Marshal(out); err == nil {
		mb.urlCache.Set("info:"+appleURL, string(b), 24*time.Hour)
	}
	return out
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
		if info.Platform == "YouTube Music" {
			set(&links.YouTubeMusic, info.OriginalURL)
		} else if info.Platform == "YouTube" {
			if u := toYouTubeMusicURL(info.OriginalURL); u != "" {
				set(&links.YouTubeMusic, u)
			} else {
				set(&links.YouTubeMusic, mb.searchYouTube(info))
			}
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
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("yt:" + key); ok {
		return v
	}

	query := key

	// 1) Try YouTube Music search (preferred)
	if u := mb.searchYouTubeViaPage("https://music.youtube.com/search?q="+url.QueryEscape(query), info); u != "" {
		mb.queryCache.Set("yt:"+key, u, 24*time.Hour)
		return u
	}

	// 2) Fallback: classic YouTube search
	if u := mb.searchYouTubeViaPage("https://www.youtube.com/results?search_query="+url.QueryEscape(query), info); u != "" {
		// prefer a music.youtube.com watch url for consistency
		if vid := youtubeVideoIDFromURL(u); vid != "" {
			u = "https://music.youtube.com/watch?v=" + vid
		}
		mb.queryCache.Set("yt:"+key, u, 24*time.Hour)
		return u
	}

	// 3) Last resort: return YT Music search page (DO NOT cache failures)
	return "https://music.youtube.com/search?q=" + url.QueryEscape(query)
}

func (mb *MusicBot) searchYouTubeViaPage(searchURL string, info *SongInfo) string {
	resp, err := mb.fetch(searchURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube search fetch failed: %v", err)
		}
		return ""
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		if mb.config.Debug {
			log.Printf("YouTube search parse failed: %v", err)
		}
		return ""
	}

	jsonStr := jsonObjectFromScriptContaining(doc, "ytInitialData")
	if jsonStr == "" {
		return ""
	}
	var any map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &any); err != nil {
		if mb.config.Debug {
			log.Printf("ytInitialData unmarshal failed: %v", err)
		}
		return ""
	}
	if vid := findPreferredYouTubeVideoID(any, info.Artist, info.Title); vid != "" {
		return "https://music.youtube.com/watch?v=" + vid
	}
	if vid := extractFirstYouTubeVideoID(any); vid != "" {
		return "https://music.youtube.com/watch?v=" + vid
	}
	return ""
}

// Spotify: JSON/regex/anchors; now with reliable DDG fallback regardless of config
func (mb *MusicBot) searchSpotify(info *SongInfo) string {
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("sp:" + key); ok {
		return v
	}

	query := key
	searchURL := fmt.Sprintf("https://open.spotify.com/search/%s", url.QueryEscape(query))

	// Try scraping the search page first (works occasionally when HTML contains preloaded data)
	if resp, err := mb.fetch(searchURL); err == nil {
		func() {
			defer func() {
				if resp != nil && resp.Body != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
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
					u := "https://open.spotify.com/track/" + foundID
					mb.queryCache.Set("sp:"+key, u, 24*time.Hour)
					return
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
					mb.queryCache.Set("sp:"+key, track, 24*time.Hour)
					return
				}
			}
		}()
	}

	// DDG fallback (reliable, no API keys)
	if u := mb.searchSpotifyViaDDG(info.Artist, info.Title); u != "" {
		mb.queryCache.Set("sp:"+key, u, 24*time.Hour)
		return u
	}

	// As a last resort return the search page (DO NOT cache failures)
	return searchURL
}

// Optional DuckDuckGo HTML fallback (now used whenever primary fails)
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
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

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

func storefrontFromAppleURL(u *url.URL) string {
	// formats: /us/album/... or /ua/song/...
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && len(parts[0]) == 2 {
		return strings.ToUpper(parts[0])
	}
	return "US"
}

func (mb *MusicBot) searchAppleMusic(info *SongInfo) string {
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("am:" + key); ok {
		return v
	}

	if info.Platform == "Apple Music" && info.OriginalURL != "" {
		mb.queryCache.Set("am:"+key, info.OriginalURL, 24*time.Hour)
		return info.OriginalURL
	}
	query := key
	searchURL := fmt.Sprintf("https://music.apple.com/search?term=%s", url.QueryEscape(query))

	// Try Apple Music search HTML (may or may not contain anchors server-side)
	if resp, err := mb.fetch(searchURL); err == nil {
		func() {
			defer func() {
				if resp != nil && resp.Body != nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
			}()
			doc, err2 := goquery.NewDocumentFromReader(resp.Body)
			if err2 == nil {
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
					mb.queryCache.Set("am:"+key, found, 24*time.Hour)
					return
				}
			}
		}()
	}

	// iTunes Search API with scoring; pass storefront country if we can
	country := "US"
	if info.OriginalURL != "" {
		if u, err := url.Parse(info.OriginalURL); err == nil {
			country = storefrontFromAppleURL(u)
		}
	}
	api := fmt.Sprintf("https://itunes.apple.com/search?term=%s&media=music&entity=song&limit=10&country=%s", url.QueryEscape(query), country)
	if resp2, err := mb.fetch(api); err == nil {
		defer func() { io.Copy(io.Discard, resp2.Body); resp2.Body.Close() }()
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
			if bestScore >= 3 && best != "" {
				// prefer music.apple.com domain
				best = strings.Replace(best, "itunes.apple.com", "music.apple.com", 1)
				mb.queryCache.Set("am:"+key, best, 24*time.Hour)
				return best
			}
		}
	}

	// Last resort: return search page (DO NOT cache failures)
	return searchURL
}

// ---------------------------
// JSON helpers
// ---------------------------

func jsonObjectFromScriptContaining(doc *goquery.Document, needle string) string {
	var out string
	doc.Find("script").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		t := s.Text()
		idx := strings.Index(t, needle)
		if idx == -1 {
			return true
		}
		start := strings.Index(t[idx:], "{")
		if start == -1 {
			return true
		}
		start += idx
		depth := 0
		for i := start; i < len(t); i++ {
			switch t[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					out = t[start : i+1]
					return false
				}
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
		score += 4
	}
	if strings.Contains(ct, na) {
		score += 3
	}
	if strings.Contains(cc, na) {
		score += 2
	}
	// prefer auto-generated artist topic channels slightly
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
// simple in-memory TTL cache
// ---------------------------

type cacheEntry struct {
	val string
	exp time.Time
}

type simpleCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

func newSimpleCache() *simpleCache { return &simpleCache{m: make(map[string]cacheEntry)} }

func (c *simpleCache) Get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		return "", false
	}
	return e.val, true
}

func (c *simpleCache) Set(k, v string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry{val: v, exp: time.Now().Add(ttl)}
}

// ---------------------------
// Small helpers for YouTube URLs
// ---------------------------

func youtubeVideoIDFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Host) {
	case "youtu.be":
		return strings.Trim(u.Path, "/")
	default:
		v := u.Query().Get("v")
		if v != "" {
			return v
		}
		return ""
	}
}

func toYouTubeMusicURL(raw string) string {
	if id := youtubeVideoIDFromURL(raw); id != "" {
		return "https://music.youtube.com/watch?v=" + id
	}
	return ""
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
		SpotifyDDGFallback: envBool("SPOTIFY_DDG_FALLBACK", false), // preserved for compat; we still use DDG fallback if primary fails
	}

	bot, err := NewMusicBot(config)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("🎵 Music Link Converter Bot is running…")
	log.Println("No API keys required — using web scraping!")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bot.Run(ctx)
}
