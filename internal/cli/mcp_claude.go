package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/spf13/cobra"
)

type claudeServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type mcpClaudeOptions struct {
	RepoRoot   string
	Name       string
	ConfigPath string
	Command    string
	Force      bool
}

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func newMcpInstallCmd() *cobra.Command {
	var opts mcpClaudeOptions
	cmd := &cobra.Command{
		Use:   "install claude",
		Short: "Install NeuroFS MCP into Claude Desktop",
		Args: func(cmd *cobra.Command, args []string) error {
			return requireClaudeClient(args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			path, name, err := installClaudeMCP(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed NeuroFS MCP server %q in %s\n", name, path)
			fmt.Fprintf(cmd.OutOrStdout(), "Restart Claude Desktop completely before using it.\n")
			return nil
		},
	}
	addMcpClaudeFlags(cmd, &opts, true)
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Overwrite an existing Claude MCP server with the same name")
	return cmd
}

func newMcpUninstallCmd() *cobra.Command {
	var opts mcpClaudeOptions
	cmd := &cobra.Command{
		Use:   "uninstall claude",
		Short: "Remove a NeuroFS MCP entry from Claude Desktop",
		Args: func(cmd *cobra.Command, args []string) error {
			return requireClaudeClient(args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			path, name, err := uninstallClaudeMCP(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed NeuroFS MCP server %q from %s\n", name, path)
			return nil
		},
	}
	addMcpClaudeFlags(cmd, &opts, false)
	return cmd
}

func newMcpDoctorCmd() *cobra.Command {
	var opts mcpClaudeOptions
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor claude",
		Short: "Check Claude Desktop MCP configuration for NeuroFS",
		Args: func(cmd *cobra.Command, args []string) error {
			return requireClaudeClient(args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			checks, err := doctorClaudeMCP(opts)
			if jsonOut {
				if encErr := json.NewEncoder(cmd.OutOrStdout()).Encode(checks); encErr != nil {
					return encErr
				}
				return err
			}
			for _, check := range checks {
				fmt.Fprintf(cmd.OutOrStdout(), "%-5s %s — %s\n", strings.ToUpper(check.Status), check.Name, check.Detail)
			}
			return err
		},
	}
	addMcpClaudeFlags(cmd, &opts, false)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable doctor results")
	return cmd
}

func addMcpClaudeFlags(cmd *cobra.Command, opts *mcpClaudeOptions, includeCommand bool) {
	cmd.Flags().StringVar(&opts.RepoRoot, "repo", "", "Repository root to expose (defaults to current directory)")
	cmd.Flags().StringVar(&opts.Name, "name", "", "Claude MCP server name (defaults to neurofs-<repo>)")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", "", "Path to claude_desktop_config.json")
	if includeCommand {
		cmd.Flags().StringVar(&opts.Command, "command", "", "Path to neurofs executable (defaults to current executable)")
	}
}

func requireClaudeClient(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected client name: claude")
	}
	if strings.EqualFold(args[0], "claude") {
		return nil
	}
	return fmt.Errorf("unsupported MCP client %q (only claude is supported)", args[0])
}

func installClaudeMCP(opts mcpClaudeOptions) (string, string, error) {
	repo, err := resolveInstallRepo(opts.RepoRoot)
	if err != nil {
		return "", "", err
	}
	name := opts.Name
	if name == "" {
		name = defaultMCPServerName(repo)
	}
	command, err := resolveMCPCommand(opts.Command)
	if err != nil {
		return "", "", err
	}
	configPath, err := claudeConfigPath(opts.ConfigPath)
	if err != nil {
		return "", "", err
	}
	root, servers, err := readClaudeConfig(configPath)
	if err != nil {
		return "", "", err
	}
	if _, exists := servers[name]; exists && !opts.Force {
		return "", "", fmt.Errorf("Claude MCP server %q already exists in %s (use --force to overwrite)", name, configPath)
	}
	server := claudeServerConfig{
		Command: command,
		Args:    []string{"mcp", "--repo", repo},
	}
	if err := setClaudeServer(root, servers, name, server); err != nil {
		return "", "", err
	}
	if err := writeClaudeConfig(configPath, root); err != nil {
		return "", "", err
	}
	return configPath, name, nil
}

func uninstallClaudeMCP(opts mcpClaudeOptions) (string, string, error) {
	repo, _ := resolveInstallRepo(opts.RepoRoot)
	name := opts.Name
	if name == "" {
		name = defaultMCPServerName(repo)
	}
	configPath, err := claudeConfigPath(opts.ConfigPath)
	if err != nil {
		return "", "", err
	}
	root, servers, err := readClaudeConfig(configPath)
	if err != nil {
		return "", "", err
	}
	if _, exists := servers[name]; !exists {
		return "", "", fmt.Errorf("Claude MCP server %q not found in %s", name, configPath)
	}
	delete(servers, name)
	if err := setClaudeServers(root, servers); err != nil {
		return "", "", err
	}
	if err := writeClaudeConfig(configPath, root); err != nil {
		return "", "", err
	}
	return configPath, name, nil
}

func doctorClaudeMCP(opts mcpClaudeOptions) ([]doctorCheck, error) {
	var checks []doctorCheck
	add := func(name, status, detail string) {
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: detail})
	}
	failures := 0
	fail := func(name, detail string) {
		failures++
		add(name, "fail", detail)
	}

	repo, repoErr := resolveInstallRepo(opts.RepoRoot)
	if repoErr != nil {
		fail("repo", repoErr.Error())
	} else {
		add("repo", "pass", repo)
	}
	name := opts.Name
	if name == "" {
		name = defaultMCPServerName(repo)
	}
	configPath, err := claudeConfigPath(opts.ConfigPath)
	if err != nil {
		fail("config path", err.Error())
		return checks, doctorError(failures)
	}
	add("config path", "pass", configPath)

	root, servers, err := readClaudeConfig(configPath)
	if err != nil {
		fail("config json", err.Error())
		return checks, doctorError(failures)
	}
	_ = root
	add("config json", "pass", "valid JSON")

	raw, exists := servers[name]
	if !exists {
		fail("server entry", fmt.Sprintf("%q not found", name))
		return checks, doctorError(failures)
	}
	var server claudeServerConfig
	if err := json.Unmarshal(raw, &server); err != nil {
		fail("server entry", fmt.Sprintf("decode %q: %v", name, err))
		return checks, doctorError(failures)
	}
	add("server entry", "pass", name)

	if filepath.IsAbs(server.Command) {
		if st, err := os.Stat(server.Command); err == nil && !st.IsDir() {
			add("command", "pass", server.Command)
		} else {
			fail("command", fmt.Sprintf("%s is not executable or does not exist", server.Command))
		}
	} else {
		fail("command", fmt.Sprintf("%q is not an absolute path", server.Command))
	}

	if repoArg := repoFromMCPArgs(server.Args); repoArg != "" {
		if repoErr == nil && !sameCleanPath(repoArg, repo) {
			fail("repo arg", fmt.Sprintf("configured %s, expected %s", repoArg, repo))
		} else {
			add("repo arg", "pass", repoArg)
		}
	} else {
		add("repo arg", "warn", "server args do not include --repo; cwd-based MCP is fragile")
	}

	if failures == 0 {
		if err := smokeClaudeMCPServer(server); err != nil {
			fail("protocol", err.Error())
		} else {
			add("protocol", "pass", "initialize and tools/list succeeded")
		}
	}
	return checks, doctorError(failures)
}

func doctorError(failures int) error {
	if failures > 0 {
		return fmt.Errorf("doctor found %d failure(s)", failures)
	}
	return nil
}

func resolveInstallRepo(repoRoot string) (string, error) {
	if repoRoot == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
		repoRoot = cwd
	}
	cfg, err := config.New(repoRoot)
	if err != nil {
		return "", fmt.Errorf("repo config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return "", fmt.Errorf("repo config: %w", err)
	}
	abs, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repo: %w", err)
	}
	return abs, nil
}

