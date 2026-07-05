package cli

import (
	"bytes"
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
