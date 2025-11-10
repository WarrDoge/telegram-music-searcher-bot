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
	"github.com/gocolly/colly/v2"
	"github.com/sony/gobreaker"
	"github.com/texttheater/golang-levenshtein/levenshtein"
	"go.uber.org/zap"
)

// ---------------------------
// Configuration & Types
// ---------------------------

type Config struct {
	TelegramToken          string
	Debug                  bool
	MaxConcurrentFetches   int
	MaxConcurrentMessages  int
	RequestTimeout         time.Duration
	RetryAttempts          int
	RetryMinDelay          time.Duration
	RetryMaxDelay          time.Duration
	CacheTTL               time.Duration
	NegativeCacheTTL       time.Duration // Cache failed searches separately
	FuzzyMatchThreshold    float64       // Levenshtein distance threshold (0-1)
	CircuitBreakerMaxFails int           // Open circuit after N failures
	CircuitBreakerTimeout  time.Duration // Circuit breaker timeout
	TextMirrorURL          string        // Text mirror service (r.jina.ai)
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
	collector   *colly.Collector
	logger      *zap.Logger

	// Caches
	queryCache    *SimpleCache
	urlCache      *SimpleCache
	negativeCache *SimpleCache // Cache for failed searches

	// Circuit Breakers per service
	spotifyBreaker    *gobreaker.CircuitBreaker
	youtubeBreaker    *gobreaker.CircuitBreaker
	appleMusicBreaker *gobreaker.CircuitBreaker

	// Semaphores
	fetchSem *Semaphore
	msgSem   *Semaphore
}

type OEmbedResponse struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	AuthorName  string `json:"author_name"`
}

type YouTubeCandidate struct {
	ID      string
	Title   string
	Channel string
}

// ---------------------------
// Patterns & Constants
// ---------------------------

var (
	USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// URL and parsing patterns
	reFirstURL     = regexp.MustCompile(`https?://[^\s]+`)
	reSpotifyTrack = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?track/([A-Za-z0-9]+)`)
	reSpotifyAlbum = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?album/([A-Za-z0-9]+)`)
	reSpotifyURI   = regexp.MustCompile(`spotify:track:([A-Za-z0-9]+)`)

	// Cleaning patterns
	reParenBlock       = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)
	reFeat             = regexp.MustCompile(`(?i)\s*(feat\.?|featuring)\s+[-–—·,]*[^-–—·,]+`)
	reMidDotSep        = regexp.MustCompile(`\s*[·•]\s*`)
	reDash             = regexp.MustCompile(`\s*[-–—]\s*`)
	reAppleMusicSuffix = regexp.MustCompile(`(?i)\s+(?:on|в|у|na|en|sur|su|auf|no|em|di|de|a)\s+apple\s*music`)
	rePlatformNames    = regexp.MustCompile(`(?i)\b(?:apple\s*music|spotify|youtube(?:\s*music)?)\b`)

	// YouTube artist cleaning
	reVEVO      = regexp.MustCompile(`(?i)VEVO$`)
	reOfficial  = regexp.MustCompile(`(?i)Official$`)
	reCamelCase = regexp.MustCompile(`([a-z])([A-Z])`)

	// Spotify embed extraction
	reNextData = regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">(.+?)</script>`)

	fancyQuotes = map[string]string{
		"\u00AB": "", // «
		"\u00BB": "", // »
		"\u201C": "", // "
		"\u201D": "", // "
		"\u201E": "", // „
		"\u2019": "", // '
		"\u2018": "", // '
	}
)

// ---------------------------
// Utility Classes
// ---------------------------

type CacheEntry struct {
	Value     string
	ExpiresAt time.Time
}

type SimpleCache struct {
	mu    sync.RWMutex
	cache map[string]CacheEntry
}

func NewSimpleCache() *SimpleCache {
	return &SimpleCache{
		cache: make(map[string]CacheEntry),
	}
}

func (c *SimpleCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		return "", false
	}
	return entry.Value, true
}

func (c *SimpleCache) Set(key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = CacheEntry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}
}

type Semaphore struct {
	sem chan struct{}
}

func NewSemaphore(max int) *Semaphore {
	return &Semaphore{
		sem: make(chan struct{}, max),
	}
}

func (s *Semaphore) Acquire() {
	s.sem <- struct{}{}
}

func (s *Semaphore) Release() {
	<-s.sem
}

func (s *Semaphore) Run(fn func()) {
	s.Acquire()
	defer s.Release()
	fn()
}

// ---------------------------
// Bot Lifecycle
// ---------------------------

// NewLogger creates a new zap logger
func NewLogger(debug bool) (*zap.Logger, error) {
	if debug {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// createCircuitBreaker creates a circuit breaker with the given name
func createCircuitBreaker(name string, config *Config, logger *zap.Logger) *gobreaker.CircuitBreaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: uint32(config.CircuitBreakerMaxFails),
		Interval:    time.Minute,
		Timeout:     config.CircuitBreakerTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 3 && failureRatio >= 0.6
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			if logger != nil {
				logger.Info("Circuit breaker state changed",
					zap.String("service", name),
					zap.String("from", from.String()),
					zap.String("to", to.String()))
			}
		},
	}
	return gobreaker.NewCircuitBreaker(settings)
}

