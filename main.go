// Telegram Music Searcher Bot - Monolith version
// Converts music links between Spotify, YouTube Music, and Apple Music
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
	"github.com/sony/gobreaker"
	"github.com/texttheater/golang-levenshtein/levenshtein"
	"go.uber.org/zap"
)

// ============================================================================
// CONSTANTS AND PATTERNS
// ============================================================================

const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var (
	// Text cleaning patterns
	reParenBlock       = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)
	reFeat             = regexp.MustCompile(`(?i)\s*(feat\.?|featuring)\s+[-–—·,]*[^-–—·,]+`)
	reMidDotSep        = regexp.MustCompile(`\s*[·•]\s*`)
	reDash             = regexp.MustCompile(`\s*[-–—]\s*`)
	reAppleMusicSuffix = regexp.MustCompile(`(?i)\s+(?:on|в|у|na|en|sur|su|auf|no|em|di|de|a)\s+apple\s*music`)
	rePlatformNames    = regexp.MustCompile(`(?i)\b(?:apple\s*music|spotify|youtube(?:\s*music)?)\b`)
	reVEVO             = regexp.MustCompile(`(?i)VEVO$`)
	reOfficial         = regexp.MustCompile(`(?i)Official$`)
	reCamelCase        = regexp.MustCompile(`([a-z])([A-Z])`)

	// Spotify patterns
	reSpotifyTrack = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?track/([A-Za-z0-9]+)`)
	reSpotifyAlbum = regexp.MustCompile(`(?:^|/)(?:intl-[a-z]{2}/)?album/([A-Za-z0-9]+)`)
	reSpotifyURI   = regexp.MustCompile(`spotify:track:([A-Za-z0-9]+)`)
	reNextData     = regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">(.+?)</script>`)

	// Search patterns
	reFirstURL = regexp.MustCompile(`https?://[^\s]+`)

	// Fancy quotes map for normalization
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

// ============================================================================
// CONFIG TYPES
// ============================================================================

// Config holds all configuration for the music bot
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
	NegativeCacheTTL       time.Duration
	FuzzyMatchThreshold    float64
	CircuitBreakerMaxFails int
	CircuitBreakerTimeout  time.Duration
	TextMirrorURL          string
}

// ============================================================================
// CONFIG FUNCTIONS
// ============================================================================

// LoadConfig creates a new Config from environment variables with sensible defaults
func LoadConfig() (*Config, error) {
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if telegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN environment variable is required")
	}

	cfg := &Config{
		TelegramToken:          telegramToken,
		Debug:                  envBool("BOT_DEBUG", false),
		MaxConcurrentFetches:   8,
		MaxConcurrentMessages:  32,
		RequestTimeout:         12 * time.Second,
		RetryAttempts:          2,
		RetryMinDelay:          150 * time.Millisecond,
		RetryMaxDelay:          350 * time.Millisecond,
		CacheTTL:               24 * time.Hour,
		NegativeCacheTTL:       5 * time.Minute,
		FuzzyMatchThreshold:    0.7,
		CircuitBreakerMaxFails: 5,
		CircuitBreakerTimeout:  60 * time.Second,
		TextMirrorURL:          "https://r.jina.ai/",
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return cfg, nil
}

// Validate checks if the configuration values are valid
func (c *Config) Validate() error {
	if c.TelegramToken == "" {
		return fmt.Errorf("telegram token cannot be empty")
	}

	if c.MaxConcurrentFetches < 1 {
		return fmt.Errorf("max concurrent fetches must be at least 1, got %d", c.MaxConcurrentFetches)
	}

	if c.MaxConcurrentMessages < 1 {
		return fmt.Errorf("max concurrent messages must be at least 1, got %d", c.MaxConcurrentMessages)
	}

	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive, got %v", c.RequestTimeout)
	}

	if c.RetryAttempts < 0 {
		return fmt.Errorf("retry attempts cannot be negative, got %d", c.RetryAttempts)
	}

	if c.FuzzyMatchThreshold < 0 || c.FuzzyMatchThreshold > 1 {
		return fmt.Errorf("fuzzy match threshold must be between 0 and 1, got %f", c.FuzzyMatchThreshold)
	}

	if c.CircuitBreakerMaxFails < 1 {
		return fmt.Errorf("circuit breaker max fails must be at least 1, got %d", c.CircuitBreakerMaxFails)
	}

	if c.CircuitBreakerTimeout <= 0 {
		return fmt.Errorf("circuit breaker timeout must be positive, got %v", c.CircuitBreakerTimeout)
	}

	return nil
}

