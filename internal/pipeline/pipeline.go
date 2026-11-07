package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/openaccountants/firefly-iii-ai-categorize/internal/cache"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/classifier"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/firefly"
	"github.com/openaccountants/firefly-iii-ai-categorize/internal/job"
)

const categoryTTL = 5 * time.Minute

// historyMatchLookup is how many past entries to fetch when checking for a
// confident history-based match (larger than the AI prompt context limit).
const historyMatchLookup = 20

// historyMatchMinCount is the minimum number of agreeing past transactions
// required to skip the AI call and auto-classify from history.
const historyMatchMinCount = 2

// historyMatchMaxRatio is the maximum ratio between the new transaction amount
// and a historical amount for the two to be considered "same ballpark".
// 2.0 means the amounts may differ by at most 2× in either direction.
const historyMatchMaxRatio = 2.0

// Pipeline runs the classify-and-update flow for a single transaction.
// It is shared by both the webhook handler and the batch runner.
type Pipeline struct {
	firefly    *firefly.Client
	classifier classifier.Classifier
	cache      *cache.Cache
	registry   *job.Registry
	contextN   int

	destinationMatch bool

	// Short-lived category cache to avoid re-fetching on every job in a batch.
	catMu      sync.RWMutex
	catCache   []firefly.Category
	catFetched time.Time

	// Short-lived expense-account cache (only used when destinationMatch is true).
	acctMu      sync.RWMutex
	acctCache   []firefly.Account
	acctFetched time.Time
}

func New(
	fc *firefly.Client,
	cl classifier.Classifier,
	ca *cache.Cache,
	reg *job.Registry,
	contextN int,
	destinationMatch bool,
) *Pipeline {
	return &Pipeline{
		firefly:          fc,
		classifier:       cl,
		cache:            ca,
		registry:         reg,
		contextN:         contextN,
		destinationMatch: destinationMatch,
	}
}

// DestinationMatchEnabled returns whether destination matching is enabled in config.
func (p *Pipeline) DestinationMatchEnabled() bool {
	return p.destinationMatch
}

// RunOptions controls which parts of the pipeline execute.
type RunOptions struct {
	ClassifyCategory bool // if true, run category classification
	MatchDestination bool // if true, run destination matching
}

// Run executes the full classification pipeline for a queued job.
func (p *Pipeline) Run(ctx context.Context, j *job.Job, txn firefly.Transaction) error {
	return p.RunWithOptions(ctx, j, txn, RunOptions{
		ClassifyCategory: true,
		MatchDestination: p.destinationMatch,
	})
}