// createCollyCollector creates a Colly collector for web scraping
func createCollyCollector(config *Config) *colly.Collector {
	collector := colly.NewCollector(
		colly.UserAgent(USER_AGENT),
		colly.Async(true),
	)

	collector.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: config.MaxConcurrentFetches,
		RandomDelay: 150 * time.Millisecond,
	})

	collector.SetRequestTimeout(config.RequestTimeout)
	return collector
}

func NewMusicBot(config *Config) (*MusicBot, error) {
	// Initialize logger
	logger, err := NewLogger(config.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	bot, err := tgbotapi.NewBotAPI(config.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}
	bot.Debug = config.Debug
	logger.Info("Authorized as Telegram bot",
		zap.String("username", bot.Self.UserName),
		zap.Bool("debug", bot.Debug))

	httpClient := &http.Client{
		Timeout: config.RequestTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}

	// Setup Colly collector
	collector := createCollyCollector(config)

	return &MusicBot{
		config:            config,
		telegramBot:       bot,
		httpClient:        httpClient,
		collector:         collector,
		logger:            logger,
		queryCache:        NewSimpleCache(),
		urlCache:          NewSimpleCache(),
		negativeCache:     NewSimpleCache(),
		spotifyBreaker:    createCircuitBreaker("Spotify", config, logger),
		youtubeBreaker:    createCircuitBreaker("YouTube", config, logger),
		appleMusicBreaker: createCircuitBreaker("AppleMusic", config, logger),
		fetchSem:          NewSemaphore(config.MaxConcurrentFetches),
		msgSem:            NewSemaphore(config.MaxConcurrentMessages),
	}, nil
}

func (mb *MusicBot) Run(ctx context.Context) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := mb.telegramBot.GetUpdatesChan(u)

	log.Println("🎵 Music Link Converter Bot is running…")
	log.Println("📡 No API keys required - using web scraping!")

	for {
		select {
		case <-ctx.Done():
			log.Println("👋 Shutting down bot loop gracefully…")
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Message == nil {
				continue
			}
			go func(m *tgbotapi.Message) {
				mb.msgSem.Run(func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("⚠️  Panic in handleMessage: %v", r)
						}
					}()
					mb.handleMessage(m)
				})
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

	// Enrich missing artist using other platform results
	if strings.TrimSpace(info.Artist) == "" {
		if links.AppleMusic != "" {
			if si := mb.getAppleMusicInfo(links.AppleMusic); si != nil && strings.TrimSpace(si.Artist) != "" {
				info.Artist = si.Artist
			}
		}
		if strings.TrimSpace(info.Artist) == "" && links.YouTubeMusic != "" {
			if si := mb.getYouTubeInfo(links.YouTubeMusic, "YouTube Music"); si != nil && strings.TrimSpace(si.Artist) != "" {
				info.Artist = si.Artist
			}
		}
	}

	if links.Spotify == "" && links.YouTubeMusic == "" && links.AppleMusic == "" {
		mb.sendReply(chatID, replyTo, md2("😕 I couldn't find matches on other platforms. It might be a regional or rare release."))
		return
	}

	mb.sendLinks(chatID, replyTo, info, links)
}

// ---------------------------
// Telegram Helpers
// ---------------------------

func (mb *MusicBot) sendReply(chatID int64, replyToID int, text string) {
	for _, chunk := range chunkText(text, 3500) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		msg.ReplyToMessageID = replyToID
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		msg.DisableWebPagePreview = true

		if _, err := mb.telegramBot.Send(msg); err != nil {
			if mb.config.Debug {
				log.Printf("⚠️  Error sending message with MarkdownV2: %v", err)
			}
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
		cut := strings.LastIndex(s[:max], "\n")
		if cut < max/2 {
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
		log.Printf("⚠️  Error sending links: %v", err)
	}
}

func md2(s string) string {
	return tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, s)
}

// ---------------------------
// HTTP Helpers
// ---------------------------

func (mb *MusicBot) fetch(target string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < mb.config.RetryAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}
		req.Header.Set("User-Agent", USER_AGENT)
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

		if attempt < mb.config.RetryAttempts-1 {
			delay := mb.config.RetryMinDelay + time.Duration(rand.Intn(int(mb.config.RetryMaxDelay-mb.config.RetryMinDelay)))
			if mb.config.Debug {
				log.Printf("🔄 Retry %d/%d for %s after %v", attempt+1, mb.config.RetryAttempts, target, delay)
			}
			time.Sleep(delay)
		}
	}
	return nil, lastErr
}

func firstURL(s string) string {
	m := reFirstURL.FindString(s)
	return strings.TrimRight(m, ".,);!?]}>\"'")
}

// ---------------------------
// Platform Extraction
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
// Spotify Info (Improved)
// ---------------------------

func (mb *MusicBot) getSpotifyInfo(spotifyURL string) *SongInfo {
	// Check cache first
	if v, ok := mb.urlCache.Get("info:" + spotifyURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si
		}
	}

	// Check negative cache
	if _, ok := mb.negativeCache.Get("info:" + spotifyURL); ok {
		if mb.config.Debug {
			mb.logger.Debug("Spotify URL in negative cache, skipping", zap.String("url", spotifyURL))
		}
		return nil
	}

	if !reSpotifyTrack.MatchString(spotifyURL) {
		return nil
	}

	// Use circuit breaker
	result, err := mb.spotifyBreaker.Execute(func() (interface{}, error) {
		return mb.extractSpotifyWithFallbacks(spotifyURL)
	})

	if err != nil {
		mb.logger.Error("Spotify circuit breaker error", zap.Error(err))
		mb.negativeCache.Set("info:"+spotifyURL, "failed", mb.config.NegativeCacheTTL)
		return nil
	}

	if result == nil {
		mb.negativeCache.Set("info:"+spotifyURL, "failed", mb.config.NegativeCacheTTL)
		return nil
	}

	si := result.(*SongInfo)
	// Cache successful result
	if b, err := json.Marshal(si); err == nil {
		mb.urlCache.Set("info:"+spotifyURL, string(b), mb.config.CacheTTL)
	}
	return si
}

