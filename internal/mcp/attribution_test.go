package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/runid"
	"github.com/Gere2/neurofs/internal/usage"
)

// TestServerNeverAttachesStaleAmbientRunID is the end-to-end proof of the
// correlation-coverage contract: a long-lived MCP server inherits whatever
// NEUROFS_RUN_ID its launcher happened to export, but that value describes the
// launch, not the request being served. Attaching it would silently attribute
// one run's evidence to another, so the server must record the gap instead.
//
// The test drives the real server over its real transport and inspects the
// artifact that actually lands on disk.
func TestServerNeverAttachesStaleAmbientRunID(t *testing.T) {
	// A fixed id, not a generated one: runid.FromEnv is read once per process,
	// so under -count>1 a fresh id per iteration would be planted in the
	// environment but never observed, and the control below would fail. The
	// value only has to be live and identifiable.
	const staleID = runid.RunID("run-stale-launch-environment-id")
	t.Setenv(runid.EnvVar, staleID.String())

	// Control: the ambient id must actually be live in this process, otherwise
	// an "unavailable" result below would prove nothing — it would just mean
	// there was no id to attach. runid.FromEnv is read once per process, so
	// this also catches an earlier test having cached the lookup.
	ambient, err := runid.Current(context.Background())
	if err != nil {
		t.Fatalf("control: ambient lookup failed: %v", err)
	}
	if ambient.RunID != staleID {
		t.Fatalf("control: ambient id is %q, want %q — the environment was already "+
			"read before this test, so the suppression assertion below would be vacuous",
			ambient.RunID, staleID)
	}

	repo := t.TempDir()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(inR, outW, io.Discard, "test")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		runErr := srv.Run(ctx)
		closePipeWriter(t, outW)
		done <- runErr
	}()

	call, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "neurofs_feedback",
			"arguments": map[string]any{
				"repo":   repo,
				"query":  "how does auth work",
				"rating": "yes",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		defer closePipeWriter(t, inW)
		msgs := []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			string(call),
		}
		for _, m := range msgs {
			if _, werr := inW.Write([]byte(m + "\n")); werr != nil {
				return
			}
		}
	}()

	dec := json.NewDecoder(outR)
	for i := 0; i < 2; i++ {
		var resp Response
		if derr := dec.Decode(&resp); derr != nil {
			t.Fatalf("decode response %d: %v", i+1, derr)
		}
		if resp.Error != nil {
			t.Fatalf("response %d error: %+v", i+1, resp.Error)
		}
	}
	cancel()
	<-done

	raw, err := os.ReadFile(usage.FeedbackPath(repo))
	if err != nil {
		t.Fatalf("the tool call wrote no feedback artifact: %v", err)
	}
	line := strings.TrimSpace(strings.Split(strings.TrimSpace(string(raw)), "\n")[0])

	var fb usage.Feedback
	if err := json.Unmarshal([]byte(line), &fb); err != nil {
		t.Fatalf("unmarshal feedback: %v", err)
	}

	if fb.RunID == staleID {
		t.Fatalf("server attached the stale launch-environment id %q to a served request", staleID)
	}
	if !fb.RunID.IsZero() {
		t.Fatalf("server attached run id %q despite unavailable correlation", fb.RunID)
	}
	if fb.Correlation != runid.CorrelationUnavailable {
		t.Fatalf("got correlation %q, want %q", fb.Correlation, runid.CorrelationUnavailable)
	}
	if fb.Reason == "" {
		t.Fatal("unavailable correlation persisted without a reason")
	}
	if err := fb.Availability.Validate(); err != nil {
		t.Fatalf("persisted attribution is invalid: %v", err)
	}

	// The raw JSON must carry the gap explicitly: a consumer reading this line
	// has to tell "not correlated" from "field forgotten".
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["run_id"]; ok {
		t.Error("uncorrelated artifact persisted a run_id field")
	}
	if fields["run_correlation"] != string(runid.CorrelationUnavailable) {
		t.Errorf("run_correlation on disk: got %v", fields["run_correlation"])
	}
}
