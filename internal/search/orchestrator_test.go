package search

import (
	"context"
	"testing"
	"time"
)

func TestResolveSpotifyShortLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test with a real spotify.link URL
	shortURL := "https://spotify.link/XUscIFZSvXb"
	resolved, err := resolveSpotifyShortLink(ctx, shortURL)

	if err != nil {
		t.Logf("Note: Short link resolution failed (this might be expected in CI): %v", err)
		// Don't fail the test as this requires network access
		return
	}

	if resolved == "" {
		t.Error("Expected non-empty resolved URL")
		return
	}

	if !containsString(resolved, "open.spotify.com/track/") {
		t.Errorf("Expected resolved URL to contain 'open.spotify.com/track/', got: %s", resolved)
	}

	t.Logf("Successfully resolved: %s -> %s", shortURL, resolved)
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && containsAtPosition(s, substr))
}

func containsAtPosition(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
