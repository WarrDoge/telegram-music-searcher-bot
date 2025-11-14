// Package util provides utility functions for text processing and matching
package util

import (
	"regexp"
	"strings"
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

// GetMidDotSepRegex returns the mid-dot separator regex
func GetMidDotSepRegex() *regexp.Regexp {
	return reMidDotSep
}

// GetDashRegex returns the dash separator regex
func GetDashRegex() *regexp.Regexp {
	return reDash
}

// GetAppleMusicSuffixRegex returns the Apple Music suffix regex
func GetAppleMusicSuffixRegex() *regexp.Regexp {
	return reAppleMusicSuffix
}

// GetFancyQuotes returns the fancy quotes map
func GetFancyQuotes() map[string]string {
	return fancyQuotes
}