// envBool parses a boolean from environment variable with a default value
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

// ============================================================================
// UTILITY FUNCTIONS - TEXT NORMALIZATION
// ============================================================================

// NormalizeQuery creates a normalized search query from artist and title
func NormalizeQuery(artist, title string) string {
	clean := func(s string) string {
		s = reParenBlock.ReplaceAllString(s, "")
		s = reFeat.ReplaceAllString(s, "")

		ls := strings.ToLower(s)
		ls = strings.ReplaceAll(ls, "\u00A0", " ")
		ls = reAppleMusicSuffix.ReplaceAllString(ls, "")
		ls = rePlatformNames.ReplaceAllString(ls, "")

		for fancy := range fancyQuotes {
			ls = strings.ReplaceAll(ls, fancy, "")
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

// NormalizeForMatch normalizes a string for fuzzy matching
func NormalizeForMatch(s string) string {
	s = strings.ToLower(s)
	s = reParenBlock.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"-", " ", "—", " ", "–", " ", "·", " ", ".", " ", ",", " ",
		"!", " ", "?", " ", "/", " ", "&", " and ", "'", " ", "'", " ",
	).Replace(s)

	for fancy := range fancyQuotes {
		s = strings.ReplaceAll(s, fancy, "")
	}

	s = rePlatformNames.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

// CleanPlatformNoise removes platform-specific noise from strings
func CleanPlatformNoise(s string) string {
	s = strings.ReplaceAll(s, "\u00A0", " ")
	s = reAppleMusicSuffix.ReplaceAllString(s, "")
	s = rePlatformNames.ReplaceAllString(s, "")
	for fancy := range fancyQuotes {
		s = strings.ReplaceAll(s, fancy, "")
	}
	return strings.TrimSpace(s)
}

// CleanTitleArtist cleans both title and artist strings
func CleanTitleArtist(title, artist string) (string, string) {
	return CleanPlatformNoise(title), CleanPlatformNoise(artist)
}

// CleanYouTubeInfo cleans YouTube title and artist information
func CleanYouTubeInfo(title, artist string) (string, string) {
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

// SplitFromOgTitle splits og:title into title and artist
func SplitFromOgTitle(ogTitle, ogDesc string) (string, string) {
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

	ln := NormalizeForMatch(left)
	rn := NormalizeForMatch(right)
	an := NormalizeForMatch(hintArtist)

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
	if LooksLikeArtistList(right) {
		return left, right
	}
	if LooksLikeArtistList(left) {
		return right, left
	}

	// Default: Title — Artist
	return left, right
}

// IsAlbumish checks if a string looks like an album name
func IsAlbumish(s string) bool {
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

// LooksLikeArtistList checks if a string looks like a list of artists
func LooksLikeArtistList(s string) bool {
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

// ============================================================================
// UTILITY FUNCTIONS - SIMILARITY
// ============================================================================

// CalculateSimilarity returns a similarity score between 0 and 1
func CalculateSimilarity(s1, s2 string) float64 {
	if s1 == "" || s2 == "" {
		return 0
	}

	n1 := NormalizeForMatch(s1)
	n2 := NormalizeForMatch(s2)

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

// ============================================================================
// CACHE IMPLEMENTATION
// ============================================================================

// Cache defines the interface for caching operations
type Cache interface {
	Get(key string) (string, bool)
	Set(key, value string, ttl time.Duration)
	StartCleanup(ctx context.Context, interval time.Duration)
}

// Entry represents a cached value with expiration
type Entry struct {
	Value     string
	ExpiresAt time.Time
}

// SimpleCache is a thread-safe in-memory cache with TTL support
type SimpleCache struct {
	mu    sync.RWMutex
	cache map[string]Entry
}

// NewCache creates a new SimpleCache instance
func NewCache() *SimpleCache {
	return &SimpleCache{
		cache: make(map[string]Entry),
	}
}

// Get retrieves a value from the cache
func (c *SimpleCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		return "", false
	}
	return entry.Value, true
}

// Set stores a value in the cache with the specified TTL
func (c *SimpleCache) Set(key, value string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache[key] = Entry{
		Value:     value,
		ExpiresAt: time.Now().Add(ttl),
	}
}

// StartCleanup starts a background goroutine that periodically removes expired entries
func (c *SimpleCache) StartCleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.cleanup()
		case <-ctx.Done():
			return
		}
	}
}

// cleanup removes all expired entries from the cache
func (c *SimpleCache) cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for k, v := range c.cache {
		if now.After(v.ExpiresAt) {
			delete(c.cache, k)
		}
	}
}

// Size returns the current number of entries in the cache
func (c *SimpleCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// ============================================================================
// HTTP CLIENT - CONFIG AND TYPES
// ============================================================================

// HTTPConfig holds HTTP client configuration
type HTTPConfig struct {
	Timeout       time.Duration
	RetryAttempts int
	RetryMinDelay time.Duration
	RetryMaxDelay time.Duration
	Debug         bool
}

// HTTPClient provides HTTP operations with retry logic
type HTTPClient struct {
	httpClient *http.Client
	config     *HTTPConfig
	logger     *zap.Logger
	rand       *rand.Rand
}

// NewHTTPClient creates a new HTTP client with the given configuration
func NewHTTPClient(config *HTTPConfig, logger *zap.Logger) *HTTPClient {
	httpClient := &http.Client{
		Timeout: config.Timeout,
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

	return &HTTPClient{
		httpClient: httpClient,
		config:     config,
		logger:     logger,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Fetch fetches a URL with retry logic
func (c *HTTPClient) Fetch(ctx context.Context, targetURL string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < c.config.RetryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
		if err != nil {
			return nil, fmt.Errorf("new request: %w", err)
		}

		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,uk;q=0.8,ru;q=0.7")
		req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		} else if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("status %d for %s", resp.StatusCode, targetURL)
		} else {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("status %d for %s", resp.StatusCode, targetURL)
		}

		if attempt < c.config.RetryAttempts-1 {
			delay := c.config.RetryMinDelay + time.Duration(c.rand.Intn(int(c.config.RetryMaxDelay-c.config.RetryMinDelay)))
			if c.config.Debug {
				c.logger.Debug("Retrying HTTP request",
					zap.Int("attempt", attempt+1),
					zap.Int("max_attempts", c.config.RetryAttempts),
					zap.String("url", targetURL),
					zap.Duration("delay", delay))
			}

			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	return nil, fmt.Errorf("failed to fetch %s after %d attempts: %w", targetURL, c.config.RetryAttempts, lastErr)
}

// ============================================================================
// CIRCUIT BREAKER
// ============================================================================

// CircuitBreaker wraps a gobreaker circuit breaker
type CircuitBreaker struct {
	breaker *gobreaker.CircuitBreaker
	logger  *zap.Logger
}

// CircuitBreakerConfig holds circuit breaker configuration
type CircuitBreakerConfig struct {
	Name          string
	MaxFails      int
	Timeout       time.Duration
	OnStateChange func(name string, from gobreaker.State, to gobreaker.State)
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(config *CircuitBreakerConfig, logger *zap.Logger) *CircuitBreaker {
	settings := gobreaker.Settings{
		Name:        config.Name,
		MaxRequests: uint32(config.MaxFails),
		Interval:    time.Minute,
		Timeout:     config.Timeout,
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
			if config.OnStateChange != nil {
				config.OnStateChange(name, from, to)
			}
		},
	}

	return &CircuitBreaker{
		breaker: gobreaker.NewCircuitBreaker(settings),
		logger:  logger,
	}
}

// Execute runs the function through the circuit breaker
func (cb *CircuitBreaker) Execute(fn func() (interface{}, error)) (interface{}, error) {
	return cb.breaker.Execute(fn)
}

// State returns the current state of the circuit breaker
func (cb *CircuitBreaker) State() gobreaker.State {
	return cb.breaker.State()
}

// ============================================================================
// SEMAPHORE
// ============================================================================

// Semaphore provides a simple semaphore implementation
type Semaphore struct {
	sem chan struct{}
}

// NewSemaphore creates a new semaphore with the given maximum capacity
func NewSemaphore(max int) *Semaphore {
	return &Semaphore{
		sem: make(chan struct{}, max),
	}
}

// Acquire acquires a slot in the semaphore
func (s *Semaphore) Acquire() {
	s.sem <- struct{}{}
}

// Release releases a slot in the semaphore
func (s *Semaphore) Release() {
	<-s.sem
}

// Run executes the given function while holding a semaphore slot
func (s *Semaphore) Run(fn func()) {
	s.Acquire()
	defer s.Release()
	fn()
}

// ============================================================================
// PLATFORM TYPES
// ============================================================================

// SongInfo contains metadata about a song
type SongInfo struct {
	Title       string
	Artist      string
	Album       string
	Platform    string
	OriginalURL string
}

// MusicLinks contains URLs for a song across different platforms
type MusicLinks struct {
	Spotify      string
	YouTubeMusic string
	AppleMusic   string
}

// Extractor extracts song information from a platform URL
type Extractor interface {
	ExtractSongInfo(ctx context.Context, url string) (*SongInfo, error)
	Platform() string
}

// Searcher searches for a song on a platform
type Searcher interface {
	Search(ctx context.Context, info *SongInfo) (string, error)
	Platform() string
}

// Platform combines extraction and search capabilities
type Platform interface {
	Extractor
	Searcher
}

// OEmbedResponse represents a standard oEmbed API response
type OEmbedResponse struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	AuthorName  string `json:"author_name"`
}

// YouTubeCandidate represents a YouTube search result candidate
type YouTubeCandidate struct {
	ID      string
	Title   string
	Channel string
}

// ============================================================================
// SPOTIFY IMPLEMENTATION
// ============================================================================

// SpotifyConfig holds Spotify-specific configuration
type SpotifyConfig struct {
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
	TextMirrorURL    string
	Debug            bool
}

// Spotify implements the Platform interface for Spotify
type Spotify struct {
	httpClient     *HTTPClient
	circuitBreaker *CircuitBreaker
	cache          Cache
	negativeCache  Cache
	config         *SpotifyConfig
	logger         *zap.Logger
}

// NewSpotify creates a new Spotify platform implementation
func NewSpotify(
	httpClient *HTTPClient,
	circuitBreaker *CircuitBreaker,
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
		parts := reMidDotSep.Split(strings.ReplaceAll(desc, "\u00A0", " "), -1)

		if artist == "" && len(parts) >= 2 {
			p0, p1 := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			switch {
			case title != "" && strings.EqualFold(p0, title):
				artist = p1
			case title != "" && strings.EqualFold(p1, title):
				artist = p0
			case IsAlbumish(p0) && !IsAlbumish(p1):
				artist = p1
			case IsAlbumish(p1) && !IsAlbumish(p0):
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
			case !IsAlbumish(p0) && IsAlbumish(p1):
				title = p0
			case !IsAlbumish(p1) && IsAlbumish(p0):
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
	if artist == "" || IsAlbumish(artist) ||
		(LooksLikeArtistList(title) && !LooksLikeArtistList(artist)) ||
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

	title, artist := SplitFromOgTitle(ogTitle, ogDesc)
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
	ti, ar := SplitFromOgTitle(ogTitle, ogDesc)
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
		parts := reMidDotSep.Split(strings.ReplaceAll(ogDesc, "\u00A0", " "), -1)
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
	key := NormalizeQuery(info.Artist, info.Title)
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
	t := NormalizeForMatch(title)
	a := NormalizeForMatch(artist)

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

// ============================================================================
// YOUTUBE IMPLEMENTATION
// ============================================================================

// YouTubeConfig holds YouTube-specific configuration
type YouTubeConfig struct {
	CacheTTL time.Duration
	Debug    bool
}

// YouTube implements the Platform interface for YouTube/YouTube Music
type YouTube struct {
	httpClient *HTTPClient
	cache      Cache
	config     *YouTubeConfig
	logger     *zap.Logger
}

// NewYouTube creates a new YouTube platform implementation
func NewYouTube(
	httpClient *HTTPClient,
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

			title, artist := CleanYouTubeInfo(data.Title, data.AuthorName)

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
			title, artist := CleanYouTubeInfo(data.VideoDetails.Title, data.VideoDetails.Author)

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
	key := NormalizeQuery(info.Artist, info.Title)
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
	na := NormalizeForMatch(wantArtist)
	nt := NormalizeForMatch(wantTitle)
	ct := NormalizeForMatch(c.Title)
	cc := NormalizeForMatch(c.Channel)

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

// ============================================================================
// APPLE MUSIC IMPLEMENTATION
// ============================================================================

// AppleMusicConfig holds Apple Music-specific configuration
type AppleMusicConfig struct {
	CacheTTL time.Duration
	Debug    bool
}

// AppleMusic implements the Platform interface for Apple Music
type AppleMusic struct {
	httpClient *HTTPClient
	cache      Cache
	config     *AppleMusicConfig
	logger     *zap.Logger
}

// NewAppleMusic creates a new Apple Music platform implementation
func NewAppleMusic(
	httpClient *HTTPClient,
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
		for fancy := range fancyQuotes {
			cleanOgTitle = strings.ReplaceAll(cleanOgTitle, fancy, "")
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
	title, artist = CleanTitleArtist(title, artist)

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
	key := NormalizeQuery(info.Artist, info.Title)
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
			wantT := NormalizeForMatch(info.Title)
			wantA := NormalizeForMatch(info.Artist)

			bestScore := -999
			bestURL := ""

			for _, result := range data.Results {
				if result.TrackViewURL == "" {
					continue
				}

				t := NormalizeForMatch(result.TrackName)
				ar := NormalizeForMatch(result.ArtistName)

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

// ============================================================================
// SEARCH ORCHESTRATOR
// ============================================================================

// Orchestrator coordinates search across multiple music platforms
type Orchestrator struct {
	spotify    Platform
	youtube    Platform
	appleMusic Platform
}

// NewOrchestrator creates a new search orchestrator
func NewOrchestrator(spotify, youtube, appleMusic Platform) *Orchestrator {
	return &Orchestrator{
		spotify:    spotify,
		youtube:    youtube,
		appleMusic: appleMusic,
	}
}

// ExtractSongInfo attempts to extract song info from text containing a URL
func (o *Orchestrator) ExtractSongInfo(ctx context.Context, text string) (*SongInfo, error) {
	text = strings.TrimSpace(text)
	urlStr := firstURL(text)
	if urlStr == "" {
		return nil, nil
	}

	// Detect platform from URL
	host := strings.ToLower(urlStr)

	// Handle Spotify short links
	if strings.Contains(host, "spotify.link") || strings.Contains(host, "spotify.app.link") {
		resolvedURL, err := resolveSpotifyShortLink(ctx, urlStr)
		if err == nil && resolvedURL != "" {
			return o.spotify.ExtractSongInfo(ctx, resolvedURL)
		}
		return nil, fmt.Errorf("failed to resolve Spotify short link: %w", err)
	}

	switch {
	case strings.Contains(host, "open.spotify.com"):
		return o.spotify.ExtractSongInfo(ctx, urlStr)
	case strings.Contains(host, "music.youtube.com"):
		return o.youtube.ExtractSongInfo(ctx, urlStr)
	case strings.Contains(host, "youtube.com") || strings.Contains(host, "youtu.be"):
		return o.youtube.ExtractSongInfo(ctx, urlStr)
	case strings.Contains(host, "music.apple.com") || strings.Contains(host, "itunes.apple.com"):
		return o.appleMusic.ExtractSongInfo(ctx, urlStr)
	default:
		return nil, nil
	}
}

// FindOnAllPlatforms searches for the song on all platforms
func (o *Orchestrator) FindOnAllPlatforms(ctx context.Context, info *SongInfo) *MusicLinks {
	links := &MusicLinks{}

	var wg sync.WaitGroup
	wg.Add(3)

	// Spotify search
	go func() {
		defer wg.Done()
		if info.Platform == "Spotify" {
			links.Spotify = info.OriginalURL
		} else {
			if url, err := o.spotify.Search(ctx, info); err == nil {
				links.Spotify = url
			}
		}
	}()

	// YouTube Music search
	go func() {
		defer wg.Done()
		switch info.Platform {
		case "YouTube Music":
			links.YouTubeMusic = info.OriginalURL
		case "YouTube":
			if u := ToYouTubeMusicURL(info.OriginalURL); u != "" {
				links.YouTubeMusic = u
			} else {
				if url, err := o.youtube.Search(ctx, info); err == nil {
					links.YouTubeMusic = url
				}
			}
		default:
			if url, err := o.youtube.Search(ctx, info); err == nil {
				links.YouTubeMusic = url
			}
		}
	}()

	// Apple Music search
	go func() {
		defer wg.Done()
		if info.Platform == "Apple Music" {
			links.AppleMusic = info.OriginalURL
		} else {
			if url, err := o.appleMusic.Search(ctx, info); err == nil {
				links.AppleMusic = url
			}
		}
	}()

	wg.Wait()
	return links
}

// EnrichArtistInfo attempts to enrich missing artist information from other platforms
func (o *Orchestrator) EnrichArtistInfo(ctx context.Context, info *SongInfo, links *MusicLinks) {
	if strings.TrimSpace(info.Artist) != "" {
		return
	}

	// Try Apple Music first
	if links.AppleMusic != "" {
		if si, err := o.appleMusic.ExtractSongInfo(ctx, links.AppleMusic); err == nil && si != nil && strings.TrimSpace(si.Artist) != "" {
			info.Artist = si.Artist
			return
		}
	}

	// Try YouTube Music
	if strings.TrimSpace(info.Artist) == "" && links.YouTubeMusic != "" {
		if si, err := o.youtube.ExtractSongInfo(ctx, links.YouTubeMusic); err == nil && si != nil && strings.TrimSpace(si.Artist) != "" {
			info.Artist = si.Artist
		}
	}
}

// firstURL extracts the first URL from text
func firstURL(s string) string {
	m := reFirstURL.FindString(s)
	return strings.TrimRight(m, ".,);!?]}>\"'")
}

// resolveSpotifyShortLink follows a spotify.link redirect
func resolveSpotifyShortLink(ctx context.Context, shortURL string) (string, error) {
	client := &http.Client{
		Timeout: 10 * http.DefaultClient.Timeout,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", shortURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch short link: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	finalURL := resp.Request.URL.String()
	if strings.Contains(finalURL, "open.spotify.com/track/") {
		if idx := strings.Index(finalURL, "?"); idx != -1 {
			finalURL = finalURL[:idx]
		}
		return finalURL, nil
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	re := regexp.MustCompile(`open\.spotify\.com/track/([A-Za-z0-9]+)`)
	match := re.FindStringSubmatch(string(body))
	if len(match) > 1 {
		trackID := match[1]
		return fmt.Sprintf("https://open.spotify.com/track/%s", trackID), nil
	}

	return "", fmt.Errorf("could not extract Spotify URL from short link")
}

// ============================================================================
// TELEGRAM CLIENT
// ============================================================================

// TelegramClient wraps the Telegram bot API with helper methods
type TelegramClient struct {
	bot   *tgbotapi.BotAPI
	debug bool
}

// NewTelegramClient creates a new Telegram client
func NewTelegramClient(token string, debug bool) (*TelegramClient, error) {
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}
	bot.Debug = debug

	return &TelegramClient{
		bot:   bot,
		debug: debug,
	}, nil
}

// GetBot returns the underlying Telegram bot API
func (c *TelegramClient) GetBot() *tgbotapi.BotAPI {
	return c.bot
}

// SendReply sends a reply message with Markdown formatting
func (c *TelegramClient) SendReply(chatID int64, replyToID int, text string) error {
	for _, chunk := range chunkText(text, 3500) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		msg.ReplyToMessageID = replyToID
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		msg.DisableWebPagePreview = true

		if _, err := c.bot.Send(msg); err != nil {
			if c.debug {
				log.Printf("⚠️  Error sending message with MarkdownV2: %v", err)
			}
			msg.ParseMode = ""
			if _, err := c.bot.Send(msg); err != nil {
				return fmt.Errorf("failed to send message: %w", err)
			}
		}
	}
	return nil
}

// SendLinks sends a message with inline keyboard buttons for music links
func (c *TelegramClient) SendLinks(chatID int64, replyToID int, info *SongInfo, links *MusicLinks) error {
	title := Md2(info.Title)
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}
	artist = Md2(artist)

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

	if _, err := c.bot.Send(msg); err != nil {
		return fmt.Errorf("failed to send links: %w", err)
	}

	return nil
}

// chunkText splits text into chunks of specified maximum length
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

// Md2 escapes text for Telegram MarkdownV2
func Md2(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(s)
}

// ============================================================================
// BOT IMPLEMENTATION
// ============================================================================

// MusicBot is the main bot instance
type MusicBot struct {
	config       *Config
	telegram     *TelegramClient
	httpClient   *HTTPClient
	logger       *zap.Logger
	orchestrator *Orchestrator

	// Caches
	queryCache    *SimpleCache
	urlCache      *SimpleCache
	negativeCache *SimpleCache

	// Circuit Breakers
	spotifyBreaker    *CircuitBreaker
	youtubeBreaker    *CircuitBreaker
	appleMusicBreaker *CircuitBreaker

	// Semaphores
	fetchSem *Semaphore
	msgSem   *Semaphore
}

// NewMusicBot creates a new MusicBot instance
func NewMusicBot(cfg *Config) (*MusicBot, error) {
	// Initialize logger
	logger, err := createLogger(cfg.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create logger: %w", err)
	}

	// Create Telegram client
	tg, err := NewTelegramClient(cfg.TelegramToken, cfg.Debug)
	if err != nil {
		return nil, fmt.Errorf("failed to create telegram client: %w", err)
	}

	logger.Info("Authorized as Telegram bot",
		zap.String("username", tg.GetBot().Self.UserName),
		zap.Bool("debug", cfg.Debug))

	// Create HTTP client
	httpClientConfig := &HTTPConfig{
		Timeout:       cfg.RequestTimeout,
		RetryAttempts: cfg.RetryAttempts,
		RetryMinDelay: cfg.RetryMinDelay,
		RetryMaxDelay: cfg.RetryMaxDelay,
		Debug:         cfg.Debug,
	}
	httpClient := NewHTTPClient(httpClientConfig, logger)

	// Create caches
	queryCache := NewCache()
	urlCache := NewCache()
	negativeCache := NewCache()

	// Create circuit breakers
	spotifyBreaker := NewCircuitBreaker(&CircuitBreakerConfig{
		Name:     "Spotify",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	youtubeBreaker := NewCircuitBreaker(&CircuitBreakerConfig{
		Name:     "YouTube",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	appleMusicBreaker := NewCircuitBreaker(&CircuitBreakerConfig{
		Name:     "AppleMusic",
		MaxFails: cfg.CircuitBreakerMaxFails,
		Timeout:  cfg.CircuitBreakerTimeout,
	}, logger)

	// Create platform implementations
	spotify := NewSpotify(
		httpClient,
		spotifyBreaker,
		urlCache,
		negativeCache,
		&SpotifyConfig{
			CacheTTL:         cfg.CacheTTL,
			NegativeCacheTTL: cfg.NegativeCacheTTL,
			TextMirrorURL:    cfg.TextMirrorURL,
			Debug:            cfg.Debug,
		},
		logger,
	)

	youtube := NewYouTube(
		httpClient,
		urlCache,
		&YouTubeConfig{
			CacheTTL: cfg.CacheTTL,
			Debug:    cfg.Debug,
		},
		logger,
	)

	appleMusic := NewAppleMusic(
		httpClient,
		urlCache,
		&AppleMusicConfig{
			CacheTTL: cfg.CacheTTL,
			Debug:    cfg.Debug,
		},
		logger,
	)

	// Create search orchestrator
	orch := NewOrchestrator(spotify, youtube, appleMusic)

	return &MusicBot{
		config:            cfg,
		telegram:          tg,
		httpClient:        httpClient,
		logger:            logger,
		orchestrator:      orch,
		queryCache:        queryCache,
		urlCache:          urlCache,
		negativeCache:     negativeCache,
		spotifyBreaker:    spotifyBreaker,
		youtubeBreaker:    youtubeBreaker,
		appleMusicBreaker: appleMusicBreaker,
		fetchSem:          NewSemaphore(cfg.MaxConcurrentFetches),
		msgSem:            NewSemaphore(cfg.MaxConcurrentMessages),
	}, nil
}

// Run starts the bot's main loop
func (mb *MusicBot) Run(ctx context.Context) error {
	// Start cache cleanup goroutines
	go mb.queryCache.StartCleanup(ctx, 1*time.Hour)
	go mb.urlCache.StartCleanup(ctx, 1*time.Hour)
	go mb.negativeCache.StartCleanup(ctx, 10*time.Minute)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := mb.telegram.GetBot().GetUpdatesChan(u)

	log.Println("🎵 Music Link Converter Bot is running…")
	log.Println("📡 No API keys required - using web scraping!")

	for {
		select {
		case <-ctx.Done():
			log.Println("👋 Shutting down bot loop gracefully…")
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}

			// Handle message in a goroutine with concurrency control
			go func(m *tgbotapi.Message) {
				mb.msgSem.Run(func() {
					defer func() {
						if r := recover(); r != nil {
							mb.logger.Error("Panic in handleMessage",
								zap.Any("panic", r),
								zap.Int64("chat_id", m.Chat.ID),
								zap.Int("message_id", m.MessageID))
						}
					}()

					msgCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()

					if err := mb.handleMessage(msgCtx, m); err != nil {
						mb.logger.Error("Error handling message",
							zap.Error(err),
							zap.Int64("chat_id", m.Chat.ID),
							zap.Int("message_id", m.MessageID))
					}
				})
			}(update.Message)
		}
	}
}

// handleMessage handles a single incoming message
func (mb *MusicBot) handleMessage(ctx context.Context, m *tgbotapi.Message) error {
	chatID := m.Chat.ID
	replyTo := m.MessageID

	// Handle commands
	if m.IsCommand() {
		switch m.Command() {
		case "start":
			return mb.telegram.SendReply(chatID, replyTo,
				"🎵 *Welcome to Music Link Converter\\!*\n\nSend me a Spotify, YouTube/YouTube Music, or Apple Music link, and I'll find it on all platforms for you\\.\n\n_No API keys required_")
		case "help":
			return mb.telegram.SendReply(chatID, replyTo,
				"📖 *How to use:*\n\n1\\. Send a music link from Spotify, YouTube/YouTube Music, or Apple Music\n2\\. I'll search for the same song on all platforms\n3\\. Get links for all three services\\!\n\n*Supported formats:*\n• Spotify: `https://open\\.spotify\\.com/track/\\.\\.\\.`\n• YouTube Music: `https://music\\.youtube\\.com/watch?v=\\.\\.\\.`\n• YouTube: `https://www\\.youtube\\.com/watch?v=\\.\\.\\.` or `https://youtu\\.be/\\.\\.\\.`\n• Apple Music: `https://music\\.apple\\.com/\\.\\.\\.`")
		}
		return nil
	}

	// Extract song info from message
	info, err := mb.orchestrator.ExtractSongInfo(ctx, m.Text)
	if err != nil {
		mb.logger.Debug("Failed to extract song info", zap.Error(err))
		return mb.telegram.SendReply(chatID, replyTo,
			"❌ Please send a valid *music link* from Spotify, YouTube/YouTube Music, or Apple Music\\.")
	}

	if info == nil {
		return mb.telegram.SendReply(chatID, replyTo,
			"❌ Please send a valid *music link* from Spotify, YouTube/YouTube Music, or Apple Music\\.")
	}

	// Send initial response
	artist := info.Artist
	if strings.TrimSpace(artist) == "" {
		artist = "Unknown Artist"
	}

	if err := mb.telegram.SendReply(chatID, replyTo,
		fmt.Sprintf("🔍 Found: *%s* by *%s*\n\nSearching other platforms…", Md2(info.Title), Md2(artist))); err != nil {
		mb.logger.Error("Failed to send initial reply", zap.Error(err))
	}

	// Search on all platforms
	links := mb.orchestrator.FindOnAllPlatforms(ctx, info)

	// Enrich missing artist using other platform results
	mb.orchestrator.EnrichArtistInfo(ctx, info, links)

	// Check if we found any links
	if links.Spotify == "" && links.YouTubeMusic == "" && links.AppleMusic == "" {
		return mb.telegram.SendReply(chatID, replyTo,
			"😕 I couldn't find matches on other platforms\\. It might be a regional or rare release\\.")
	}

	// Send results
	return mb.telegram.SendLinks(chatID, replyTo, info, links)
}

// createLogger creates a zap logger
func createLogger(debug bool) (*zap.Logger, error) {
	if debug {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// ============================================================================
// MAIN FUNCTION
// ============================================================================

func main() {
	// Load configuration
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("❌ Configuration error: %v", err)
	}

	// Create bot instance
	musicBot, err := NewMusicBot(cfg)
	if err != nil {
		log.Fatalf("❌ Failed to create bot: %v", err)
	}

	// Start health check server
	go startHealthCheckServer()

	// Setup graceful shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run the bot
	if err := musicBot.Run(ctx); err != nil {
		log.Fatalf("❌ Bot error: %v", err)
	}

	log.Println("✅ Bot shut down successfully")
}

// startHealthCheckServer starts an HTTP server for health checks
func startHealthCheckServer() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	})

	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("READY"))
	})

	port := os.Getenv("HEALTH_PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🏥 Health check server listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Printf("⚠️  Health check server error: %v", err)
	}
}
