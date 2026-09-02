package kernel

import "fmt"

// EpistemicStatus is how strongly NeuroFS may treat a claim. The three values
// are not a confidence score: they are different kinds of statement.
type EpistemicStatus string

const (
	// StatusObservation is something NeuroFS or an adapter saw in a source
	// (a file range, a document snapshot, a log line). It is not a conclusion.
	StatusObservation EpistemicStatus = "observation"
	// StatusInference is a model or heuristic conclusion that is not itself
	// present as evidence. It may be useful, but it is not a fact.
	StatusInference EpistemicStatus = "inference"
	// StatusConfirmedFact is an assertion that has been checked against
	// evidence (human amendment, verifier receipt, or an explicit confirmation
	// record). It still points at evidence; NeuroFS does not become the SoR.
	StatusConfirmedFact EpistemicStatus = "confirmed_fact"
)

// Validate reports whether s is one of the three statuses.
func (s EpistemicStatus) Validate() error {
	switch s {
	case StatusObservation, StatusInference, StatusConfirmedFact:
		return nil
	case "":
		return fmt.Errorf("kernel: epistemic status is required")
	default:
		return fmt.Errorf("kernel: unknown epistemic status %q", s)
	}
}
