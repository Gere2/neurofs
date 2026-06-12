package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallAndUninstallClaudeMCP(t *testing.T) {
	repo := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	initial := `{
  "theme": "dark",
  "mcpServers": {
    "other": {"command": "/bin/echo", "args": ["ok"]}
  }
}`
	if err := os.WriteFile(configPath, []byte(initial), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	command := filepath.Join(repo, "bin", "neurofs")

	path, name, err := installClaudeMCP(mcpClaudeOptions{
		RepoRoot:   repo,
		ConfigPath: configPath,
		Command:    command,
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if path != configPath || name != "neurofs-"+slugMCPName(filepath.Base(repo)) {
		t.Fatalf("unexpected install result path=%s name=%s", path, name)
	}

	root, servers, err := readClaudeConfig(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if _, ok := root["theme"]; !ok {
		t.Fatalf("install should preserve unrelated root keys: %+v", root)
	}
	if _, ok := servers["other"]; !ok {
		t.Fatalf("install should preserve other MCP servers: %+v", servers)
	}
	var installed claudeServerConfig
	if err := json.Unmarshal(servers[name], &installed); err != nil {
		t.Fatalf("decode installed server: %v", err)
	}
	if installed.Command != command {
		t.Fatalf("command = %s, want %s", installed.Command, command)
	}
	if len(installed.Args) != 3 || installed.Args[0] != "mcp" || installed.Args[1] != "--repo" || installed.Args[2] != repo {
		t.Fatalf("unexpected args: %+v", installed.Args)
	}

	if _, _, err := installClaudeMCP(mcpClaudeOptions{
		RepoRoot:   repo,
		ConfigPath: configPath,
		Command:    command,
	}); err == nil {
		t.Fatalf("expected duplicate install to require --force")
	}

	if _, _, err := uninstallClaudeMCP(mcpClaudeOptions{
		RepoRoot:   repo,
		ConfigPath: configPath,
	}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	_, servers, err = readClaudeConfig(configPath)
	if err != nil {
		t.Fatalf("read after uninstall: %v", err)
	}
	if _, ok := servers[name]; ok {
		t.Fatalf("server %s should have been removed: %+v", name, servers)
	}
	if _, ok := servers["other"]; !ok {
		t.Fatalf("uninstall should preserve other MCP servers: %+v", servers)
	}
}

func TestRepoFromMCPArgs(t *testing.T) {
	if got := repoFromMCPArgs([]string{"mcp", "--repo", "/repo"}); got != "/repo" {
		t.Fatalf("repo arg = %q", got)
	}
	if got := repoFromMCPArgs([]string{"mcp", "--repo=/repo2"}); got != "/repo2" {
		t.Fatalf("repo equals arg = %q", got)
	}
}