// extractSpotifyWithFallbacks tries all 6 extraction methods for Spotify
func (mb *MusicBot) extractSpotifyWithFallbacks(spotifyURL string) (*SongInfo, error) {
	// Layer 1: Try current OEmbed API
	if info := mb.trySpotifyOEmbed(spotifyURL); info != nil {
		return info, nil
	}

	// Layer 2: Try legacy OEmbed endpoint
	if info := mb.trySpotifyLegacyOEmbed(spotifyURL); info != nil {
		return info, nil
	}

	// Layer 3: Try __NEXT_DATA__ extraction
	if info := mb.extractFromSpotifyEmbed(spotifyURL); info != nil && info.Artist != "" {
		return info, nil
	}

	// Layer 4: Try JSON-LD structured data
	if info := mb.trySpotifyJSONLD(spotifyURL); info != nil {
		return info, nil
	}

	// Layer 5: Try OpenGraph meta tags
	if info := mb.fallbackSpotifyScrape(spotifyURL); info != nil {
		return info, nil
	}

	// Layer 6: Try text mirror fallback
	if info := mb.trySpotifyTextMirror(spotifyURL); info != nil {
		return info, nil
	}

	return nil, fmt.Errorf("all Spotify extraction methods failed")
}

// trySpotifyOEmbed attempts standard OEmbed API (Layer 1)
func (mb *MusicBot) trySpotifyOEmbed(spotifyURL string) *SongInfo {
	oembedURL := fmt.Sprintf("https://open.spotify.com/oembed?url=%s", url.QueryEscape(spotifyURL))
	resp, err := mb.fetch(oembedURL)
	if err != nil {
		if mb.config.Debug {
			mb.logger.Debug("Spotify OEmbed error", zap.Error(err))
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		if mb.config.Debug {
			mb.logger.Debug("Spotify OEmbed decode error", zap.Error(err))
		}
		return nil
	}

	if mb.config.Debug {
		log.Printf("🔍 [Spotify] OEmbed response: title=%q artist=%q desc=%q", oembed.Title, oembed.AuthorName, oembed.Description)
	}

	title := strings.TrimSpace(oembed.Title)
	artist := strings.TrimSpace(oembed.AuthorName)
	title = strings.TrimSuffix(title, " | Spotify")

	// Parse description for missing fields
	if desc := strings.TrimSpace(oembed.Description); desc != "" && (title == "" || artist == "") {
		parts := reMidDotSep.Split(strings.ReplaceAll(desc, "\u00A0", " "), -1)

		if artist == "" && len(parts) >= 2 {
			p0, p1 := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch {
			case title != "" && strings.EqualFold(p0, title):
				artist = p1
			case title != "" && strings.EqualFold(p1, title):
				artist = p0
			case isAlbumish(p0) && !isAlbumish(p1):
				artist = p1
			case isAlbumish(p1) && !isAlbumish(p0):
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
			case !isAlbumish(p0) && isAlbumish(p1):
				title = p0
			case !isAlbumish(p1) && isAlbumish(p0):
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

	// NEW: Try embed page extraction if artist missing
	if artist == "" {
		if mb.config.Debug {
			log.Printf("🔍 [Spotify] Artist missing from OEmbed, trying embed page")
		}
		if embedInfo := mb.extractFromSpotifyEmbed(spotifyURL); embedInfo != nil && embedInfo.Artist != "" {
			artist = embedInfo.Artist
			if title == "" && embedInfo.Title != "" {
				title = embedInfo.Title
			}
		}
	}

	// Fallback to scraping if parsing looks wrong
	if artist == "" || isAlbumish(artist) ||
		(looksLikeArtistList(title) && !looksLikeArtistList(artist)) ||
		strings.EqualFold(title, artist) {
		if mb.config.Debug {
			log.Printf("🔍 [Spotify] Artist missing or invalid (%q), falling back to scrape", artist)
		}
		if scraped := mb.fallbackSpotifyScrape(spotifyURL); scraped != nil {
			if scraped.Title != "" && scraped.Artist != "" {
				if b, err := json.Marshal(scraped); err == nil {
					mb.urlCache.Set("info:"+spotifyURL, string(b), mb.config.CacheTTL)
				}
				return scraped
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
		if scraped := mb.fallbackSpotifyScrape(spotifyURL); scraped != nil {
			if b, err := json.Marshal(scraped); err == nil {
				mb.urlCache.Set("info:"+spotifyURL, string(b), mb.config.CacheTTL)
			}
			return scraped
		}
	}

	si := &SongInfo{Title: title, Artist: artist, Platform: "Spotify", OriginalURL: spotifyURL}
	return si
}

// trySpotifyLegacyOEmbed attempts legacy OEmbed endpoint (Layer 2)
func (mb *MusicBot) trySpotifyLegacyOEmbed(spotifyURL string) *SongInfo {
	// Some regions/locales may use different oEmbed endpoint
	legacyURL := fmt.Sprintf("https://embed.spotify.com/oembed/?url=%s", url.QueryEscape(spotifyURL))
	resp, err := mb.fetch(legacyURL)
	if err != nil {
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	var oembed OEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&oembed); err != nil {
		return nil
	}

	title := strings.TrimSpace(oembed.Title)
	artist := strings.TrimSpace(oembed.AuthorName)

	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}
	}

	return nil
}

// trySpotifyJSONLD attempts JSON-LD structured data extraction (Layer 4)
func (mb *MusicBot) trySpotifyJSONLD(spotifyURL string) *SongInfo {
	resp, err := mb.fetch(spotifyURL)
	if err != nil {
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
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
		}
	}

	return nil
}

// trySpotifyTextMirror attempts text mirror fallback (Layer 6)
func (mb *MusicBot) trySpotifyTextMirror(spotifyURL string) *SongInfo {
	resp, err := mb.fetchViaTextMirror(spotifyURL)
	if err != nil {
		if mb.config.Debug {
			mb.logger.Debug("Text mirror fetch failed", zap.Error(err))
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	ogTitle := doc.Find("meta[property='og:title']").AttrOr("content", "")
	ogDesc := doc.Find("meta[property='og:description']").AttrOr("content", "")

	title, artist := splitFromOgTitle(ogTitle, ogDesc)
	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}
	}

	return nil
}

// Extract from Spotify embed page - __NEXT_DATA__ extraction (Layer 3)
func (mb *MusicBot) extractFromSpotifyEmbed(spotifyURL string) *SongInfo {
	match := reSpotifyTrack.FindStringSubmatch(spotifyURL)
	if len(match) < 2 {
		return nil
	}

	trackID := match[1]
	embedURL := fmt.Sprintf("https://open.spotify.com/embed/track/%s", trackID)

	resp, err := mb.fetch(embedURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [Spotify Embed] Fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	// Find __NEXT_DATA__ JSON
	matches := reNextData.FindSubmatch(body)
	if len(matches) < 2 {
		return nil
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
		if mb.config.Debug {
			log.Printf("🔍 [Spotify Embed] JSON parse error: %v", err)
		}
		return nil
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

	if mb.config.Debug {
		log.Printf("🔍 [Spotify Embed] Extracted: title=%q artist=%q", title, artist)
	}

	if title != "" && artist != "" {
		return &SongInfo{
			Title:       title,
			Artist:      artist,
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}
	}

	return nil
}

func (mb *MusicBot) fallbackSpotifyScrape(spotifyURL string) *SongInfo {
	resp, err := mb.fetch(spotifyURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [Spotify scrape] Fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	if mb.config.Debug {
		preview := string(body)
		if len(preview) > 500 {
			preview = preview[:500]
		}
		log.Printf("🔍 [Spotify scrape] HTML length: %d", len(body))
		log.Printf("🔍 [Spotify scrape] HTML preview: %s", preview)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [Spotify scrape] Parse error: %v", err)
		}
		return nil
	}

	ogTitle := doc.Find("meta[property='og:title']").AttrOr("content", "")
	ogDesc := doc.Find("meta[property='og:description']").AttrOr("content", "")

	if mb.config.Debug {
		log.Printf("🔍 [Spotify scrape] og:title: %q", ogTitle)
		log.Printf("🔍 [Spotify scrape] og:description: %q", ogDesc)
		log.Printf("🔍 [Spotify scrape] All meta tags: %d", doc.Find("meta[property^='og:']").Length())
	}

	// Try robust split from og:title
	ti, ar := splitFromOgTitle(ogTitle, ogDesc)
	if mb.config.Debug {
		log.Printf("🔍 [Spotify scrape] splitFromOgTitle result: title=%q artist=%q", ti, ar)
	}
	if ti != "" && ar != "" {
		return &SongInfo{Title: ti, Artist: ar, Platform: "Spotify", OriginalURL: spotifyURL}
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
		}
	}

	if strings.Contains(cleanTitle, " - ") {
		parts := strings.SplitN(cleanTitle, " - ", 2)
		return &SongInfo{
			Title:       strings.TrimSpace(parts[0]),
			Artist:      strings.TrimSpace(parts[1]),
			Platform:    "Spotify",
			OriginalURL: spotifyURL,
		}
	}

	// Last resort: salvage artist from description
	if ogDesc != "" {
		parts := reMidDotSep.Split(strings.ReplaceAll(ogDesc, "\u00A0", " "), -1)
		if len(parts) > 0 {
			artistGuess := strings.TrimSpace(parts[0])
			if artistGuess != "" {
				return &SongInfo{
					Title:       cleanTitle,
					Artist:      artistGuess,
					Platform:    "Spotify",
					OriginalURL: spotifyURL,
				}
			}
		}
	}

	return nil
}

// ---------------------------
// YouTube Info (Improved)
// ---------------------------

func (mb *MusicBot) getYouTubeInfo(youtubeURL string, platform string) *SongInfo {
	if v, ok := mb.urlCache.Get("info:" + youtubeURL); ok {
		var si SongInfo
		if json.Unmarshal([]byte(v), &si) == nil {
			return &si
		}
	}

	// Try OEmbed first
	oembedURL := fmt.Sprintf("https://www.youtube.com/oembed?format=json&url=%s", url.QueryEscape(youtubeURL))
	resp, err := mb.fetch(oembedURL)
	if err == nil {
		defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

		var data struct {
			Title      string `json:"title"`
			AuthorName string `json:"author_name"`
		}

		if json.NewDecoder(resp.Body).Decode(&data) == nil && data.Title != "" {
			if mb.config.Debug {
				log.Printf("🔍 [%s] OEmbed response: title=%q author=%q", platform, data.Title, data.AuthorName)
			}

			title, artist := cleanYouTubeInfo(data.Title, data.AuthorName)

			if mb.config.Debug {
				log.Printf("🔍 [%s] Extracted from OEmbed: title=%q artist=%q", platform, title, artist)
			}

			si := &SongInfo{
				Title:       title,
				Artist:      artist,
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
			if b, err := json.Marshal(si); err == nil {
				mb.urlCache.Set("info:"+youtubeURL, string(b), mb.config.CacheTTL)
			}
			return si
		}
	}

	// Fallback to scraping
	resp, err = mb.fetch(youtubeURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [%s] Fetch error: %v", platform, err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
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
			title, artist := cleanYouTubeInfo(data.VideoDetails.Title, data.VideoDetails.Author)

			si := &SongInfo{
				Title:       title,
				Artist:      artist,
				Platform:    platform,
				OriginalURL: youtubeURL,
			}
			if b, err := json.Marshal(si); err == nil {
				mb.urlCache.Set("info:"+youtubeURL, string(b), mb.config.CacheTTL)
			}
			return si
		}
	}

	// Fallback to og:title
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil
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
			mb.urlCache.Set("info:"+youtubeURL, string(b), mb.config.CacheTTL)
		}
		return si
	}

	if title != "" {
		si := &SongInfo{
			Title:       strings.TrimSpace(title),
			Artist:      "",
			Platform:    platform,
			OriginalURL: youtubeURL,
		}
		if b, err := json.Marshal(si); err == nil {
			mb.urlCache.Set("info:"+youtubeURL, string(b), mb.config.CacheTTL)
		}
		return si
	}

	return nil
}

// NEW: Clean YouTube artist info (like JS version)
func cleanYouTubeInfo(title, artist string) (string, string) {
	// Remove Topic suffix
	artist = strings.TrimSuffix(artist, " - Topic")

	// Remove VEVO/Official
	artist = reVEVO.ReplaceAllString(artist, "")
	artist = reOfficial.ReplaceAllString(artist, "")
	artist = strings.TrimSpace(artist)

	// Fix camelCase (e.g., "TaylorSwift" → "Taylor Swift")
	artist = reCamelCase.ReplaceAllString(artist, "$1 $2")

	// Remove artist prefix from title
	if artist != "" {
		prefix := strings.ToLower(artist) + " - "
		if strings.HasPrefix(strings.ToLower(title), prefix) {
			title = title[len(prefix):]
		}
	}

	return strings.TrimSpace(title), strings.TrimSpace(artist)
}

// ---------------------------
// Apple Music Info (Improved)
// ---------------------------

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
			log.Printf("🔍 [Apple Music] Fetch error: %v", err)
		}
		return nil
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [Apple Music] Parse error: %v", err)
		}
		return nil
	}

	title, artist := extractAppleLD(doc)

	if title == "" {
		title = doc.Find("meta[property='og:title']").AttrOr("content", "")
	}

	// Parse og:title if needed
	if title != "" && artist == "" {
		cleanOgTitle := title
		for fancy, plain := range fancyQuotes {
			cleanOgTitle = strings.ReplaceAll(cleanOgTitle, fancy, plain)
		}

		// Pattern 1: "Title by Artist on Apple Music"
		if strings.Contains(cleanOgTitle, " by ") && strings.Contains(cleanOgTitle, " on Apple Music") {
			parts := strings.SplitN(cleanOgTitle, " by ", 2)
			title = strings.TrimSpace(parts[0])
			artist = strings.TrimSpace(strings.ReplaceAll(parts[1], " on Apple Music", ""))
		} else if strings.Contains(cleanOgTitle, ",") && reAppleMusicSuffix.MatchString(cleanOgTitle) {
			parts := strings.SplitN(cleanOgTitle, ",", 2)
			if len(parts) == 2 {
				title = strings.TrimSpace(parts[0])
				artist = strings.TrimSpace(reAppleMusicSuffix.ReplaceAllString(parts[1], ""))
			}
		}
	}

	// Clean up
	title, artist = cleanTitleArtist(title, artist)

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
		return nil
	}

	si := &SongInfo{
		Title:       title,
		Artist:      artist,
		Platform:    "Apple Music",
		OriginalURL: appleURL,
	}
	if b, err := json.Marshal(si); err == nil {
		mb.urlCache.Set("info:"+appleURL, string(b), mb.config.CacheTTL)
	}
	return si
}

