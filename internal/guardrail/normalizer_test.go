package guardrail

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestNormalize exercises the full normalisation pipeline against a broad set
// of inputs. Each case specifies the raw prompt and the exact canonical string
// expected after all six pipeline stages have been applied.
func TestNormalize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// ------------------------------------------------------------------ //
		// Edge cases
		// ------------------------------------------------------------------ //
		{
			name: "empty string",
			in:   "",
			want: "",
		},
		{
			name: "pure whitespace",
			in:   "   \t\n  ",
			want: "",
		},
		{
			name: "only punctuation",
			in:   "!@#$%^&*()",
			want: "",
		},
		{
			name: "only digits",
			in:   "12345",
			want: "12345",
		},
		{
			name: "only letters",
			in:   "hello",
			want: "hello",
		},
		{
			name: "mixed case letters preserved as lowercase",
			in:   "Hello World",
			want: "hello world",
		},
		{
			name: "leading and trailing whitespace trimmed",
			in:   "  analyze this  ",
			want: "analyze this",
		},
		{
			name: "internal whitespace collapsed",
			in:   "analyze   this   data",
			want: "analyze this data",
		},
		{
			name: "tabs and newlines collapsed to single space",
			in:   "analyze\tthis\ndata",
			want: "analyze this data",
		},

		// ------------------------------------------------------------------ //
		// Punctuation stripping
		// ------------------------------------------------------------------ //
		{
			name: "commas and periods removed",
			in:   "analyze this, please. it is important.",
			want: "analyze this please it is important",
		},
		{
			name: "exclamation and question marks removed",
			in:   "What is the capital of France?",
			want: "what is the capital of france",
		},
		{
			name: "apostrophes in contractions removed",
			in:   "don't stop the music",
			want: "dont stop the music",
		},
		{
			name: "hyphens between words removed",
			in:   "state-of-the-art model",
			want: "stateoftheart model",
		},
		{
			name: "parentheses removed",
			in:   "summarize (briefly) this document",
			want: "summarize briefly this document",
		},

		// ------------------------------------------------------------------ //
		// UUID stripping
		// ------------------------------------------------------------------ //
		{
			name: "lowercase UUID stripped",
			in:   "analyze data for 550e8400-e29b-41d4-a716-446655440000",
			want: "analyze data for",
		},
		{
			name: "uppercase UUID stripped",
			in:   "record 550E8400-E29B-41D4-A716-446655440000 result",
			want: "record result",
		},
		{
			name: "mixed case UUID stripped",
			in:   "process 550e8400-E29B-41d4-A716-446655440000",
			want: "process",
		},
		{
			name: "multiple UUIDs stripped",
			in:   "link 550e8400-e29b-41d4-a716-446655440000 to 6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			want: "link to",
		},
		{
			name: "UUID embedded in sentence stripped",
			in:   "The item 550e8400-e29b-41d4-a716-446655440000 was updated",
			want: "the item was updated",
		},
		{
			name: "no UUID — string unchanged by UUID step",
			in:   "analyze climate data",
			want: "analyze climate data",
		},

		// ------------------------------------------------------------------ //
		// ISO 8601 date and datetime stripping
		// ------------------------------------------------------------------ //
		{
			name: "date only stripped",
			in:   "data from 2024-03-15",
			want: "data from",
		},
		{
			name: "datetime with T separator stripped (lowercased first)",
			in:   "event at 2024-03-15T14:30:00",
			want: "event at",
		},
		{
			name: "datetime with Z timezone stripped",
			in:   "timestamp 2024-03-15T14:30:00Z",
			want: "timestamp",
		},
		{
			name: "datetime with fractional seconds stripped",
			in:   "logged at 2024-03-15T14:30:00.123Z",
			want: "logged at",
		},
		{
			name: "datetime with positive offset stripped",
			in:   "scheduled 2024-03-15T14:30:00+05:30",
			want: "scheduled",
		},
		{
			name: "datetime with negative offset stripped",
			in:   "deadline 2024-03-15T14:30:00-08:00",
			want: "deadline",
		},
		{
			name: "multiple dates stripped",
			in:   "between 2024-01-01 and 2024-12-31",
			want: "between and",
		},
		{
			name: "UUID and date both stripped",
			in:   "item 550e8400-e29b-41d4-a716-446655440000 on 2024-03-15",
			want: "item on",
		},

		// ------------------------------------------------------------------ //
		// Filler prefix stripping — prefix only, not mid-sentence
		// ------------------------------------------------------------------ //
		{
			name: "please prefix stripped",
			in:   "Please analyze this",
			want: "analyze this",
		},
		{
			name: "can you prefix stripped",
			in:   "Can you summarize this document",
			want: "summarize this document",
		},
		{
			name: "could you prefix stripped",
			in:   "Could you explain quantum entanglement",
			want: "explain quantum entanglement",
		},
		{
			name: "would you prefix stripped",
			in:   "Would you translate this text",
			want: "translate this text",
		},
		{
			name: "will you prefix stripped",
			in:   "Will you review my code",
			want: "review my code",
		},
		{
			name: "i need you to prefix stripped",
			in:   "I need you to write a function",
			want: "write a function",
		},
		{
			name: "i want you to prefix stripped",
			in:   "I want you to explain this",
			want: "explain this",
		},
		{
			name: "help me prefix stripped",
			in:   "Help me debug this error",
			want: "debug this error",
		},
		{
			name: "chained fillers stripped iteratively",
			in:   "Please can you analyze this",
			want: "analyze this",
		},
		{
			name: "triple chained filler stripped iteratively",
			in:   "Please could you help me do this",
			want: "do this",
		},

		// ------------------------------------------------------------------ //
		// CRITICAL: mid-sentence fillers must NOT be stripped
		// ------------------------------------------------------------------ //
		{
			name: "please mid-sentence not stripped",
			in:   "analyze this data please",
			want: "analyze this data please",
		},
		{
			name: "can you mid-sentence not stripped",
			in:   "tell me if can you do this",
			want: "tell me if can you do this",
		},
		{
			name: "analyze as first semantic word not stripped",
			in:   "Please analyze this document carefully",
			want: "analyze this document carefully",
		},
		{
			name: "help mid-sentence not stripped",
			in:   "this will help me understand",
			want: "this will help me understand",
		},
		{
			name: "word please inside a sentence stays",
			in:   "do this please and that please",
			want: "do this please and that please",
		},

		// ------------------------------------------------------------------ //
		// Realistic full-sentence prompts
		// ------------------------------------------------------------------ //
		{
			name: "realistic data analysis prompt with uuid and date",
			in:   "Please analyze this data from 2024-03-15 for UUID 550e8400-e29b-41d4-a716-446655440000",
			want: "analyze this data from for uuid",
		},
		{
			name: "realistic question stripped to semantic core",
			in:   "Could you summarize the document uploaded on 2023-11-22T09:00:00Z?",
			want: "summarize the document uploaded on",
		},
		{
			name: "all-caps sentence normalised",
			in:   "PLEASE ANALYZE THIS DATA",
			want: "analyze this data",
		},
		{
			name: "numbers retained",
			in:   "calculate the 42nd fibonacci number",
			want: "calculate the 42nd fibonacci number",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Normalize(tc.in)
			if got != tc.want {
				t.Errorf("Normalize(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalize_Idempotent verifies that applying Normalize twice produces the
// same result as applying it once. An idempotent normaliser is essential for
// correct deduplication: a second pass must never further transform output
// that is already in canonical form.
func TestNormalize_Idempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"Please analyze data from 2024-01-01",
		"Could you help me with UUID 550e8400-e29b-41d4-a716-446655440000?",
		"  multiple   spaces  and\ttabs  ",
		"",
		"already normalized",
	}

	for _, in := range inputs {
		once := Normalize(in)
		twice := Normalize(once)
		if once != twice {
			t.Errorf("Normalize not idempotent for %q:\n  first  = %q\n  second = %q",
				in, once, twice)
		}
	}
}

// TestHash verifies that Hash returns consistent SHA-256 hex digests and that
// semantically different strings produce different hashes.
func TestHash(t *testing.T) {
	t.Parallel()

	t.Run("deterministic — same input same hash", func(t *testing.T) {
		t.Parallel()
		h1 := Hash("analyze climate data")
		h2 := Hash("analyze climate data")
		if h1 != h2 {
			t.Errorf("Hash not deterministic: %q vs %q", h1, h2)
		}
	})

	t.Run("empty string hashes to sha256 of empty", func(t *testing.T) {
		t.Parallel()
		raw := sha256.Sum256([]byte(""))
		want := hex.EncodeToString(raw[:])
		got := Hash("")
		if got != want {
			t.Errorf("Hash(\"\") = %q, want %q", got, want)
		}
	})

	t.Run("different strings produce different hashes", func(t *testing.T) {
		t.Parallel()
		h1 := Hash("analyze climate data")
		h2 := Hash("summarize the report")
		if h1 == h2 {
			t.Error("different strings unexpectedly produced the same hash")
		}
	})

	t.Run("hash length is 64 hex characters (sha256)", func(t *testing.T) {
		t.Parallel()
		h := Hash("some prompt")
		if len(h) != 64 {
			t.Errorf("hash length = %d, want 64", len(h))
		}
	})

	t.Run("hash is lowercase hex", func(t *testing.T) {
		t.Parallel()
		h := Hash("some prompt")
		if h != strings.ToLower(h) {
			t.Errorf("hash is not lowercase: %q", h)
		}
	})

	t.Run("pool recycling — concurrent calls return consistent results", func(t *testing.T) {
		t.Parallel()
		const prompt = "analyze this"
		expected := Hash(prompt)
		// Fire 50 goroutines to exercise pool contention.
		errs := make(chan string, 50)
		for range 50 {
			go func() {
				if got := Hash(prompt); got != expected {
					errs <- got
				} else {
					errs <- ""
				}
			}()
		}
		for range 50 {
			if bad := <-errs; bad != "" {
				t.Errorf("concurrent Hash returned %q, want %q", bad, expected)
			}
		}
	})
}

// TestNormalizeAndHash verifies the end-to-end contract used by
// GuardrailMiddleware: two prompts that are semantically identical (modulo
// ephemeral artefacts) must produce the same hash after normalisation.
func TestNormalizeAndHash(t *testing.T) {
	t.Parallel()

	equivalentPairs := [][2]string{
		{
			"Please analyze data from 2024-03-15 for item 550e8400-e29b-41d4-a716-446655440000",
			"Can you analyze data from 2023-07-04 for item 6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		},
		{
			"Could you summarize this?",
			"Summarize this",
		},
		{
			"PLEASE HELP ME ANALYZE THE REPORT",
			"analyze the report",
		},
	}

	for _, pair := range equivalentPairs {
		h1 := Hash(Normalize(pair[0]))
		h2 := Hash(Normalize(pair[1]))
		if h1 != h2 {
			t.Errorf("expected same hash for semantically equivalent prompts:\n  %q → %q\n  %q → %q",
				pair[0], Normalize(pair[0]),
				pair[1], Normalize(pair[1]),
			)
		}
	}
}
