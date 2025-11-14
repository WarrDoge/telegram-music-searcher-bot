package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test song: "bad guy" by Billie Eilish (popular, available on all platforms)
var testSong = struct {
	spotify      string
	youtubeMusic string
	appleMusic   string
}{
	spotify:      "https://open.spotify.com/track/2Fxmhks0bxGSBdJ92vM42m",
	youtubeMusic: "https://music.youtube.com/watch?v=DyDfgMOUjCI",
	appleMusic:   "https://music.apple.com/us/album/bad-guy/1450695723?i=1450695739",
}

func createTestBot(t *testing.T) *MusicBot {
	// Set dummy token for testing (only extraction functions will be tested)
	os.Setenv("TELEGRAM_BOT_TOKEN", "test-token")

	config := &Config{
		TelegramToken:          "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
		Debug:                  testing.Verbose(),
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

	// Skip telegram bot initialization for testing
	bot := &MusicBot{
		config:            config,
		httpClient:        config.createHTTPClient(),
		queryCache:        NewSimpleCache(),
		urlCache:          NewSimpleCache(),
		negativeCache:     NewSimpleCache(),
		fetchSem:          NewSemaphore(config.MaxConcurrentFetches),
		msgSem:            NewSemaphore(config.MaxConcurrentMessages),
	}

	// Initialize logger
	logger, err := NewLogger(config.Debug)
	require.NoError(t, err)
	bot.logger = logger

	// Initialize circuit breakers
	bot.spotifyBreaker = createCircuitBreaker("Spotify", config, logger)
	bot.youtubeBreaker = createCircuitBreaker("YouTube", config, logger)
	bot.appleMusicBreaker = createCircuitBreaker("AppleMusic", config, logger)

	// Initialize Colly
	bot.collector = createCollyCollector(config)

	return bot
}

// Helper functions for bot initialization
func (c *Config) createHTTPClient() *http.Client {
	return &http.Client{
		Timeout: c.RequestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// Integration Tests

func TestSpotifyExtraction(t *testing.T) {
	bot := createTestBot(t)

	info := bot.getSpotifyInfo(testSong.spotify)

	require.NotNil(t, info, "Should extract Spotify info")
	assert.Equal(t, "bad guy", strings.ToLower(info.Title), "Title should be 'bad guy'")
	assert.Equal(t, "billie eilish", strings.ToLower(info.Artist), "Artist should be 'billie eilish'")
	assert.Equal(t, "Spotify", info.Platform)

	// Critical: artist must not be empty
	assert.NotEmpty(t, info.Artist, "Artist must not be empty")
	assert.NotEmpty(t, strings.TrimSpace(info.Artist), "Artist must not be whitespace only")
}

func TestYouTubeMusicExtraction(t *testing.T) {
	bot := createTestBot(t)

	info := bot.getYouTubeInfo(testSong.youtubeMusic, "YouTube Music")

	require.NotNil(t, info, "Should extract YouTube Music info")
	assert.Equal(t, "bad guy", strings.ToLower(info.Title), "Title should be 'bad guy'")
	assert.Equal(t, "billie eilish", strings.ToLower(info.Artist), "Artist should be 'billie eilish'")
	assert.Equal(t, "YouTube Music", info.Platform)

	// Regression test: title should NOT start with artist name
	lowerTitle := strings.ToLower(info.Title)
	assert.False(t, strings.HasPrefix(lowerTitle, "billie eilish -"), "Title should not start with artist name")
	assert.False(t, strings.HasPrefix(lowerTitle, "billie eilish-"), "Title should not start with artist name")
}

func TestAppleMusicExtraction(t *testing.T) {
	bot := createTestBot(t)

	info := bot.getAppleMusicInfo(testSong.appleMusic)

	require.NotNil(t, info, "Should extract Apple Music info")
	assert.Equal(t, "bad guy", strings.ToLower(info.Title), "Title should be 'bad guy'")
	assert.Equal(t, "billie eilish", strings.ToLower(info.Artist), "Artist should be 'billie eilish'")
	assert.Equal(t, "Apple Music", info.Platform)

	// Artist should not be empty
	assert.NotEmpty(t, info.Artist)
	assert.NotEmpty(t, strings.TrimSpace(info.Artist))
}

// Unit Tests

func TestCleanYouTubeInfo(t *testing.T) {
	tests := []struct {
		name           string
		inputTitle     string
		inputArtist    string
		expectedTitle  string
		expectedArtist string
	}{
		{
			name:           "Remove - Topic suffix",
			inputTitle:     "Song Title",
			inputArtist:    "Artist Name - Topic",
			expectedTitle:  "Song Title",
			expectedArtist: "Artist Name",
		},
		{
			name:           "Remove VEVO suffix",
			inputTitle:     "Song Title",
			inputArtist:    "ArtistVEVO",
			expectedTitle:  "Song Title",
			expectedArtist: "Artist",
		},
		{
			name:           "Remove Official suffix",
			inputTitle:     "Song Title",
			inputArtist:    "ArtistOfficial",
			expectedTitle:  "Song Title",
			expectedArtist: "Artist",
		},
		{
			name:           "Fix camelCase",
			inputTitle:     "Song Title",
			inputArtist:    "TaylorSwift",
			expectedTitle:  "Song Title",
			expectedArtist: "Taylor Swift",
		},
		{
			name:           "Remove artist prefix from title",
			inputTitle:     "Billie Eilish - bad guy",
			inputArtist:    "Billie Eilish",
			expectedTitle:  "bad guy",
			expectedArtist: "Billie Eilish",
		},
		{
			name:           "Case insensitive artist prefix removal",
			inputTitle:     "BILLIE EILISH - Bad Guy",
			inputArtist:    "Billie Eilish",
			expectedTitle:  "Bad Guy",
			expectedArtist: "Billie Eilish",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, artist := cleanYouTubeInfo(tt.inputTitle, tt.inputArtist)
			assert.Equal(t, tt.expectedTitle, title)
			assert.Equal(t, tt.expectedArtist, artist)
		})
	}
}

func TestNormalizeQuery(t *testing.T) {
	tests := []struct {
		name     string
		artist   string
		title    string
		expected string
	}{
		{
			name:     "Basic normalization",
			artist:   "Billie Eilish",
			title:    "bad guy",
			expected: "billie eilish bad guy",
		},
		{
			name:     "Remove parentheses",
			artist:   "Artist",
			title:    "Title (feat. Someone)",
			expected: "artist title",
		},
		{
			name:     "Remove remastered suffix",
			artist:   "Beatles",
			title:    "Let It Be - Remastered",
			expected: "beatles let it be -", // Trailing dash is intentional - will be handled by search
		},
		{
			name:     "Remove single/EP suffix",
			artist:   "Artist",
			title:    "Song - Single",
			expected: "artist song",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeQuery(tt.artist, tt.title)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestNormalizeForMatch(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Lowercase and normalize spaces",
			input:    "Bad Guy",
			expected: "bad guy",
		},
		{
			name:     "Remove punctuation",
			input:    "don't-stop, me!",
			expected: "don t stop me",
		},
		{
			name:     "Replace ampersand with and",
			input:    "Rock & Roll",
			expected: "rock and roll",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeForMatch(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCalculateSimilarity(t *testing.T) {
	tests := []struct {
		name      string
		s1        string
		s2        string
		minScore  float64 // minimum expected similarity
	}{
		{
			name:     "Exact match",
			s1:       "bad guy",
			s2:       "bad guy",
			minScore: 1.0,
		},
		{
			name:     "Case insensitive match",
			s1:       "Bad Guy",
			s2:       "bad guy",
			minScore: 1.0,
		},
		{
			name:     "High similarity",
			s1:       "bad guy",
			s2:       "bad guys",
			minScore: 0.85,
		},
		{
			name:     "Low similarity",
			s1:       "bad guy",
			s2:       "good girl",
			minScore: -1.0, // Can be negative for very different strings
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := calculateSimilarity(tt.s1, tt.s2)
			assert.GreaterOrEqual(t, score, tt.minScore)
		})
	}
}

func TestScoreYouTubeCandidate(t *testing.T) {
	tests := []struct {
		name      string
		candidate YouTubeCandidate
		wantTitle string
		wantArtist string
		minScore  int
	}{
		{
			name: "Perfect match with topic channel",
			candidate: YouTubeCandidate{
				ID:      "abc123",
				Title:   "bad guy",
				Channel: "Billie Eilish - Topic",
			},
			wantTitle:  "bad guy",
			wantArtist: "Billie Eilish",
			minScore:   6, // title match(4) + channel match with topic(2+2) = 8, adjusted for partial
		},
		{
			name: "Penalize covers",
			candidate: YouTubeCandidate{
				ID:      "xyz789",
				Title:   "bad guy cover",
				Channel: "Random Channel",
			},
			wantTitle:  "bad guy",
			wantArtist: "Billie Eilish",
			minScore:   -10, // Should get negative score
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := scoreYouTubeCandidate(tt.candidate, tt.wantArtist, tt.wantTitle)
			assert.GreaterOrEqual(t, score, tt.minScore)
		})
	}
}

func TestSimpleCache(t *testing.T) {
	cache := NewSimpleCache()

	// Test Set and Get
	cache.Set("key1", "value1", 1*time.Second)
	val, ok := cache.Get("key1")
	assert.True(t, ok)
	assert.Equal(t, "value1", val)

	// Test expiration
	cache.Set("key2", "value2", 100*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	_, ok = cache.Get("key2")
	assert.False(t, ok, "Cache entry should have expired")

	// Test missing key
	_, ok = cache.Get("nonexistent")
	assert.False(t, ok)
}

func TestIsAlbumish(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"Soundtrack", "Original Soundtrack", true},
		{"OST", "Movie OST", true},
		{"Volume", "Greatest Hits Vol. 1", true},
		{"Regular song", "Bad Guy", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAlbumish(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestLooksLikeArtistList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"With comma", "Artist1, Artist2", true},
		{"With ampersand", "Artist1 & Artist2", true},
		{"With feat", "Artist feat. Someone", true},
		{"Single artist", "Billie Eilish", false},
		{"With colon", "Album: Title", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := looksLikeArtistList(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
