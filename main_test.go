package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// UNIT TESTS - Text Normalization
// ============================================================================

func TestNormalizeQuery(t *testing.T) {
	tests := []struct {
		name     string
		artist   string
		title    string
		expected string
	}{
		{
			name:     "basic normalization",
			artist:   "Billie Eilish",
			title:    "bad guy",
			expected: "billie eilish bad guy",
		},
		{
			name:     "removes parentheses",
			artist:   "Taylor Swift",
			title:    "Love Story (Taylor's Version)",
			expected: "taylor swift love story",
		},
		{
			name:     "removes feat",
			artist:   "Ed Sheeran",
			title:    "Shape of You (feat. Stormzy)",
			expected: "ed sheeran shape of you",
		},
		{
			name:     "removes remastered",
			artist:   "The Beatles",
			title:    "Let It Be Remastered",
			expected: "the beatles let it be",
		},
		{
			name:     "removes official video",
			artist:   "Adele",
			title:    "Hello Official Video",
			expected: "adele hello",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeQuery(tt.artist, tt.title)
			if result != tt.expected {
				t.Errorf("NormalizeQuery(%q, %q) = %q, want %q",
					tt.artist, tt.title, result, tt.expected)
			}
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
			name:     "basic normalization",
			input:    "Bad Guy",
			expected: "bad guy",
		},
		{
			name:     "removes dashes",
			input:    "Love-Story",
			expected: "love story",
		},
		{
			name:     "removes special chars",
			input:    "Don't Stop Me Now!",
			expected: "don t stop me now",
		},
		{
			name:     "removes parentheses content",
			input:    "Song (Remix)",
			expected: "song",
		},
		{
			name:     "normalizes ampersand",
			input:    "You & Me",
			expected: "you and me",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeForMatch(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeForMatch(%q) = %q, want %q",
					tt.input, result, tt.expected)
			}
		})
	}
}

func TestCleanYouTubeInfo(t *testing.T) {
	tests := []struct {
		name           string
		title          string
		artist         string
		expectedTitle  string
		expectedArtist string
	}{
		{
			name:           "removes Topic suffix",
			title:          "Bad Guy",
			artist:         "Billie Eilish - Topic",
			expectedTitle:  "Bad Guy",
			expectedArtist: "Billie Eilish",
		},
		{
			name:           "removes VEVO suffix",
			title:          "Shape of You",
			artist:         "Ed SheeranVEVO",
			expectedTitle:  "Shape of You",
			expectedArtist: "Ed Sheeran",
		},
		{
			name:           "removes artist prefix from title",
			title:          "Adele - Hello",
			artist:         "Adele",
			expectedTitle:  "Hello",
			expectedArtist: "Adele",
		},
		{
			name:           "fixes camelCase artist",
			title:          "Shake It Off",
			artist:         "TaylorSwift",
			expectedTitle:  "Shake It Off",
			expectedArtist: "Taylor Swift",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, artist := CleanYouTubeInfo(tt.title, tt.artist)
			if title != tt.expectedTitle {
				t.Errorf("CleanYouTubeInfo title = %q, want %q", title, tt.expectedTitle)
			}
			if artist != tt.expectedArtist {
				t.Errorf("CleanYouTubeInfo artist = %q, want %q", artist, tt.expectedArtist)
			}
		})
	}
}

func TestSplitFromOgTitle(t *testing.T) {
	tests := []struct {
		name           string
		ogTitle        string
		ogDesc         string
		expectedTitle  string
		expectedArtist string
	}{
		{
			name:           "handles 'by' separator",
			ogTitle:        "Bad Guy by Billie Eilish",
			ogDesc:         "",
			expectedTitle:  "Bad Guy",
			expectedArtist: "Billie Eilish",
		},
		{
			name:           "handles dash separator with description hint",
			ogTitle:        "Bad Guy — Billie Eilish",
			ogDesc:         "Billie Eilish · Song · 2019",
			expectedTitle:  "Bad Guy",
			expectedArtist: "Billie Eilish",
		},
		{
			name:           "handles reversed order with hint",
			ogTitle:        "Billie Eilish — Bad Guy",
			ogDesc:         "Billie Eilish · Song · 2019",
			expectedTitle:  "Bad Guy",
			expectedArtist: "Billie Eilish",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, artist := SplitFromOgTitle(tt.ogTitle, tt.ogDesc)
			if title != tt.expectedTitle {
				t.Errorf("SplitFromOgTitle title = %q, want %q", title, tt.expectedTitle)
			}
			if artist != tt.expectedArtist {
				t.Errorf("SplitFromOgTitle artist = %q, want %q", artist, tt.expectedArtist)
			}
		})
	}
}