// Improved Apple Music JSON-LD extraction (like JS version)
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

				// NEW: Check nested audio object for MusicRecording
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

// ---------------------------
// Search
// ---------------------------

func (mb *MusicBot) findOnAllPlatforms(info *SongInfo) *MusicLinks {
	links := &MusicLinks{}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		if info.Platform == "Spotify" {
			links.Spotify = info.OriginalURL
		} else {
			links.Spotify = mb.searchSpotify(info)
		}
	}()

	go func() {
		defer wg.Done()
		if info.Platform == "YouTube Music" {
			links.YouTubeMusic = info.OriginalURL
		} else if info.Platform == "YouTube" {
			if u := toYouTubeMusicURL(info.OriginalURL); u != "" {
				links.YouTubeMusic = u
			} else {
				links.YouTubeMusic = mb.searchYouTube(info)
			}
		} else {
			links.YouTubeMusic = mb.searchYouTube(info)
		}
	}()

	go func() {
		defer wg.Done()
		if info.Platform == "Apple Music" {
			links.AppleMusic = info.OriginalURL
		} else {
			links.AppleMusic = mb.searchAppleMusic(info)
		}
	}()

	wg.Wait()
	return links
}

// ---------------------------
// Spotify Search (Improved with Colly)
// ---------------------------