// RunWithOptions executes the classification pipeline with fine-grained control
// over which parts (category classification vs destination matching) are active.
// txn must contain every split in the group (see firefly.Client.UpdateTransaction).
func (p *Pipeline) RunWithOptions(ctx context.Context, j *job.Job, txn firefly.Transaction, opts RunOptions) error {
	p.registry.SetInProgress(j.ID)
	transactionID := txn.ID

	categories, err := p.getCategories(ctx)
	if err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("get categories: %w", err)
	}

	clCats := make([]classifier.Category, len(categories))
	for i, c := range categories {
		clCats[i] = classifier.Category{Name: c.Name, Notes: c.Notes}
	}

	var expenseAccounts []firefly.Account
	if opts.MatchDestination {
		var err error
		expenseAccounts, err = p.getExpenseAccounts(ctx)
		if err != nil {
			slog.Warn("failed to fetch expense accounts, continuing without destination matching", "error", err)
		}
	}
	clAccounts := make([]classifier.AccountCandidate, 0, len(expenseAccounts))
	for _, a := range expenseAccounts {
		// Placeholder payees such as "(unknown destination)" are not real
		// merchants. Offering them as candidates invites the model to MATCH
		// unrelated withdrawals onto that single fake account.
		if classifier.IsBlankName(a.Name) {
			continue
		}
		clAccounts = append(clAccounts, classifier.AccountCandidate{ID: a.ID, Name: a.Name})
	}

	gkey := classifier.GroupKey(j.DestinationName, j.Description)

	// Fetch enough history for both the match check and the AI prompt.
	lookupLimit := historyMatchLookup
	if p.contextN > lookupLimit {
		lookupLimit = p.contextN
	}
	history := excludeTransaction(p.cache.GetHistory(ctx, gkey, lookupLimit), transactionID)

	// Try independent history matches for category and destination.
	// When both match, the LLM call can be skipped entirely. When only one
	// matches, the LLM is still called but the matched value overrides the
	// LLM's result for that field.
	historyMatchCat, histCatCount := tryHistoryMatch(history, j.Amount)
	historyMatchDestID, histDestCount := tryDestinationHistoryMatch(history, j.Amount)

	// Validate the destination history match against the current account list.
	if historyMatchDestID != "" {
		found := false
		for _, a := range expenseAccounts {
			if a.ID != historyMatchDestID {
				continue
			}
			// Never re-apply a placeholder payee: doing so would keep
			// collapsing unrelated merchants onto the same fake account.
			found = !classifier.IsBlankName(a.Name)
			break
		}
		if !found {
			historyMatchDestID = ""
			histDestCount = 0
		}
	}

	// If both category and destination have history matches, skip the LLM entirely.
	// Also skip when we only care about destination and it has a history match.
	if (!opts.ClassifyCategory || historyMatchCat != "") && (historyMatchDestID != "" || !opts.MatchDestination) {
		categoryID := ""
		for _, c := range categories {
			if c.Name == historyMatchCat {
				categoryID = c.ID
				break
			}
		}
		reason := fmt.Sprintf("Auto-matched from %d previous transactions with the same payee.", histCatCount)
		outcome := firefly.UpdateOutcome{
			Outcome:       string(classifier.Classified),
			Category:      historyMatchCat,
			CategoryID:    categoryID,
			DestinationID: historyMatchDestID,
			Reason:        reason,
		}
		if historyMatchDestID != "" {
			outcome.DestConfidence = "CLASSIFIED"
		}
		if err := p.firefly.UpdateTransaction(ctx, txn, outcome); err != nil {
			p.registry.SetFailed(j.ID, err.Error())
			return fmt.Errorf("update transaction: %w", err)
		}

		var destName, destAction string
		if historyMatchDestID != "" {
			destAction = "MATCH"
			for _, a := range expenseAccounts {
				if a.ID == historyMatchDestID {
					destName = a.Name
					break
				}
			}
		}
		p.registry.SetFinished(j.ID, string(classifier.Classified), historyMatchCat, reason, "", "", "", destName, destAction)
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:        transactionID,
			DestinationName:      j.DestinationName,
			Description:          j.Description,
			CategoryName:         historyMatchCat,
			GroupKey:             gkey,
			Amount:               derefAmount(j.Amount),
			DestinationAccountID: historyMatchDestID,
		})
		slog.Info("transaction history-matched",
			"id", transactionID,
			"category", historyMatchCat,
			"catMatches", histCatCount,
			"destMatches", histDestCount,
		)
		return nil
	}

	// Trim history to the configured context limit for the AI prompt.
	promptHistory := history
	if len(promptHistory) > p.contextN {
		promptHistory = promptHistory[:p.contextN]
	}

	result, err := p.classifier.Classify(ctx, classifier.Request{
		Categories:           clCats,
		DestinationName:      j.DestinationName,
		Description:          j.Description,
		Amount:               j.Amount,
		History:              promptHistory,
		ExpenseAccounts:      clAccounts,
		DestinationMatching:  opts.MatchDestination,
		CategoryOnly:         opts.ClassifyCategory && !opts.MatchDestination,
	})
	if err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("classify: %w", err)
	}

	categoryID := ""
	for _, c := range categories {
		if c.Name == result.Category {
			categoryID = c.ID
			break
		}
	}

	outcome := firefly.UpdateOutcome{
		Outcome:    string(result.Outcome),
		Category:   result.Category,
		CategoryID: categoryID,
		Reason:     result.Reason,
		Assumption: result.Assumption,
	}

	// Override category with history match when available — the LLM was only
	// needed for destination (or the other way around).
	if opts.ClassifyCategory && historyMatchCat != "" {
		for _, c := range categories {
			if c.Name == historyMatchCat {
				outcome.CategoryID = c.ID
				break
			}
		}
		outcome.Category = historyMatchCat
		outcome.Outcome = string(classifier.Classified)
		outcome.Reason = fmt.Sprintf("Auto-matched from %d previous transactions. AI was consulted for destination.", histCatCount)
	}

	// Process destination account result (when enabled and valid).
	var destAccount string
	var destAction string
	if result.Destination != nil {
		destAccount = result.Destination.Name
		destAction = result.Destination.Action

		switch result.Destination.Action {
		case "MATCH":
			for _, a := range expenseAccounts {
				if strings.EqualFold(a.Name, result.Destination.Name) {
					outcome.DestinationID = a.ID
					break
				}
			}
			if outcome.DestinationID == "" {
				slog.Warn("destination MATCH failed to find account", "name", result.Destination.Name)
				destAccount = ""
				destAction = ""
			} else {
				outcome.DestConfidence = result.Destination.Confidence
			}
		case "CREATE":
			if result.Destination.Confidence != "CLASSIFIED" {
				// CREATE is only attempted when the LLM is confident.
				slog.Info("destination CREATE skipped — confidence is ASSUMED", "name", result.Destination.Name)
				destAccount = ""
				destAction = ""
				break
			}
			created, err := p.firefly.CreateExpenseAccount(ctx, result.Destination.Name)
			if err != nil {
				slog.Error("failed to create expense account", "name", result.Destination.Name, "error", err)
				destAccount = ""
				destAction = ""
				break
			}
			outcome.DestinationID = created.ID
			outcome.DestConfidence = result.Destination.Confidence
			slog.Info("created expense account", "name", created.Name, "id", created.ID)

			// Append to in-memory cache so subsequent jobs in the same batch
			// see the new account without waiting for the 5-minute TTL.
			p.acctMu.Lock()
			p.acctCache = append(p.acctCache, created)
			p.acctMu.Unlock()
		}
	}

	// Override destination with history match when the LLM didn't produce one
	// (or produced a weaker one) but history has a confident match.
	if historyMatchDestID != "" && outcome.DestinationID == "" {
		outcome.DestinationID = historyMatchDestID
		outcome.DestConfidence = "CLASSIFIED"
		destAction = "MATCH"
		for _, a := range expenseAccounts {
			if a.ID == historyMatchDestID {
				destAccount = a.Name
				break
			}
		}
	}

	if err := p.firefly.UpdateTransaction(ctx, txn, outcome); err != nil {
		p.registry.SetFailed(j.ID, err.Error())
		return fmt.Errorf("update transaction: %w", err)
	}

	// Use history override values for the finished job when they were applied.
	finishedOutcome := string(result.Outcome)
	finishedCategory := result.Category
	finishedReason := result.Reason
	if !opts.ClassifyCategory {
		finishedOutcome = string(classifier.Classified)
		finishedCategory = ""
		finishedReason = "Destination-only matching (category not evaluated)."
	}
	if opts.ClassifyCategory && historyMatchCat != "" {
		finishedOutcome = string(classifier.Classified)
		finishedCategory = historyMatchCat
		finishedReason = outcome.Reason
	}

	p.registry.SetFinished(
		j.ID,
		finishedOutcome,
		finishedCategory,
		finishedReason,
		result.Assumption,
		result.RawPrompt,
		result.RawResponse,
		destAccount,
		destAction,
	)

	cachedCategory := finishedCategory
	if cachedCategory == "" && opts.ClassifyCategory {
		cachedCategory = result.Category
	}
	if cachedCategory != "" || opts.MatchDestination {
		p.cache.Append(classifier.HistoricalEntry{
			TransactionID:        transactionID,
			DestinationName:      j.DestinationName,
			Description:          j.Description,
			CategoryName:         cachedCategory,
			GroupKey:             gkey,
			Amount:               derefAmount(j.Amount),
			DestinationAccountID: outcome.DestinationID,
		})
	}

	slog.Info("transaction classified",
		"id", transactionID,
		"outcome", result.Outcome,
		"category", result.Category,
	)
	return nil
}

