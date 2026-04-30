// Package guardrail implements the prompt-safety layer of the AgentMesh
// request pipeline. It sits immediately inside the authentication middleware
// and runs before any token budget check or upstream call is made.
//
// Responsibilities:
//
//   - Body-size enforcement: requests larger than 1 MiB are rejected before
//     any parsing occurs.
//   - Streaming block: requests with stream:true are rejected with 501 because
//     the v1 ResponseRecorder does not support chunked transfer.
//   - Prompt normalisation: ephemeral artefacts (UUIDs, timestamps,
//     punctuation, filler prefixes) are stripped so that semantically identical
//     prompts produce the same SHA-256 hash regardless of incidental variation.
//   - Loop detection: a sliding-window circuit breaker (Breaker) trips with
//     429 LOOP_DETECTED when the same normalised hash appears more than
//     MaxIdenticalHash times within WindowSize for the same tenant+agent pair.
//
// All normalisation and hashing is allocation-efficient: compiled regexes and
// a sync.Pool of hash.Hash values are reused across requests.
package guardrail

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"regexp"
	"strings"
	"sync"
)

// Package-level compiled regexes — compiled once, used concurrently.
var (
	// uuidRe matches standard UUIDs (v1–v5) regardless of case.
	// The (?i) flag is redundant after lowercasing but kept for robustness when
	// the regex is reused in other contexts.
	uuidRe = regexp.MustCompile(
		`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
	)

	// iso8601Re matches ISO 8601 dates and datetimes.
	// Applied after lowercasing, so the 'T' separator appears as 't'.
	//   date only:      2023-01-15
	//   with time:      2023-01-15t14:30:00
	//   with fraction:  2023-01-15t14:30:00.123
	//   with zone:      2023-01-15t14:30:00z  /  …+05:30
	iso8601Re = regexp.MustCompile(
		`\d{4}-\d{2}-\d{2}` +
			`(?:t\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:z|[+-]\d{2}(?::\d{2})?)?)?`,
	)

	// punctRe removes any character that is not a lowercase letter, digit, or
	// whitespace. Applied after lowercasing so uppercase letters are already gone.
	punctRe = regexp.MustCompile(`[^a-z0-9\s]`)

	// spaceRe collapses any run of whitespace characters to a single ASCII space.
	spaceRe = regexp.MustCompile(`\s+`)
)

// fillerPrefixes lists common conversational openers that add no semantic value.
// These are stripped ONLY when they appear at the beginning of the normalised
// string via strings.TrimPrefix, so mid-string occurrences (e.g. the word
// "please" inside "analyze this data") are left completely untouched.
var fillerPrefixes = []string{
	"please ",
	"can you ",
	"could you ",
	"would you ",
	"will you ",
	"i need you to ",
	"i want you to ",
	"help me ",
}

// hashPool recycles sha256 hash.Hash values across calls to Hash, eliminating
// per-call heap allocation for the hasher itself.
var hashPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// Normalize returns a canonical, deduplication-ready form of prompt by applying
// the following pipeline in order:
//
//  1. Lowercase the entire string.
//  2. Strip UUIDs (case-insensitive regex).
//  3. Strip ISO 8601 date/datetime strings.
//  4. Strip punctuation (retain letters, digits, and whitespace).
//  5. Collapse runs of whitespace to a single space and trim edges.
//  6. Iteratively strip known conversational prefix fillers using
//     strings.TrimPrefix — never a global replacer — so that semantic verbs
//     such as "analyze" or "please" appearing mid-string are preserved.
func Normalize(prompt string) string {
	s := strings.ToLower(prompt)
	s = uuidRe.ReplaceAllString(s, "")
	s = iso8601Re.ReplaceAllString(s, "")
	s = punctRe.ReplaceAllString(s, "")
	s = spaceRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)

	// Iteratively strip prefix fillers. Each removal may expose a new filler
	// at the start (e.g. "please can you " → "can you " → ""), so we loop
	// until a full pass produces no change.
	for changed := true; changed; {
		changed = false
		for _, prefix := range fillerPrefixes {
			trimmed := strings.TrimPrefix(s, prefix)
			if trimmed != s {
				s = strings.TrimSpace(trimmed)
				changed = true
			}
		}
	}

	return s
}

// Hash returns the lowercase hex-encoded SHA-256 digest of normalized.
// It acquires a hash.Hash from a sync.Pool to avoid per-call allocation on
// the hot path; only the unavoidable string→[]byte conversion remains.
func Hash(normalized string) string {
	h := hashPool.Get().(hash.Hash)
	h.Reset()
	_, _ = h.Write([]byte(normalized))
	sum := h.Sum(nil)
	hashPool.Put(h)
	return hex.EncodeToString(sum)
}
