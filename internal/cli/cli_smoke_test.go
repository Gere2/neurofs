package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubcommandsHelp(t *testing.T) {
	subcommands := []string{
		"scan",
		"setup",
		"watch",
		"ask",
		"pack",
		"expand",
		"task",
		"measure",
		"memory",
		"recall",
		"stats",
		"bench",
		"economy",
		"ground",
		"learn",
		"audit",
		"gate",
		"ui",
		"mcp",
		"proxy",
		"version",
	}

	for _, sub := range subcommands {
		t.Run(sub, func(t *testing.T) {
			cmd := New()
			buf := new(bytes.Buffer)
			cmd.SetOut(buf)
			cmd.SetErr(buf)
			cmd.SetArgs([]string{sub, "--help"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("command %q failed to execute help: %v", sub, err)
			}
			if buf.Len() == 0 {
				t.Errorf("command %q help output was empty", sub)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	cmd := New()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version failed: %v", err)
	}
	if strings.TrimSpace(buf.String()) == "" {
		t.Fatal("version command must print a resolved build version")
	}
}

func TestInvalidCommandFlags(t *testing.T) {
	cmd := New()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"stats", "--invalid-flag-xyz"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for invalid flag, got nil")
	}
}

func TestGateSkipFixturesHelpNamesUnaffectedCriteria(t *testing.T) {
	flag := newGateCmd().Flags().Lookup("skip-fixtures")
	if flag == nil {
		t.Fatal("gate --skip-fixtures flag is missing")
	}
	for _, criterion := range []string{"G1", "G2", "G4", "G5"} {
		if !strings.Contains(flag.Usage, criterion) {
			t.Errorf("--skip-fixtures help %q does not mention unaffected %s", flag.Usage, criterion)
		}
	}
}

func TestG5AttestationRejectsNonCanonicalFlagsBeforeRunning(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/g5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "economy search limit",
			args: []string{
				"economy", "--repo", repo, "--g5-attest", "--search-limit", "7",
			},
			want: "--g5-attest requires",
		},
		{
			name: "gate truncated fixtures",
			args: []string{
				"gate", "--repo", repo, "--g5-attest", "--json", "--max-fixtures", "1",
			},
			want: "--g5-attest requires",
		},
		{
			name: "economy retained JSON",
			args: []string{
				"economy", "--repo", repo, "--g5-attest", "--g5-engine-root", repo,
			},
			want: "retained JSON",
		},
		{
			name: "economy engine root",
			args: []string{
				"economy", "--repo", repo, "--g5-attest", "--json",
			},
			want: "--g5-engine-root",
		},
		{
			name: "gate persisted bundles",
			args: []string{
				"gate", "--repo", repo, "--g5-attest", "--json",
				"--g5-engine-root", repo, "--bundles-dir", filepath.Join(repo, "bundles"),
			},
			want: "does not accept --bundles-dir",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := New()
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			cmd.SetArgs(test.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