func TestIsAlbumish(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"soundtrack", "Original Soundtrack", true},
		{"ost", "Game OST", true},
		{"volume", "Greatest Hits Vol. 1", true},
		{"season", "Season 2 Music", true},
		{"regular song", "Bad Guy", false},
		{"regular album", "When We All Fall Asleep", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsAlbumish(tt.input)
			if result != tt.expected {
				t.Errorf("IsAlbumish(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestLooksLikeArtistList(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"comma separated", "Ed Sheeran, Justin Bieber", true},
		{"with ampersand", "Simon & Garfunkel", true},
		{"with feat", "Drake feat. Rihanna", true},
		{"single artist", "Billie Eilish", false},
		{"song title", "Love Story", false},
		{"title with dash", "Anti-Hero", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := LooksLikeArtistList(tt.input)
			if result != tt.expected {
				t.Errorf("LooksLikeArtistList(%q) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

// ============================================================================
// UNIT TESTS - Similarity
// ============================================================================

func TestCalculateSimilarity(t *testing.T) {
	tests := []struct {
		name     string
		s1       string
		s2       string
		minScore float64
		maxScore float64
	}{
		{"exact match", "bad guy", "bad guy", 1.0, 1.0},
		{"case insensitive", "Bad Guy", "bad guy", 1.0, 1.0},
		{"similar strings", "bad guy", "badguy", 0.8, 1.0},
		{"different strings", "hello", "world", -1.0, 0.2},
		{"empty strings", "", "", 0.0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateSimilarity(tt.s1, tt.s2)
			if result < tt.minScore || result > tt.maxScore {
				t.Errorf("CalculateSimilarity(%q, %q) = %v, want between %v and %v",
					tt.s1, tt.s2, result, tt.minScore, tt.maxScore)
			}
		})
	}
}

// ============================================================================
// UNIT TESTS - Cache
// ============================================================================

func TestCacheBasicOperations(t *testing.T) {
	cache := NewCache()

	// Test Set and Get
	cache.Set("key1", "value1", 1*time.Hour)

	value, ok := cache.Get("key1")
	if !ok {
		t.Fatal("Expected to find key1 in cache")
	}
	if value != "value1" {
		t.Errorf("Expected value1, got %s", value)
	}

	// Test non-existent key
	_, ok = cache.Get("nonexistent")
	if ok {
		t.Error("Expected not to find nonexistent key")
	}
}

func TestCacheExpiration(t *testing.T) {
	cache := NewCache()

	// Set with very short TTL
	cache.Set("expiring", "value", 10*time.Millisecond)

	// Should exist immediately
	_, ok := cache.Get("expiring")
	if !ok {
		t.Fatal("Expected to find key immediately after setting")
	}

	// Wait for expiration
	time.Sleep(20 * time.Millisecond)

	// Should not exist after expiration
	_, ok = cache.Get("expiring")
	if ok {
		t.Error("Expected key to be expired")
	}
}

// ============================================================================
// INTEGRATION TESTS - Real HTTP Requests
// ============================================================================

var testSong = struct {
	title        string
	artist       string
	spotify      string
	youtubeMusic string
	appleMusic   string
}{
	title:        "bad guy",
	artist:       "billie eilish",
	spotify:      "https://open.spotify.com/track/2Fxmhks0bxGSBdJ92vM42m",
	youtubeMusic: "https://music.youtube.com/watch?v=ZD6rXLXZOEI",
	appleMusic:   "https://music.apple.com/us/album/bad-guy/1450695723?i=1450695739",
}

func TestSpotifyExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	platforms := createTestPlatforms(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("should extract bad guy by Billie Eilish from Spotify", func(t *testing.T) {
		info, err := platforms.spotify.ExtractSongInfo(ctx, testSong.spotify)
		if err != nil {
			t.Fatalf("Failed to extract Spotify info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		// Verify title
		if strings.ToLower(info.Title) != testSong.title {
			t.Errorf("Title = %q, want %q", info.Title, testSong.title)
		}

		// Verify artist
		if strings.ToLower(info.Artist) != testSong.artist {
			t.Errorf("Artist = %q, want %q", info.Artist, testSong.artist)
		}

		// Critical: artist must not be empty
		if strings.TrimSpace(info.Artist) == "" {
			t.Error("Artist must not be empty")
		}

		// Verify platform
		if info.Platform != "Spotify" {
			t.Errorf("Platform = %q, want Spotify", info.Platform)
		}

		// Test display format
		display := info.Title + " by " + info.Artist
		expectedDisplay := "bad guy by billie eilish"
		if strings.ToLower(display) != expectedDisplay {
			t.Errorf("Display format = %q, want %q", display, expectedDisplay)
		}

		t.Logf("✅ Spotify extraction: %s by %s", info.Title, info.Artist)
		t.Logf("   Display: %s", display)
	})
}

func TestYouTubeMusicExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	platforms := createTestPlatforms(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("should extract bad guy by Billie Eilish from YouTube Music", func(t *testing.T) {
		info, err := platforms.youtube.ExtractSongInfo(ctx, testSong.youtubeMusic)
		if err != nil {
			t.Fatalf("Failed to extract YouTube info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		// Title should be just "bad guy", NOT "Billie Eilish - bad guy"
		if strings.ToLower(info.Title) != testSong.title {
			t.Errorf("Title = %q, want %q", info.Title, testSong.title)
		}

		// Artist should be clean
		if strings.ToLower(info.Artist) != testSong.artist {
			t.Errorf("Artist = %q, want %q", info.Artist, testSong.artist)
		}

		// Test display format - should NOT duplicate artist
		display := info.Title + " by " + info.Artist
		expectedDisplay := "bad guy by billie eilish"
		if strings.ToLower(display) != expectedDisplay {
			t.Errorf("Display format = %q, want %q", display, expectedDisplay)
		}

		// Should not contain duplication
		lowerDisplay := strings.ToLower(display)
		if strings.Contains(lowerDisplay, "billie eilish - bad guy by billie eilish") {
			t.Error("Display format contains artist duplication")
		}

		t.Logf("✅ YouTube Music extraction: %s by %s", info.Title, info.Artist)
		t.Logf("   Display: %s", display)
	})

	t.Run("should remove artist prefix from YouTube titles", func(t *testing.T) {
		info, err := platforms.youtube.ExtractSongInfo(ctx, testSong.youtubeMusic)
		if err != nil {
			t.Fatalf("Failed to extract YouTube info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		// Regression test: title should NOT start with artist name
		if strings.HasPrefix(strings.ToLower(info.Title), "billie eilish -") {
			t.Errorf("Title should not start with artist name: %q", info.Title)
		}

		// Should be clean title only
		if strings.ToLower(info.Title) != testSong.title {
			t.Errorf("Title = %q, want %q", info.Title, testSong.title)
		}
	})

	t.Run("should clean VEVO and other channel suffixes", func(t *testing.T) {
		info, err := platforms.youtube.ExtractSongInfo(ctx, testSong.youtubeMusic)
		if err != nil {
			t.Fatalf("Failed to extract YouTube info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		// Should not contain VEVO or Official
		if strings.HasSuffix(info.Artist, "VEVO") || strings.HasSuffix(info.Artist, "vevo") {
			t.Errorf("Artist should not end with VEVO: %q", info.Artist)
		}
		if strings.HasSuffix(info.Artist, "Official") {
			t.Errorf("Artist should not end with Official: %q", info.Artist)
		}

		t.Logf("✅ YouTube artist cleaned: %s", info.Artist)
	})
}

func TestAppleMusicExtraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	platforms := createTestPlatforms(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	t.Run("should extract bad guy by Billie Eilish from Apple Music", func(t *testing.T) {
		info, err := platforms.appleMusic.ExtractSongInfo(ctx, testSong.appleMusic)
		if err != nil {
			t.Fatalf("Failed to extract Apple Music info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		if strings.ToLower(info.Title) != testSong.title {
			t.Errorf("Title = %q, want %q", info.Title, testSong.title)
		}

		if strings.ToLower(info.Artist) != testSong.artist {
			t.Errorf("Artist = %q, want %q", info.Artist, testSong.artist)
		}

		if info.Platform != "Apple Music" {
			t.Errorf("Platform = %q, want Apple Music", info.Platform)
		}

		if info.OriginalURL != testSong.appleMusic {
			t.Errorf("OriginalURL = %q, want %q", info.OriginalURL, testSong.appleMusic)
		}

		t.Logf("✅ Apple Music extraction: %s by %s", info.Title, info.Artist)
	})

	t.Run("should NOT duplicate artist in title", func(t *testing.T) {
		info, err := platforms.appleMusic.ExtractSongInfo(ctx, testSong.appleMusic)
		if err != nil {
			t.Fatalf("Failed to extract Apple Music info: %v", err)
		}

		if info == nil {
			t.Fatal("Expected non-nil SongInfo")
		}

		// Regression test: title should NOT contain "by Artist"
		if strings.Contains(strings.ToLower(info.Title), "by billie eilish") {
			t.Errorf("Title should not contain 'by artist': %q", info.Title)
		}

		if strings.ToLower(info.Title) != testSong.title {
			t.Errorf("Title = %q, want %q", info.Title, testSong.title)
		}

		// Test display format
		display := info.Title + " by " + info.Artist
		expectedDisplay := "bad guy by billie eilish"
		if strings.ToLower(display) != expectedDisplay {
			t.Errorf("Display format = %q, want %q", display, expectedDisplay)
		}

		t.Logf("✅ Apple Music title check:")
		t.Logf("   Title: %s", info.Title)
		t.Logf("   Artist: %s", info.Artist)
		t.Logf("   Display: %s", display)
	})
}

func TestCrossPlatformConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	platforms := createTestPlatforms(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	t.Run("all platforms should return clean consistent data", func(t *testing.T) {
		// Fetch from all platforms in parallel
		type result struct {
			info *SongInfo
			err  error
		}

		spotifyCh := make(chan result, 1)
		youtubeCh := make(chan result, 1)
		appleCh := make(chan result, 1)

		go func() {
			info, err := platforms.spotify.ExtractSongInfo(ctx, testSong.spotify)
			spotifyCh <- result{info, err}
		}()

		go func() {
			info, err := platforms.youtube.ExtractSongInfo(ctx, testSong.youtubeMusic)
			youtubeCh <- result{info, err}
		}()

		go func() {
			info, err := platforms.appleMusic.ExtractSongInfo(ctx, testSong.appleMusic)
			appleCh <- result{info, err}
		}()

		spotifyResult := <-spotifyCh
		youtubeResult := <-youtubeCh
		appleResult := <-appleCh

		if spotifyResult.err != nil {
			t.Fatalf("Spotify error: %v", spotifyResult.err)
		}
		if youtubeResult.err != nil {
			t.Fatalf("YouTube error: %v", youtubeResult.err)
		}
		if appleResult.err != nil {
			t.Fatalf("Apple Music error: %v", appleResult.err)
		}

		spotify := spotifyResult.info
		youtube := youtubeResult.info
		apple := appleResult.info

		if spotify == nil || youtube == nil || apple == nil {
			t.Fatal("All platform infos should be non-nil")
		}

		// All titles should be clean (no artist prefix)
		if strings.ToLower(spotify.Title) != testSong.title {
			t.Errorf("Spotify title = %q, want %q", spotify.Title, testSong.title)
		}
		if strings.ToLower(youtube.Title) != testSong.title {
			t.Errorf("YouTube title = %q, want %q", youtube.Title, testSong.title)
		}
		if strings.ToLower(apple.Title) != testSong.title {
			t.Errorf("Apple title = %q, want %q", apple.Title, testSong.title)
		}

		// All artists must exist and be clean
		if strings.TrimSpace(spotify.Artist) == "" {
			t.Error("Spotify artist must not be empty")
		}
		if strings.TrimSpace(youtube.Artist) == "" {
			t.Error("YouTube artist must not be empty")
		}
		if strings.TrimSpace(apple.Artist) == "" {
			t.Error("Apple artist must not be empty")
		}

		if strings.ToLower(spotify.Artist) != testSong.artist {
			t.Errorf("Spotify artist = %q, want %q", spotify.Artist, testSong.artist)
		}
		if strings.ToLower(youtube.Artist) != testSong.artist {
			t.Errorf("YouTube artist = %q, want %q", youtube.Artist, testSong.artist)
		}
		if strings.ToLower(apple.Artist) != testSong.artist {
			t.Errorf("Apple artist = %q, want %q", apple.Artist, testSong.artist)
		}

		// Test display format for all platforms
		spotifyDisplay := spotify.Title + " by " + spotify.Artist
		youtubeDisplay := youtube.Title + " by " + youtube.Artist
		appleDisplay := apple.Title + " by " + apple.Artist

		expectedDisplay := "bad guy by billie eilish"
		if strings.ToLower(spotifyDisplay) != expectedDisplay {
			t.Errorf("Spotify display = %q, want %q", spotifyDisplay, expectedDisplay)
		}
		if strings.ToLower(youtubeDisplay) != expectedDisplay {
			t.Errorf("YouTube display = %q, want %q", youtubeDisplay, expectedDisplay)
		}
		if strings.ToLower(appleDisplay) != expectedDisplay {
			t.Errorf("Apple display = %q, want %q", appleDisplay, expectedDisplay)
		}

		t.Logf("✅ Cross-platform comparison:")
		t.Logf("  Spotify: %s by %s → %s", spotify.Title, spotify.Artist, spotifyDisplay)
		t.Logf("  YouTube: %s by %s → %s", youtube.Title, youtube.Artist, youtubeDisplay)
		t.Logf("  Apple: %s by %s → %s", apple.Title, apple.Artist, appleDisplay)
	})
}

func TestDisplayFormatValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	testPlatforms := createTestPlatforms(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	t.Run("should produce clean display format without duplication", func(t *testing.T) {
		platforms := []struct {
			name string
			url  string
			fn   func(context.Context, string) (*SongInfo, error)
		}{
			{"Spotify", testSong.spotify, testPlatforms.spotify.ExtractSongInfo},
			{"YouTube", testSong.youtubeMusic, testPlatforms.youtube.ExtractSongInfo},
			{"Apple", testSong.appleMusic, testPlatforms.appleMusic.ExtractSongInfo},
		}

		for _, platform := range platforms {
			info, err := platform.fn(ctx, platform.url)
			if err != nil {
				t.Errorf("%s extraction failed: %v", platform.name, err)
				continue
			}

			if info == nil {
				t.Errorf("%s returned nil info", platform.name)
				continue
			}

			display := info.Title + " by " + info.Artist

			// Should not have duplicated artist
			lowerDisplay := strings.ToLower(display)
			artistCount := strings.Count(lowerDisplay, strings.ToLower(testSong.artist))
			if artistCount != 1 {
				t.Errorf("%s: artist appears %d times in display, want 1: %q",
					platform.name, artistCount, display)
			}

			// Should not have malformed patterns
			if strings.Contains(display, " - ") && strings.Contains(display, " by ") {
				// Check for "Artist - Title by Artist" pattern
				if strings.Index(display, " - ") < strings.Index(display, " by ") {
					t.Errorf("%s: malformed pattern 'Artist - Title by Artist': %q",
						platform.name, display)
				}
			}

			// Should not have "by" twice
			if strings.Count(strings.ToLower(display), " by ") > 1 {
				t.Errorf("%s: display contains 'by' multiple times: %q",
					platform.name, display)
			}

			t.Logf("✅ %s display format: %s", platform.name, display)
		}
	})
}

// ============================================================================
// TEST HELPERS
// ============================================================================

type testPlatforms struct {
	spotify    *Spotify
	youtube    *YouTube
	appleMusic *AppleMusic
}

func createTestPlatforms(t *testing.T) *testPlatforms {
	t.Helper()

	// Create logger
	logger, err := createLogger(false)
	if err != nil {
		t.Fatalf("Failed to create logger: %v", err)
	}

	// Create HTTP client
	httpClient := NewHTTPClient(&HTTPConfig{
		Timeout:       12 * time.Second,
		RetryAttempts: 2,
		RetryMinDelay: 150 * time.Millisecond,
		RetryMaxDelay: 350 * time.Millisecond,
		Debug:         false,
	}, logger)

	// Create caches
	urlCache := NewCache()
	negativeCache := NewCache()

	// Create circuit breakers
	spotifyBreaker := NewCircuitBreaker(&CircuitBreakerConfig{
		Name:     "Spotify",
		MaxFails: 5,
		Timeout:  60 * time.Second,
	}, logger)

	// Create platform implementations
	spotify := NewSpotify(
		httpClient,
		spotifyBreaker,
		urlCache,
		negativeCache,
		&SpotifyConfig{
			CacheTTL:         24 * time.Hour,
			NegativeCacheTTL: 5 * time.Minute,
			TextMirrorURL:    "https://r.jina.ai/",
			Debug:            false,
		},
		logger,
	)

	youtube := NewYouTube(
		httpClient,
		urlCache,
		&YouTubeConfig{
			CacheTTL: 24 * time.Hour,
			Debug:    false,
		},
		logger,
	)

	appleMusic := NewAppleMusic(
		httpClient,
		urlCache,
		&AppleMusicConfig{
			CacheTTL: 24 * time.Hour,
			Debug:    false,
		},
		logger,
	)

	return &testPlatforms{
		spotify:    spotify,
		youtube:    youtube,
		appleMusic: appleMusic,
	}
}