func (mb *MusicBot) searchSpotify(info *SongInfo) string {
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("sp:" + key); ok {
		return v
	}

	query := key
	searchURL := fmt.Sprintf("https://open.spotify.com/search/%s", url.QueryEscape(query))

	// Try with standard HTTP first
	if resp, err := mb.fetch(searchURL); err == nil {
		func() {
			defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err == nil {
				// Look for spotify:track: URI
				if m := reSpotifyURI.FindSubmatch(body); len(m) == 2 {
					u := fmt.Sprintf("https://open.spotify.com/track/%s", m[1])
					mb.queryCache.Set("sp:"+key, u, mb.config.CacheTTL)
					return
				}

				// Look for track links
				if m := reSpotifyTrack.FindSubmatch(body); len(m) == 2 {
					u := canonicalSpotifyTrack(string(body))
					if u != "" {
						mb.queryCache.Set("sp:"+key, u, mb.config.CacheTTL)
						return
					}
				}
			}
		}()
	}

	// DuckDuckGo fallback
	if u := mb.searchSpotifyViaDDG(info.Artist, info.Title); u != "" {
		mb.queryCache.Set("sp:"+key, u, mb.config.CacheTTL)
		return u
	}

	return searchURL
}

func (mb *MusicBot) searchSpotifyViaDDG(artist, title string) string {
	t := normalizeForMatch(title)
	a := normalizeForMatch(artist)

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

	if result := mb.searchDDGPage(ddgURL); result != "" {
		return result
	}

	// Pass 2: Loose search
	if t == "" && a == "" {
		return ""
	}

	q2 := fmt.Sprintf("site:open.spotify.com/track %s %s", t, a)
	ddgURL2 := fmt.Sprintf("https://duckduckgo.com/html/?q=%s", url.QueryEscape(strings.TrimSpace(q2)))

	return mb.searchDDGPage(ddgURL2)
}

