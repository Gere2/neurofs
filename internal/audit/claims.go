package audit

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// ClaimKind identifies the type of atomic claim extracted from model output.
type ClaimKind string

const (
	ClaimCitation ClaimKind = "citation" // file path or file:line citation
	ClaimSymbol   ClaimKind = "symbol"   // code function/struct/package name
	ClaimCodeBlock ClaimKind = "code"    // executable code block
)

// Claim represents one verifiable statement in an LLM response.
type Claim struct {
	Kind      ClaimKind `json:"kind"`
	Content   string    `json:"content"`
	RelPath   string    `json:"rel_path,omitempty"`
	Line      int       `json:"line,omitempty"`
	Verifiable bool      `json:"verifiable"`
}

// ClaimEntailment tracks the verification status of one claim.
type ClaimEntailment struct {
	Claim     Claim   `json:"claim"`
	Entailed  bool    `json:"entailed"`
	Score     float64 `json:"score"` // 0.0 to 1.0
	Reason    string  `json:"reason"`
}

// ExecutionResult tracks sandbox build/test verification outcomes.
type ExecutionResult struct {
	Ran         bool   `json:"ran"`
	BuildPassed bool   `json:"build_passed"`
	TestPassed  bool   `json:"test_passed"`
	BuildOutput string `json:"build_output,omitempty"`
	TestOutput  string `json:"test_output,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
}

// VerificationReport is the composite receipt-based verification report.
type VerificationReport struct {
	Score              float64           `json:"score"`               // composite 0.0 - 1.0
	CitationScore      float64           `json:"citation_score"`      // citation ratio
	ClaimScore         float64           `json:"claim_score"`         // claim entailment ratio
	ExecutionScore     float64           `json:"execution_score"`     // sandbox score
	Claims             []ClaimEntailment `json:"claims,omitempty"`
	Execution          ExecutionResult   `json:"execution"`
	VerifiedAt         time.Time         `json:"verified_at"`
}

var symbolPattern = regexp.MustCompile(`\b([A-Z][A-Za-z0-9_]+)\b|\b(func\s+[A-Za-z0-9_]+)\b`)

// DecomposeClaims extracts verifiable claims from a model response.
func DecomposeClaims(text string) []Claim {
	var claims []Claim

	// 1. Extract file citations
	citations := ParseCitations(text)
	for _, c := range citations {
		claims = append(claims, Claim{
			Kind:       ClaimCitation,
			Content:    c.Raw,
			RelPath:    c.RelPath,
			Line:       c.Line,
			Verifiable: true,
		})
	}

	// 2. Extract code symbols (structs / functions)
	symbolMatches := symbolPattern.FindAllString(text, -1)
	seenSymbols := make(map[string]bool)
	for _, sym := range symbolMatches {
		sym = strings.TrimSpace(sym)
		if len(sym) < 4 || seenSymbols[sym] {
			continue
		}
		seenSymbols[sym] = true
		claims = append(claims, Claim{
			Kind:       ClaimSymbol,
			Content:    sym,
			Verifiable: true,
		})
	}

	return claims
}

// EvaluateEntailment checks if decomposed claims are entailed by context or repo.
func EvaluateEntailment(claims []Claim, contextStr, repoRoot string) []ClaimEntailment {
	ctxLower := strings.ToLower(contextStr)
	results := make([]ClaimEntailment, 0, len(claims))

	for _, c := range claims {
		switch c.Kind {
		case ClaimCitation:
			entailed := false
			reason := "citation not found in context"

			if strings.Contains(ctxLower, strings.ToLower(c.RelPath)) {
				entailed = true
				reason = "cited file path found in retrieval context"
			}

			score := 0.0
			if entailed {
				score = 1.0
			}
			results = append(results, ClaimEntailment{
				Claim:    c,
				Entailed: entailed,
				Score:    score,
				Reason:   reason,
			})

		case ClaimSymbol:
			entailed := strings.Contains(contextStr, c.Content)
			reason := "symbol found in context"
			score := 1.0
			if !entailed {
				reason = "symbol missing from retrieval context"
				score = 0.0
			}
			results = append(results, ClaimEntailment{
				Claim:    c,
				Entailed: entailed,
				Score:    score,
				Reason:   reason,
			})
		}
	}

	return results
}

// RunExecutionSandbox performs go build / go test sandbox checks if in a Go repo.
func RunExecutionSandbox(ctx context.Context, repoRoot string) ExecutionResult {
	if repoRoot == "" {
		repoRoot = "."
	}

	start := time.Now()
	res := ExecutionResult{
		Ran: true,
	}

	// 1. Test go build
	buildCmd := exec.CommandContext(ctx, "go", "build", "./...")
	buildCmd.Dir = repoRoot
	buildOut, buildErr := buildCmd.CombinedOutput()
	res.BuildOutput = truncateStr(string(buildOut), 500)
	res.BuildPassed = (buildErr == nil)

	// 2. Test go test
	if res.BuildPassed {
		testCmd := exec.CommandContext(ctx, "go", "test", "-short", "-timeout", "5s", "./...")
		testCmd.Dir = repoRoot
		testOut, testErr := testCmd.CombinedOutput()
		res.TestOutput = truncateStr(string(testOut), 500)
		res.TestPassed = (testErr == nil)
	}

	res.DurationMs = time.Since(start).Milliseconds()
	return res
}

// VerifyResponse creates a full receipt-based VerificationReport.
func VerifyResponse(ctx context.Context, response, contextStr, repoRoot string, runSandbox bool) VerificationReport {
	report := VerificationReport{
		VerifiedAt: time.Now(),
	}

	if response == "" {
		return report
	}

	// Decompose & evaluate claims
	claims := DecomposeClaims(response)
	entailments := EvaluateEntailment(claims, contextStr, repoRoot)
	report.Claims = entailments

	// Calculate claim score
	if len(entailments) > 0 {
		var sum float64
		var citationsSum float64
		var citationCount int

		for _, e := range entailments {
			sum += e.Score
			if e.Claim.Kind == ClaimCitation {
				citationsSum += e.Score
				citationCount++
			}
		}

		report.ClaimScore = sum / float64(len(entailments))
		if citationCount > 0 {
			report.CitationScore = citationsSum / float64(citationCount)
		} else {
			report.CitationScore = report.ClaimScore
		}
	} else {
		// Fallback token overlap estimation if no explicit citations/symbols
		report.CitationScore = fallbackTokenOverlap(response, contextStr)
		report.ClaimScore = report.CitationScore
	}

	// Execution sandbox
	if runSandbox && repoRoot != "" {
		sandboxCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		report.Execution = RunExecutionSandbox(sandboxCtx, repoRoot)

		if report.Execution.BuildPassed && report.Execution.TestPassed {
			report.ExecutionScore = 1.0
		} else if report.Execution.BuildPassed {
			report.ExecutionScore = 0.6
		} else {
			report.ExecutionScore = 0.0
		}

		// Composite score: 40% claim, 30% citation, 30% execution
		report.Score = 0.4*report.ClaimScore + 0.3*report.CitationScore + 0.3*report.ExecutionScore
	} else {
		// Without sandbox: 60% claim, 40% citation
		report.Score = 0.6*report.ClaimScore + 0.4*report.CitationScore
	}

	if report.Score > 1.0 {
		report.Score = 1.0
	}
	return report
}

func fallbackTokenOverlap(response, contextStr string) float64 {
	respWords := strings.Fields(strings.ToLower(response))
	if len(respWords) == 0 || contextStr == "" {
		return 0.0
	}
	matched := 0
	ctxLower := strings.ToLower(contextStr)
	for _, w := range respWords {
		if len(w) > 3 && strings.Contains(ctxLower, w) {
			matched++
		}
	}
	score := float64(matched) / float64(len(respWords))
	if score > 1.0 {
		score = 1.0
	}
	return score
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
