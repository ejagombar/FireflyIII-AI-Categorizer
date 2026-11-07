package classifier

import (
	"context"
	"strings"
)

type Outcome string

const (
	Classified  Outcome = "CLASSIFIED"
	Assumed     Outcome = "ASSUMED"
	NeedsReview Outcome = "NEEDS_REVIEW"
)

type HistoricalEntry struct {
	TransactionID        string
	DestinationName      string
	Description          string
	CategoryName         string
	GroupKey             string  // effective lookup key — destination name, or description when destination is blank
	Amount               float64 // 0 means unknown; used for history-match confidence check
	DestinationAccountID string  // expense account ID from a previous classification (empty if unknown)
}

// GroupKey returns the field to use when grouping and looking up transaction
// history. When destination_name is absent or a generic placeholder, description
// is used instead so that past context is still surfaced for nameless payees.
func GroupKey(destinationName, description string) string {
	if IsBlankName(destinationName) {
		return description
	}
	return destinationName
}

// blankNames are payee names that carry no merchant information: placeholders
// written by Firefly III, by importers, or by banks that only stamp the card
// terminal. Grouping history on one of them would merge unrelated merchants
// into a single bucket, so they are never treated as a payee.
var blankNames = map[string]bool{
	"":                    true,
	"-":                   true,
	"--":                  true,
	"n/a":                 true,
	"n.a.":                true,
	"no name":             true,
	"unknown":             true,
	"unknown payee":       true,
	"unknown destination": true,
	"no registrado":       true,
	"pos purchase":        true,
	"card purchase":       true,
	"purchase":            true,
	"transfer":            true,
}

// IsBlankName reports whether a payee name identifies no merchant: it is empty,
// a known placeholder, wrapped in parentheses (Firefly III's convention for a
// placeholder account), or mentions "unknown".
func IsBlankName(s string) bool {
	norm := strings.ToLower(strings.TrimSpace(s))
	if blankNames[norm] {
		return true
	}
	if strings.Contains(norm, "unknown") || strings.Contains(norm, "no name") {
		return true
	}
	return strings.HasPrefix(norm, "(") && strings.HasSuffix(norm, ")")
}

// Category is a classifiable Firefly III category with optional notes that
// provide the LLM with extra context about what belongs in each category.
type Category struct {
	Name  string
	Notes string
}

// AccountCandidate is an expense account that the LLM can match or decide to create.
type AccountCandidate struct {
	ID   string
	Name string
}

// DestinationResult captures the LLM's decision about the destination account.
type DestinationResult struct {
	Name       string // desired account name
	Action     string // "MATCH" or "CREATE"
	Confidence string // "CLASSIFIED" or "ASSUMED"
}

type Request struct {
	Categories      []Category
	DestinationName string
	Description     string
	Amount          *float64
	History         []HistoricalEntry

	// ExpenseAccounts is set when destination-account matching is enabled.
	ExpenseAccounts []AccountCandidate

	// DestinationMatching signals to the classifier that the system prompt
	// and response parsing should include destination-account logic.
	DestinationMatching bool

	// CategoryOnly signals that only category classification is needed
	// (destination info is stripped from the prompt). When true, the classifier
	// won't bother computing a destination even if expense accounts are provided.
	CategoryOnly bool

	// SystemPromptOverride replaces the built-in system prompt when non-empty.
	// Used for special-purpose LLM calls like transfer destination suggestion.
	SystemPromptOverride string
}

type Result struct {
	Outcome     Outcome
	Category    string // empty if NeedsReview
	Reason      string
	Assumption  string // non-empty only when Assumed
	RawPrompt   string
	RawResponse string

	// Destination is set when destination-account matching was enabled
	// and the LLM returned a valid destination suggestion.
	Destination *DestinationResult
}

type Classifier interface {
	Classify(ctx context.Context, req Request) (Result, error)
}