func (mb *MusicBot) searchDDGPage(ddgURL string) string {
	resp, err := mb.fetch(ddgURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [DDG] Fetch error: %v", err)
		}
		return ""
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return ""
	}

	var result string
	doc.Find("a").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		href, exists := s.Attr("href")
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
			if track := mb.tryAlbumToTrack(resolved); track != "" {
				result = track
				return false
			}
		}

		return true
	})

	return result
}

func (mb *MusicBot) tryAlbumToTrack(albumURL string) string {
	resp, err := mb.fetch(albumURL)
	if err != nil {
		return ""
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	// Look for spotify:track: URI
	if m := reSpotifyURI.FindSubmatch(body); len(m) == 2 {
		return fmt.Sprintf("https://open.spotify.com/track/%s", m[1])
	}

	// Look for track links
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}

	var track string
	doc.Find("a").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		href, _ := s.Attr("href")
		if href != "" {
			if u := canonicalSpotifyTrack(href); u != "" {
				track = u
				return false
			}
		}
		return true
	})

	return track
}

// ---------------------------
// YouTube Search
// ---------------------------

func (mb *MusicBot) searchYouTube(info *SongInfo) string {
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("yt:" + key); ok {
		return v
	}

	query := key

	// Try YouTube Music search
	ytMusicURL := fmt.Sprintf("https://music.youtube.com/search?q=%s", url.QueryEscape(query))
	if videoID := mb.searchYouTubeViaPage(ytMusicURL, info); videoID != "" {
		u := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
		mb.queryCache.Set("yt:"+key, u, mb.config.CacheTTL)
		return u
	}

	// Fallback to regular YouTube
	ytURL := fmt.Sprintf("https://www.youtube.com/results?search_query=%s", url.QueryEscape(query))
	if videoID := mb.searchYouTubeViaPage(ytURL, info); videoID != "" {
		u := fmt.Sprintf("https://music.youtube.com/watch?v=%s", videoID)
		mb.queryCache.Set("yt:"+key, u, mb.config.CacheTTL)
		return u
	}

	return ytMusicURL
}

func (mb *MusicBot) searchYouTubeViaPage(searchURL string, info *SongInfo) string {
	resp, err := mb.fetch(searchURL)
	if err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [YouTube] Search fetch error: %v", err)
		}
		return ""
	}
	defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	// Find ytInitialData
	re := regexp.MustCompile(`ytInitialData\s*=\s*(\{.+?\});`)
	matches := re.FindSubmatch(body)
	if len(matches) < 2 {
		return ""
	}

	var data interface{}
	if err := json.Unmarshal(matches[1], &data); err != nil {
		if mb.config.Debug {
			log.Printf("🔍 [YouTube] JSON parse error: %v", err)
		}
		return ""
	}

	candidates := collectYouTubeCandidates(data)
	if len(candidates) == 0 {
		return ""
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
		return bestID
	}

	if len(candidates) > 0 {
		return candidates[0].ID
	}

	return ""
}

func collectYouTubeCandidates(v interface{}) []YouTubeCandidate {
	var result []YouTubeCandidate
	collectYouTubeCandidatesRecursive(v, &result)
	return result
}

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

