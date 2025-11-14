// Package platform provides interfaces and implementations for music platform integration
package platform

import (
	"context"
)

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
	// ExtractSongInfo extracts song metadata from a URL
	ExtractSongInfo(ctx context.Context, url string) (*SongInfo, error)

	// Platform returns the name of the platform
	Platform() string
}

// Searcher searches for a song on a platform
type Searcher interface {
	// Search searches for a song and returns its URL
	Search(ctx context.Context, info *SongInfo) (string, error)

	// Platform returns the name of the platform
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