// tryHistoryMatch returns the best category from history if at least
// historyMatchMinCount entries agree on it with no tie from another category.
// Entries whose amounts fall outside the ballpark of the new transaction are
// excluded from the vote; if either amount is unknown (zero), the amount check
// is skipped for that entry so the vote still counts.
func tryHistoryMatch(history []classifier.HistoricalEntry, amount *float64) (category string, count int) {
	votes := make(map[string]int)
	for _, h := range history {
		if h.CategoryName == "" {
			continue
		}
		if amount != nil && *amount > 0 && h.Amount > 0 {
			hi := math.Max(*amount, h.Amount)
			lo := math.Min(*amount, h.Amount)
			if hi/lo > historyMatchMaxRatio {
				continue
			}
		}
		votes[h.CategoryName]++
	}

	best, bestCount := "", 0
	for cat, n := range votes {
		if n > bestCount {
			best, bestCount = cat, n
		}
	}
	if bestCount < historyMatchMinCount {
		return "", 0
	}
	// Reject ties: another category must not have an equal vote count.
	for cat, n := range votes {
		if cat != best && n >= bestCount {
			return "", 0
		}
	}
	return best, bestCount
}

// tryDestinationHistoryMatch returns the best destination account ID from history
// if at least historyMatchMinCount entries agree on it with no tie. The same
// amount-ballpark filtering as tryHistoryMatch applies.
func tryDestinationHistoryMatch(history []classifier.HistoricalEntry, amount *float64) (accountID string, count int) {
	votes := make(map[string]int)
	for _, h := range history {
		if h.DestinationAccountID == "" {
			continue
		}
		if amount != nil && *amount > 0 && h.Amount > 0 {
			hi := math.Max(*amount, h.Amount)
			lo := math.Min(*amount, h.Amount)
			if hi/lo > historyMatchMaxRatio {
				continue
			}
		}
		votes[h.DestinationAccountID]++
	}

	best, bestCount := "", 0
	for id, n := range votes {
		if n > bestCount {
			best, bestCount = id, n
		}
	}
	if bestCount < historyMatchMinCount {
		return "", 0
	}
	for id, n := range votes {
		if id != best && n >= bestCount {
			return "", 0
		}
	}
	return best, bestCount
}