func scoreYouTubeCandidate(c YouTubeCandidate, wantArtist, wantTitle string) int {
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

// ---------------------------
// Apple Music Search
// ---------------------------

func (mb *MusicBot) searchAppleMusic(info *SongInfo) string {
	key := normalizeQuery(info.Artist, info.Title)
	if v, ok := mb.queryCache.Get("am:" + key); ok {
		return v
	}

	query := key
	searchURL := fmt.Sprintf("https://music.apple.com/search?term=%s", url.QueryEscape(query))

	// Try iTunes Search API
	country := "US"
	apiURL := fmt.Sprintf("https://itunes.apple.com/search?term=%s&media=music&entity=song&limit=10&country=%s",
		url.QueryEscape(query), country)

	resp, err := mb.fetch(apiURL)
	if err == nil {
		defer func() { io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

		var data struct {
			Results []struct {
				TrackViewURL string `json:"trackViewUrl"`
				TrackName    string `json:"trackName"`
				ArtistName   string `json:"artistName"`
			} `json:"results"`
		}

		if json.NewDecoder(resp.Body).Decode(&data) == nil && len(data.Results) > 0 {
			wantT := normalizeForMatch(info.Title)
			wantA := normalizeForMatch(info.Artist)

			bestScore := -999
			bestURL := ""

			for _, result := range data.Results {
				if result.TrackViewURL == "" {
					continue
				}

				t := normalizeForMatch(result.TrackName)
				a := normalizeForMatch(result.ArtistName)

				score := 0
				if wantT != "" && strings.Contains(t, wantT) {
					score += 3
				}
				if wantA != "" && strings.Contains(a, wantA) {
					score += 3
				}

				if score > bestScore {
					bestScore = score
					bestURL = strings.Replace(result.TrackViewURL, "itunes.apple.com", "music.apple.com", 1)
				}
			}

			if bestScore >= 3 && bestURL != "" {
				mb.queryCache.Set("am:"+key, bestURL, mb.config.CacheTTL)
				return bestURL
			}
		}
	}

	return searchURL
}

// ---------------------------
// Helper Functions
// ---------------------------

func cleanPlatformNoise(s string) string {
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = reAppleMusicSuffix.ReplaceAllString(s, "")
	s = rePlatformNames.ReplaceAllString(s, "")
	for fancy, plain := range fancyQuotes {
		s = strings.ReplaceAll(s, fancy, plain)
	}
	return strings.TrimSpace(s)
}

func cleanTitleArtist(t, a string) (string, string) {
	return cleanPlatformNoise(t), cleanPlatformNoise(a)
}

func isAlbumish(s string) bool {
	ls := strings.ToLower(strings.TrimSpace(s))
	if ls == "" {
		return false
	}
	hints := []string{"original soundtrack", "soundtrack", "ost", "score", "music from", "season ", " vol.", " volume ", ":"}
	for _, h := range hints {
		if strings.Contains(ls, h) {
			return true
		}
	}
	return false
}

func looksLikeArtistList(s string) bool {
	ls := strings.ToLower(strings.TrimSpace(s))
	if ls == "" {
		return false
	}
	if (strings.Contains(ls, ",") || strings.Contains(ls, " & ") || strings.Contains(ls, " and ") ||
		strings.Contains(ls, " feat") || strings.Contains(ls, " featuring ")) &&
		!strings.Contains(ls, ":") && !strings.Contains(ls, "-") {
		return true
	}
	return false
}

func normalizeQuery(artist, title string) string {
	clean := func(s string) string {
		s = reParenBlock.ReplaceAllString(s, "")
		s = reFeat.ReplaceAllString(s, "")

		ls := strings.ToLower(s)
		ls = strings.ReplaceAll(ls, "\u00A0", " ")
		ls = reAppleMusicSuffix.ReplaceAllString(ls, "")
		ls = rePlatformNames.ReplaceAllString(ls, "")

		for fancy, plain := range fancyQuotes {
			ls = strings.ReplaceAll(ls, fancy, plain)
		}

		repls := []string{
			" - single", "", " - ep", "", " - album", "",
			" – single", "", " – ep", "",
			" remastered", "", " - remaster", "", " remaster", "",
			" - radio edit", "", " radio edit", "",
			" official video", "", " lyric video", "", " lyrics", "",
		}
		for i := 0; i < len(repls); i += 2 {
			ls = strings.ReplaceAll(ls, repls[i], repls[i+1])
		}

		return strings.Join(strings.Fields(ls), " ")
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
		"!", " ", "?", " ", "/", " ", "&", " and ", "'", " ", "'", " ",
	).Replace(s)

	for fancy, plain := range fancyQuotes {
		s = strings.ReplaceAll(s, fancy, plain)
	}

	s = rePlatformNames.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

func splitFromOgTitle(ogTitle, ogDesc string) (string, string) {
	if ogTitle == "" {
		return "", ""
	}

	t := strings.TrimSuffix(ogTitle, " | Spotify")
	t = strings.ReplaceAll(t, "\u00A0", " ")
	t = strings.TrimSpace(t)

	// Case 1: "... by ..."
	if strings.Contains(strings.ToLower(t), " by ") {
		idx := strings.Index(strings.ToLower(t), " by ")
		return strings.TrimSpace(t[:idx]), strings.TrimSpace(t[idx+4:])
	}

	// Case 2: Dash variants
	parts := reDash.Split(t, 2)
	if len(parts) != 2 {
		return "", ""
	}

	left := strings.TrimSpace(parts[0])
	right := strings.TrimSpace(parts[1])

	// Derive hint from description
	var hintArtist string
	if ogDesc != "" {
		descParts := reMidDotSep.Split(strings.ReplaceAll(ogDesc, "\u00A0", " "), -1)
		if len(descParts) >= 1 {
			hintArtist = strings.TrimSpace(descParts[0])
		}
	}

	ln := normalizeForMatch(left)
	rn := normalizeForMatch(right)
	an := normalizeForMatch(hintArtist)

	// Match hint
	if an != "" {
		if ln == an {
			return right, left // Artist — Title
		}
		if rn == an {
			return left, right // Title — Artist
		}
	}

	// Heuristic
	if looksLikeArtistList(right) {
		return left, right
	}
	if looksLikeArtistList(left) {
		return right, left
	}

	// Default: Title — Artist
	return left, right
}

func canonicalSpotifyTrack(u string) string {
	if m := reSpotifyTrack.FindStringSubmatch(u); len(m) == 2 {
		return fmt.Sprintf("https://open.spotify.com/track/%s", m[1])
	}
	return ""
}

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

func youtubeVideoIDFromURL(raw string) string {
	u, err := url.Parse(raw)
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

func toYouTubeMusicURL(raw string) string {
	if id := youtubeVideoIDFromURL(raw); id != "" {
		return fmt.Sprintf("https://music.youtube.com/watch?v=%s", id)
	}
	return ""
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "y":
		return true
	case "0", "false", "no", "n":
		return false
	default:
		return def
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------
// Fuzzy Matching
// ---------------------------

// calculateSimilarity returns a similarity score between 0 and 1
// Uses Levenshtein distance normalized by the longer string length
func calculateSimilarity(s1, s2 string) float64 {
	if s1 == "" || s2 == "" {
		return 0
	}

	// Normalize for comparison
	n1 := normalizeForMatch(s1)
	n2 := normalizeForMatch(s2)

	if n1 == n2 {
		return 1.0
	}

	distance := levenshtein.DistanceForStrings([]rune(n1), []rune(n2), levenshtein.DefaultOptions)
	maxLen := max(len(n1), len(n2))

	if maxLen == 0 {
		return 0
	}

	return 1.0 - float64(distance)/float64(maxLen)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// validateSearchResult checks if a search result matches the original query
// using fuzzy string matching. Returns true if confidence is above threshold.
func (mb *MusicBot) validateSearchResult(foundTitle, foundArtist, wantTitle, wantArtist string) bool {
	titleSim := calculateSimilarity(foundTitle, wantTitle)
	artistSim := calculateSimilarity(foundArtist, wantArtist)

	// Combined score: title is more important (60%) than artist (40%)
	combinedScore := (titleSim * 0.6) + (artistSim * 0.4)

	if mb.config.Debug {
		mb.logger.Debug("Fuzzy match validation",
			zap.String("found_title", foundTitle),
			zap.String("found_artist", foundArtist),
			zap.String("want_title", wantTitle),
			zap.String("want_artist", wantArtist),
			zap.Float64("title_similarity", titleSim),
			zap.Float64("artist_similarity", artistSim),
			zap.Float64("combined_score", combinedScore),
			zap.Float64("threshold", mb.config.FuzzyMatchThreshold))
	}

	return combinedScore >= mb.config.FuzzyMatchThreshold
}

// ---------------------------
// Text Mirror Fallback
// ---------------------------

// fetchViaTextMirror uses r.jina.ai to fetch pre-rendered content
// This helps with JavaScript-heavy pages that require rendering
func (mb *MusicBot) fetchViaTextMirror(targetURL string) (*http.Response, error) {
	if mb.config.TextMirrorURL == "" {
		return nil, fmt.Errorf("text mirror URL not configured")
	}

	mirrorURL := mb.config.TextMirrorURL + targetURL

	if mb.config.Debug {
		mb.logger.Debug("Fetching via text mirror",
			zap.String("original_url", targetURL),
			zap.String("mirror_url", mirrorURL))
	}

	return mb.fetch(mirrorURL)
}

// ---------------------------
// Main
// ---------------------------

func main() {
	rand.Seed(time.Now().UnixNano())

	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		log.Fatal("❌ TELEGRAM_BOT_TOKEN environment variable is required")
	}

	config := &Config{
		TelegramToken:          telegramToken,
		Debug:                  envBool("BOT_DEBUG", false),
		MaxConcurrentFetches:   8,
		MaxConcurrentMessages:  32,
		RequestTimeout:         12 * time.Second,
		RetryAttempts:          2,
		RetryMinDelay:          150 * time.Millisecond,
		RetryMaxDelay:          350 * time.Millisecond,
		CacheTTL:               24 * time.Hour,
		NegativeCacheTTL:       5 * time.Minute,  // Cache failures for shorter period
		FuzzyMatchThreshold:    0.7,              // 70% similarity threshold
		CircuitBreakerMaxFails: 5,                // Open after 5 failures
		CircuitBreakerTimeout:  60 * time.Second, // Reset after 60s
		TextMirrorURL:          "https://r.jina.ai/",
	}

	bot, err := NewMusicBot(config)
	if err != nil {
		log.Fatal(err)
	}

	log.Println("🎵 Music Link Converter Bot is running…")
	log.Println("📡 No API keys required - using web scraping!")

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	bot.Run(ctx)
}
