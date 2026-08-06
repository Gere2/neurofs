package audit

import (
	"context"
	"strings"
	"testing"
)

func TestDecomposeClaims(t *testing.T) {
	text := `Here is the fix for internal/orchestrator/types.go:42.
We use the struct ModelEntry and function NewRouter to resolve tasks.`

	claims := DecomposeClaims(text)

	if len(claims) == 0 {
		t.Fatal("expected decomposed claims, got 0")
	}

	hasCitation := false
	hasSymbol := false
	for _, c := range claims {
		if c.Kind == ClaimCitation && strings.Contains(c.RelPath, "types.go") {
			hasCitation = true
		}
		if c.Kind == ClaimSymbol && (c.Content == "ModelEntry" || c.Content == "NewRouter") {
			hasSymbol = true
		}
	}

	if !hasCitation {
		t.Error("expected citation claim for types.go")
	}
	if !hasSymbol {
		t.Error("expected symbol claim for ModelEntry or NewRouter")
	}
}

func TestEvaluateEntailment(t *testing.T) {
	claims := []Claim{
		{Kind: ClaimCitation, Content: "internal/orchestrator/types.go:10", RelPath: "internal/orchestrator/types.go", Line: 10},
		{Kind: ClaimSymbol, Content: "ModelEntry"},
		{Kind: ClaimSymbol, Content: "UnknownFakeSymbol123"},
	}

	contextStr := "File internal/orchestrator/types.go contains ModelEntry struct definition"

	entailments := EvaluateEntailment(claims, contextStr, ".")

	if len(entailments) != 3 {
		t.Fatalf("expected 3 entailment results, got %d", len(entailments))
	}

	if !entailments[0].Entailed {
		t.Error("expected types.go citation to be entailed by context")
	}
	if !entailments[1].Entailed {
		t.Error("expected ModelEntry symbol to be entailed by context")
	}
	if entailments[2].Entailed {
		t.Error("expected UnknownFakeSymbol123 symbol to NOT be entailed")
	}
}

func TestVerifyResponse_FullFlow(t *testing.T) {
	ctx := context.Background()
	response := "File internal/orchestrator/types.go line 12 defines ModelEntry"
	contextStr := "File internal/orchestrator/types.go:12 defines ModelEntry"

	report := VerifyResponse(ctx, response, contextStr, ".", false)

	if report.Score <= 0 {
		t.Errorf("expected positive verification score, got %.2f", report.Score)
	}
	if report.ClaimScore < 0.8 {
		t.Errorf("expected high claim score, got %.2f", report.ClaimScore)
	}
}
