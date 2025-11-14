// Package search provides cross-platform music search orchestration
package search

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"telegram-music-bot/internal/platform"
)

var (
	reFirstURL = regexp.MustCompile(`https?://[^\s]+`)
)

// Orchestrator coordinates search across multiple music platforms
type Orchestrator struct {
	spotify    platform.Platform
	youtube    platform.Platform
	appleMusic platform.Platform
}

// New creates a new search orchestrator
func New(spotify, youtube, appleMusic platform.Platform) *Orchestrator {
	return &Orchestrator{
		spotify:    spotify,
		youtube:    youtube,
		appleMusic: appleMusic,
	}
}

// ExtractSongInfo attempts to extract song info from text containing a URL
func (o *Orchestrator) ExtractSongInfo(ctx context.Context, text string) (*platform.SongInfo, error) {
	text = strings.TrimSpace(text)
	urlStr := firstURL(text)
	if urlStr == "" {
		return nil, nil
	}

	// Detect platform from URL
	host := strings.ToLower(urlStr)

	// Handle Spotify short links (spotify.link and spotify.app.link)
	if strings.Contains(host, "spotify.link") || strings.Contains(host, "spotify.app.link") {
		resolvedURL, err := resolveSpotifyShortLink(ctx, urlStr)
		if err == nil && resolvedURL != "" {
			return o.spotify.ExtractSongInfo(ctx, resolvedURL)
		}
		// If resolution failed, return error
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
func (o *Orchestrator) FindOnAllPlatforms(ctx context.Context, info *platform.SongInfo) *platform.MusicLinks {
	links := &platform.MusicLinks{}

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
			if u := platform.ToYouTubeMusicURL(info.OriginalURL); u != "" {
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
func (o *Orchestrator) EnrichArtistInfo(ctx context.Context, info *platform.SongInfo, links *platform.MusicLinks) {
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

// resolveSpotifyShortLink follows a spotify.link redirect and extracts the full Spotify URL
func resolveSpotifyShortLink(ctx context.Context, shortURL string) (string, error) {
	// Create HTTP client that follows redirects
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

	// After following redirects, check the final URL
	finalURL := resp.Request.URL.String()
	if strings.Contains(finalURL, "open.spotify.com/track/") {
		// Clean up the URL - remove query parameters
		if idx := strings.Index(finalURL, "?"); idx != -1 {
			finalURL = finalURL[:idx]
		}
		return finalURL, nil
	}

	// Fallback: extract URL from HTML body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Look for Spotify track URL in the HTML
	re := regexp.MustCompile(`open\.spotify\.com/track/([A-Za-z0-9]+)`)
	match := re.FindStringSubmatch(string(body))
	if len(match) > 1 {
		trackID := match[1]
		return fmt.Sprintf("https://open.spotify.com/track/%s", trackID), nil
	}

	return "", fmt.Errorf("could not extract Spotify URL from short link")
}
