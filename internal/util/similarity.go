package util

import (
	"github.com/texttheater/golang-levenshtein/levenshtein"
)

// CalculateSimilarity returns a similarity score between 0 and 1
// Uses Levenshtein distance normalized by the longer string length
func CalculateSimilarity(s1, s2 string) float64 {
	if s1 == "" || s2 == "" {
		return 0
	}

	// Normalize for comparison
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
