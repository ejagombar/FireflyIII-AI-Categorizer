package classifier

import (
	"fmt"
	"strings"
)

const SystemPrompt = `You are a bank transaction classifier for a personal finance system.

You classify transactions using a three-outcome model:

1. CLASSIFIED — You are confident in the category. The transaction description, destination, and amount give you enough signal to match one of the user's existing categories.

2. ASSUMED — You are not fully confident but you are applying a conservative default. You pick the most reasonable category and disclose the assumption. Conservative means: when in doubt between a deductible and non-deductible category, pick non-deductible. When in doubt between business and personal, pick personal. The user can always override.

3. NEEDS_REVIEW — You cannot classify this transaction. The description is too vague, the destination is unknown, or the amount is ambiguous. You do not guess. You flag it for human review.

You MUST respond with valid JSON matching this schema:
{
  "outcome": "CLASSIFIED" | "ASSUMED" | "NEEDS_REVIEW",
  "category": {
    "name": "<exact category name from the list>",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "reason": "<one sentence explaining your classification>",
  "assumption": "<if ASSUMED: what you assumed and what the alternative is; otherwise null>"
}

Rules:
- category MUST be an exact match from the provided list, or null
- When outcome is CLASSIFIED, category.confidence must be CLASSIFIED
- When outcome is ASSUMED, category.confidence must be ASSUMED
- When outcome is NEEDS_REVIEW, category must be null
- Never invent categories
- When uncertain between two categories, pick the one that is less favorable to the user (conservative default)
- If the description is too vague to even assume, use NEEDS_REVIEW
- The payee may be missing or a placeholder. When it is, identify the merchant from the description before deciding
- Respond ONLY with the JSON object, no markdown, no code fences`

const SystemPromptWithDestination = `You are a bank transaction classifier for a personal finance system.

You classify transactions using a three-outcome model:

1. CLASSIFIED — You are confident in the category. The transaction description, destination, and amount give you enough signal to match one of the user's existing categories.

2. ASSUMED — You are not fully confident but you are applying a conservative default. You pick the most reasonable category and disclose the assumption. Conservative means: when in doubt between a deductible and non-deductible category, pick non-deductible. When in doubt between business and personal, pick personal. The user can always override.

3. NEEDS_REVIEW — You cannot classify this transaction. The description is too vague, the destination is unknown, or the amount is ambiguous. You do not guess. You flag it for human review.

In addition to category classification, you also assign a destination (expense) account.

You MUST respond with valid JSON matching this schema:
{
  "outcome": "CLASSIFIED" | "ASSUMED" | "NEEDS_REVIEW",
  "category": {
    "name": "<exact category name from the list>",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "destination": {
    "name": "<exact account name>",
    "action": "MATCH" | "CREATE",
    "confidence": "CLASSIFIED" | "ASSUMED"
  } | null,
  "reason": "<one sentence explaining your classification>",
  "assumption": "<if ASSUMED: what you assumed and what the alternative is; otherwise null>"
}

Category rules:
- category MUST be an exact match from the provided list, or null
- When outcome is CLASSIFIED, category.confidence must be CLASSIFIED
- When outcome is ASSUMED, category.confidence must be ASSUMED
- When outcome is NEEDS_REVIEW, category must be null

Destination account rules:
- You MUST always attempt to assign a destination account, regardless of the outcome field.
- Category confidence and destination confidence are completely independent.
- When an existing expense account clearly matches the payee, use MATCH with the exact account name from the list.
- When you are confident the payee represents a new expense account not in the list, use CREATE with a reasonable, concise account name (e.g. "Amazon", "Netflix", "Delta Airlines"). Prefer the canonical business name.
- Only set destination to null when the destination_name is truly ambiguous (e.g. generic names like "POS Purchase", "Transfer", or empty).
- If the payee is missing or a placeholder, identify the merchant from the description and use it for both the category and the destination.

Rules:
- Never invent categories
- When uncertain between two categories, pick the one that is less favorable to the user (conservative default)
- If the description is too vague to even assume, use NEEDS_REVIEW
- Respond ONLY with the JSON object, no markdown, no code fences`

// formatCategories renders the category list for the prompt.
// When any category carries notes, a bulleted list is used so the LLM can
// read each description. Without notes it falls back to a compact comma list.
func formatCategories(cats []Category) string {
	hasNotes := false
	for _, c := range cats {
		if c.Notes != "" {
			hasNotes = true
			break
		}
	}

	if !hasNotes {
		names := make([]string, len(cats))
		for i, c := range cats {
			names[i] = c.Name
		}
		return " " + strings.Join(names, ", ")
	}

	var sb strings.Builder
	for _, c := range cats {
		if c.Notes != "" {
			fmt.Fprintf(&sb, "\n  - %s — %s", c.Name, c.Notes)
		} else {
			fmt.Fprintf(&sb, "\n  - %s", c.Name)
		}
	}
	return sb.String()
}

// BuildSystemPrompt returns the system prompt, appending any user-supplied
// custom context after the fixed rules. When destinationMatching is true, the
// prompt includes destination account assignment instructions.
func BuildSystemPrompt(customContext string, destinationMatching bool) string {
	base := SystemPrompt
	if destinationMatching {
		base = SystemPromptWithDestination
	}
	if strings.TrimSpace(customContext) == "" {
		return base
	}
	return base + "\n\nAdditional context provided by the account holder:\n" + strings.TrimSpace(customContext)
}

func buildUserPrompt(req Request) string {
	var sb strings.Builder

	if len(req.History) > 0 {
		gk := GroupKey(req.DestinationName, req.Description)
		fmt.Fprintf(&sb, "Past classifications for %q:\n", gk)
		for _, h := range req.History {
			fmt.Fprintf(&sb, "  - %q → %s\n", h.Description, h.CategoryName)
		}
		sb.WriteString("\n")
	}

	if len(req.ExpenseAccounts) > 0 {
		sb.WriteString("Existing expense accounts:\n")
		for _, a := range req.ExpenseAccounts {
			// Skip placeholder accounts like "(no name)".
			if IsBlankName(a.Name) {
				continue
			}
			fmt.Fprintf(&sb, "  - %s\n", a.Name)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "Available categories:%s\n\n", formatCategories(req.Categories))
	sb.WriteString("Transaction to classify:\n")
	// A blank placeholder is not a merchant: labelling the raw bank line as the
	// destination would both duplicate the description and invite the model to
	// echo it back as an account name.
	if IsBlankName(req.DestinationName) {
		sb.WriteString("  Payee: not recorded by the bank — identify the merchant from the description\n")
	} else {
		fmt.Fprintf(&sb, "  Payee: %s\n", req.DestinationName)
	}
	fmt.Fprintf(&sb, "  Description: %s\n", req.Description)
	if req.Amount != nil {
		fmt.Fprintf(&sb, "  Amount: %.2f\n", *req.Amount)
	}

	return sb.String()
}
