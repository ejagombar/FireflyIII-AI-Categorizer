package classifier

import (
	"strings"
	"testing"
)

// A payee that carries no merchant information must be recognised as blank.
// "(unknown destination)" is what Firefly III fills in when the bank supplies no
// payee, and it was missing from the original list.
func TestIsBlankName(t *testing.T) {
	blank := []string{
		"",
		"   ",
		"-",
		"n/a",
		"n.a.",
		"no name",
		"(no name)",
		"unknown",
		"Unknown Destination",
		"(unknown destination)",
		"(Unknown destination #12)",
		"Unknown payee",
		"no registrado",
		"POS Purchase",
		"(transfer)",
		"(sin nombre)", // unknown locale: the parentheses convention covers it
	}
	for _, name := range blank {
		if !IsBlankName(name) {
			t.Errorf("IsBlankName(%q) = false, want true", name)
		}
	}

	present := []string{
		"Amazon",
		"Mercadona",
		"Café Central",
		"AZAHAR NOCHE, SL",
		"TransferWise",
		"Delta Airlines",
	}
	for _, name := range present {
		if IsBlankName(name) {
			t.Errorf("IsBlankName(%q) = true, want false", name)
		}
	}
}

// The regression: two different merchants sharing the same placeholder payee must
// not share a history bucket.
func TestGroupKeyPlaceholderDoesNotCollapseMerchants(t *testing.T) {
	const payee = "(unknown destination)"
	azahar := "COMPRA EN AZAHAR NOCHE, SL, CON LA TARJETA : XXXXXXXXXXXX9408 EL 2026-10-12"
	renfe := "COMPRA BIZUM VENTA BILLETES RENFE 2026-08-25 PEDIDO 000457060982"

	gotA := GroupKey(payee, azahar)
	gotB := GroupKey(payee, renfe)

	if gotA == gotB {
		t.Fatalf("GroupKey collapsed two merchants onto %q", gotA)
	}
	if gotA != azahar || gotB != renfe {
		t.Errorf("GroupKey should fall back to the description, got %q and %q", gotA, gotB)
	}
}

// A real payee keeps winning over the description.
func TestGroupKeyKeepsRealPayee(t *testing.T) {
	if got := GroupKey("Mercadona", "COMPRA TARJETA 2026-10-12"); got != "Mercadona" {
		t.Errorf("GroupKey() = %q, want %q", got, "Mercadona")
	}
}

// The prompt must not present a placeholder as if it were the merchant, and it
// must tell the model where to look instead.
func TestBuildUserPromptPlaceholderPayee(t *testing.T) {
	desc := "COMPRA EN AZAHAR NOCHE, SL, CON LA TARJETA : XXXXXXXXXXXX9408 EL 2026-10-12"
	prompt := buildUserPrompt(Request{
		DestinationName: "(unknown destination)",
		Description:     desc,
	})

	if strings.Contains(prompt, "(unknown destination)") {
		t.Errorf("prompt repeats the placeholder payee:\n%s", prompt)
	}
	if !strings.Contains(prompt, "not recorded by the bank") {
		t.Errorf("prompt does not flag the missing payee:\n%s", prompt)
	}
	if strings.Count(prompt, desc) != 1 {
		t.Errorf("prompt should show the description exactly once:\n%s", prompt)
	}
}

// A real payee is still shown as the payee.
func TestBuildUserPromptRealPayee(t *testing.T) {
	prompt := buildUserPrompt(Request{
		DestinationName: "Mercadona",
		Description:     "COMPRA TARJETA 2026-10-12",
	})

	if !strings.Contains(prompt, "Payee: Mercadona") {
		t.Errorf("prompt lost the real payee:\n%s", prompt)
	}
	if strings.Contains(prompt, "not recorded") {
		t.Errorf("prompt claims the payee is missing:\n%s", prompt)
	}
}

// Placeholder accounts are filtered out of the candidate list offered to the model.
func TestBuildUserPromptSkipsPlaceholderAccounts(t *testing.T) {
	prompt := buildUserPrompt(Request{
		DestinationName: "Mercadona",
		Description:     "COMPRA TARJETA 2026-10-12",
		ExpenseAccounts: []AccountCandidate{
			{ID: "1", Name: "(unknown destination)"},
			{ID: "2", Name: "Mercadona"},
		},
	})

	if strings.Contains(prompt, "(unknown destination)") {
		t.Errorf("prompt offers a placeholder account as a candidate:\n%s", prompt)
	}
	if !strings.Contains(prompt, "  - Mercadona") {
		t.Errorf("prompt dropped the real account:\n%s", prompt)
	}
}