func resolveMCPCommand(command string) (string, error) {
	if command == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolve executable: %w", err)
		}
		command = exe
	}
	abs, err := filepath.Abs(command)
	if err != nil {
		return "", fmt.Errorf("resolve command: %w", err)
	}
	return abs, nil
}

func claudeConfigPath(path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json"), nil
	default:
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

func readClaudeConfig(path string) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	root := make(map[string]json.RawMessage)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return root, make(map[string]json.RawMessage), nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return root, make(map[string]json.RawMessage), nil
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	servers := make(map[string]json.RawMessage)
	if raw, ok := root["mcpServers"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, nil, fmt.Errorf("parse mcpServers in %s: %w", path, err)
		}
	}
	return root, servers, nil
}

func setClaudeServer(root map[string]json.RawMessage, servers map[string]json.RawMessage, name string, server claudeServerConfig) error {
	raw, err := json.Marshal(server)
	if err != nil {
		return err
	}
	servers[name] = raw
	return setClaudeServers(root, servers)
}

func setClaudeServers(root map[string]json.RawMessage, servers map[string]json.RawMessage) error {
	raw, err := json.Marshal(servers)
	if err != nil {
		return err
	}
	root["mcpServers"] = raw
	return nil
}

func writeClaudeConfig(path string, root map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func defaultMCPServerName(repoRoot string) string {
	base := filepath.Base(filepath.Clean(repoRoot))
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "repo"
	}
	return "neurofs-" + slugMCPName(base)
}

func slugMCPName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && b.Len() > 0 {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "repo"
	}
	return out
}

func repoFromMCPArgs(args []string) string {
	for i, arg := range args {
		if arg == "--repo" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--repo=") {
			return strings.TrimPrefix(arg, "--repo=")
		}
	}
	return ""
}

func sameCleanPath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA == nil {
		a = aa
	}
	if errB == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func smokeClaudeMCPServer(server claudeServerConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, server.Command, server.Args...)
	cmd.Stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("server timed out")
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		return fmt.Errorf("expected initialize and tools/list responses, got %d line(s)", len(lines))
	}
	var sawTask, sawExpand, sawMeasure bool
	for _, line := range lines {
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return fmt.Errorf("invalid JSON-RPC response: %w", err)
		}
		if resp.Error != nil {
			return fmt.Errorf("server returned JSON-RPC error: %v", resp.Error)
		}
		for _, tool := range resp.Result.Tools {
			switch tool.Name {
			case "neurofs_task":
				sawTask = true
			case "neurofs_expand":
				sawExpand = true
			case "neurofs_measure":
				sawMeasure = true
			}
		}
	}
	if !sawTask || !sawExpand || !sawMeasure {
		return fmt.Errorf("tools/list missing expected NeuroFS agent tools")
	}
	return nil
}