func derefAmount(a *float64) float64 {
	if a == nil {
		return 0
	}
	return *a
}

// excludeTransaction removes any history entries that belong to the transaction
// currently being classified, preventing it from influencing its own result
// during re-categorization runs.
func excludeTransaction(history []classifier.HistoricalEntry, transactionID string) []classifier.HistoricalEntry {
	filtered := history[:0]
	for _, h := range history {
		if h.TransactionID != transactionID {
			filtered = append(filtered, h)
		}
	}
	return filtered
}

// getCategories returns cached categories, re-fetching when the TTL expires.
func (p *Pipeline) getCategories(ctx context.Context) ([]firefly.Category, error) {
	p.catMu.RLock()
	if !p.catFetched.IsZero() && time.Since(p.catFetched) < categoryTTL {
		cats := p.catCache
		p.catMu.RUnlock()
		return cats, nil
	}
	p.catMu.RUnlock()

	p.catMu.Lock()
	defer p.catMu.Unlock()

	if !p.catFetched.IsZero() && time.Since(p.catFetched) < categoryTTL {
		return p.catCache, nil
	}

	cats, err := p.firefly.GetCategories(ctx)
	if err != nil {
		return nil, err
	}
	p.catCache = cats
	p.catFetched = time.Now()
	return cats, nil
}

// getExpenseAccounts returns cached expense accounts, re-fetching when the TTL expires.
func (p *Pipeline) getExpenseAccounts(ctx context.Context) ([]firefly.Account, error) {
	p.acctMu.RLock()
	if !p.acctFetched.IsZero() && time.Since(p.acctFetched) < categoryTTL {
		accts := p.acctCache
		p.acctMu.RUnlock()
		return accts, nil
	}
	p.acctMu.RUnlock()

	p.acctMu.Lock()
	defer p.acctMu.Unlock()

	if !p.acctFetched.IsZero() && time.Since(p.acctFetched) < categoryTTL {
		return p.acctCache, nil
	}

	accts, err := p.firefly.GetExpenseAccounts(ctx)
	if err != nil {
		return nil, err
	}
	p.acctCache = accts
	p.acctFetched = time.Now()
	return accts, nil
}

// TransferSuggestion is the result of suggesting a destination asset account
// for a transfer conversion.
type TransferSuggestion struct {
	AccountName string
	RawResponse string
}

// SuggestTransfer asks the LLM to pick the best destination asset account for
// a transfer, given the transaction description and a list of available accounts.
func (p *Pipeline) SuggestTransfer(ctx context.Context, description string, accounts []classifier.AccountCandidate) (TransferSuggestion, error) {
	var acctList string
	for _, a := range accounts {
		acctList += "  - " + a.Name + "\n"
	}

	result, err := p.classifier.Classify(ctx, classifier.Request{
		SystemPromptOverride: fmt.Sprintf(
			"You select the best destination asset account for a transfer.\n"+
				"The transaction description is: %q\n\n"+
				"Available asset accounts:\n%s\n"+
				"Pick the most likely destination account.\n"+
				"Respond with ONLY valid JSON: {\"account\": \"<exact account name from the list>\"}",
			description, acctList,
		),
	})
	if err != nil {
		return TransferSuggestion{}, err
	}

	var parsed struct {
		Account string `json:"account"`
	}
	cleaned := strings.TrimSpace(result.RawResponse)
	if strings.HasPrefix(cleaned, "```") {
		end := strings.Index(cleaned[3:], "\n")
		if end >= 0 {
			cleaned = cleaned[3+end+1:]
		}
		if idx := strings.LastIndex(cleaned, "```"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return TransferSuggestion{}, fmt.Errorf("parse transfer suggestion: %w (raw: %s)", err, result.RawResponse)
	}

	return TransferSuggestion{
		AccountName: parsed.Account,
		RawResponse: result.RawResponse,
	}, nil
}
