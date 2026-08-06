package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeProjectFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanNodeAndTypeScriptMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "package.json", `{
		"name": "web-app",
		"version": "1.2.3",
		"main": "./dist/index.js",
		"module": "src/index.ts",
		"types": "dist/index.d.ts",
		"bin": {"zeta": "bin/zeta.js", "alpha": "bin/alpha.js"},
		"scripts": {"test": "vitest"},
		"dependencies": {"zod": "3", "react": "19"},
		"devDependencies": {"typescript": "5"}
	}`)
	writeProjectFile(t, repo, "tsconfig.json", `{
		// TypeScript permits comments here.
		"compilerOptions": {
			"baseUrl": ".",
			"paths": {"@app/*": ["src/*"]}
		}
	}`)

	got := Scan(repo)
	if got.Name != "web-app" || got.Version != "1.2.3" {
		t.Fatalf("identity = %q %q", got.Name, got.Version)
	}
	if !reflect.DeepEqual(got.Dependencies, []string{"react", "zod"}) {
		t.Fatalf("dependencies = %v", got.Dependencies)
	}
	if !reflect.DeepEqual(got.BinEntries, []string{"bin/alpha.js", "bin/zeta.js"}) {
		t.Fatalf("bin entries = %v", got.BinEntries)
	}
	if got.BaseURL != "." || got.PathAliases["@app"] != "src" {
		t.Fatalf("TypeScript metadata = baseURL %q aliases %v", got.BaseURL, got.PathAliases)
	}
	if !reflect.DeepEqual(got.Sources, []string{"package.json", "tsconfig.json"}) {
		t.Fatalf("sources = %v", got.Sources)
	}
}

func TestScanGoModMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "go.mod", `module github.com/acme/service

go 1.25

require (
	github.com/jackc/pgx/v5 v5.7.0
	golang.org/x/sync v0.15.0 // indirect
)

require github.com/google/uuid v1.6.0
`)

	got := Scan(repo)
	if got.Name != "service" || got.GoModule != "github.com/acme/service" || got.GoVersion != "1.25" {
		t.Fatalf("Go metadata = %+v", got)
	}
	wantDeps := []string{"github.com/google/uuid", "github.com/jackc/pgx/v5", "golang.org/x/sync"}
	if !reflect.DeepEqual(got.Dependencies, wantDeps) {
		t.Fatalf("dependencies = %v, want %v", got.Dependencies, wantDeps)
	}
	if !reflect.DeepEqual(got.Sources, []string{"go.mod"}) {
		t.Fatalf("sources = %v", got.Sources)
	}
}

func TestScanPyProjectMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "pyproject.toml", `[project]
name = "acme-tool"
version = "2.4.0"
requires-python = ">=3.11"
description = """
An intentionally multiline description.
"""
dependencies = [
  "requests>=2.32",
  "uvicorn[standard] >= 0.30",
]

[project.optional-dependencies]
test = ["pytest>=8", "mypy~=1.10"]

[project.scripts]
acme = "acme.cli:main"
`)
	writeProjectFile(t, repo, "src/acme/cli.py", "def main():\n    pass\n")

	got := Scan(repo)
	if got.Name != "acme-tool" || got.Version != "2.4.0" || got.PythonRequires != ">=3.11" {
		t.Fatalf("Python metadata = %+v", got)
	}
	if !reflect.DeepEqual(got.Dependencies, []string{"requests", "uvicorn"}) {
		t.Fatalf("dependencies = %v", got.Dependencies)
	}
	if !reflect.DeepEqual(got.OptionalDependencies, []string{"mypy", "pytest"}) {
		t.Fatalf("optional dependencies = %v", got.OptionalDependencies)
	}
	if got.PythonScripts["acme"] != "acme.cli:main" {
		t.Fatalf("scripts = %v", got.PythonScripts)
	}
	if !reflect.DeepEqual(got.BinEntries, []string{"src/acme/cli.py"}) {
		t.Fatalf("resolved script entries = %v", got.BinEntries)
	}
}

func TestScanPoetryMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "pyproject.toml", `[tool.poetry]
name = "poetry-tool"
version = "1.5.0"

[tool.poetry.dependencies]
python = "^3.11"
fastapi = "^0.115"

[tool.poetry.group.dev.dependencies]
pytest = "^8"
ruff = "^0.9"

[tool.poetry.scripts]
poetry-tool = "poetry_tool.cli:main"
`)
	writeProjectFile(t, repo, "poetry_tool/cli.py", "def main():\n    pass\n")

	got := Scan(repo)
	if got.Name != "poetry-tool" || got.Version != "1.5.0" || got.PythonRequires != "^3.11" {
		t.Fatalf("Poetry metadata = %+v", got)
	}
	if !reflect.DeepEqual(got.Dependencies, []string{"fastapi"}) {
		t.Fatalf("dependencies = %v", got.Dependencies)
	}
	if !reflect.DeepEqual(got.DevDependencies, []string{"pytest", "ruff"}) {
		t.Fatalf("dev dependencies = %v", got.DevDependencies)
	}
	if !reflect.DeepEqual(got.BinEntries, []string{"poetry_tool/cli.py"}) {
		t.Fatalf("script entries = %v", got.BinEntries)
	}
}

func TestScanCargoMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "Cargo.toml", `[package]
name = "rusty-service"
version = "0.8.1"
edition = "2024"
rust-version = "1.85"

[dependencies]
serde = { version = "1", features = ["derive"] }
tokio = "1"

[dev-dependencies]
proptest = "1"

[target.'cfg(unix)'.dependencies]
nix = "0.29"

[workspace.dependencies]
anyhow = "1"

[lib]
path = "src/core.rs"

[[bin]]
name = "server"
path = "src/bin/server.rs"
`)

	got := Scan(repo)
	if got.Name != "rusty-service" || got.Version != "0.8.1" ||
		got.RustEdition != "2024" || got.RustVersion != "1.85" {
		t.Fatalf("Cargo metadata = %+v", got)
	}
	wantDeps := []string{"anyhow", "nix", "serde", "tokio"}
	if !reflect.DeepEqual(got.Dependencies, wantDeps) {
		t.Fatalf("dependencies = %v, want %v", got.Dependencies, wantDeps)
	}
	if !reflect.DeepEqual(got.DevDependencies, []string{"proptest"}) {
		t.Fatalf("dev dependencies = %v", got.DevDependencies)
	}
	if !reflect.DeepEqual(got.BinEntries, []string{"src/core.rs", "src/bin/server.rs"}) {
		t.Fatalf("entry points = %v", got.BinEntries)
	}
}

func TestScanMixedManifestsKeepsNodeIdentityAndMergesDependencies(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "package.json", `{
		"name": "canonical-name",
		"version": "9.0.0",
		"dependencies": {"react": "19"}
	}`)
	writeProjectFile(t, repo, "go.mod", "module example.com/go-name\n\ngo 1.25\n\nrequire example.com/go-dep v1.0.0\n")
	writeProjectFile(t, repo, "pyproject.toml", `[project]
name = "python-name"
version = "3.0.0"
dependencies = ["requests>=2"]
`)
	writeProjectFile(t, repo, "Cargo.toml", `[package]
name = "cargo-name"
version = "4.0.0"
[dependencies]
serde = "1"
`)

	got := Scan(repo)
	if got.Name != "canonical-name" || got.Version != "9.0.0" {
		t.Fatalf("mixed identity = %s@%s", got.Name, got.Version)
	}
	wantDeps := []string{"example.com/go-dep", "react", "requests", "serde"}
	if !reflect.DeepEqual(got.Dependencies, wantDeps) {
		t.Fatalf("dependencies = %v, want %v", got.Dependencies, wantDeps)
	}
	wantSources := []string{"package.json", "go.mod", "pyproject.toml", "Cargo.toml"}
	if !reflect.DeepEqual(got.Sources, wantSources) {
		t.Fatalf("sources = %v, want %v", got.Sources, wantSources)
	}
}

func TestMalformedManifestsDegradeWithoutBlockingValidMetadata(t *testing.T) {
	repo := t.TempDir()
	writeProjectFile(t, repo, "package.json", `{"name":"still-valid"}`)
	writeProjectFile(t, repo, "go.mod", "module\nrequire (\n")
	writeProjectFile(t, repo, "pyproject.toml", "[project\nname = \"broken\"\n")
	writeProjectFile(t, repo, "Cargo.toml", "[package]\nname = \"broken\"\ndependencies = [\n")

	got := Scan(repo)
	if got.Name != "still-valid" {
		t.Fatalf("valid manifest was lost: %+v", got)
	}
	if !reflect.DeepEqual(got.Sources, []string{"package.json"}) {
		t.Fatalf("malformed manifests should not be sources: %v", got.Sources)
	}
}

func TestScanRejectsSymlinkedAndOversizedManifests(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		repo := t.TempDir()
		target := filepath.Join(t.TempDir(), "package.json")
		if err := os.WriteFile(target, []byte(`{"name":"outside"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(repo, "package.json")); err != nil {
			t.Fatal(err)
		}
		if got := Scan(repo); !got.IsEmpty() {
			t.Fatalf("symlinked manifest was read: %+v", got)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		repo := t.TempDir()
		path := filepath.Join(repo, "go.mod")
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(maxManifestBytes + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if got := Scan(repo); !got.IsEmpty() {
			t.Fatalf("oversized manifest was read: %+v", got)
		}
	})
}

func TestEntryPointsRejectPathsOutsideRepository(t *testing.T) {
	info := Info{
		Main:       "../../outside.js",
		Module:     "/absolute/module.js",
		Types:      "src/types.ts",
		BinEntries: []string{"../escape.py", "cmd/tool.go"},
	}
	got := info.EntryPoints()
	want := []string{"src/types.ts", "cmd/tool.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry points = %v, want %v", got, want)
	}
}

func TestInfoJSONBackwardCompatibility(t *testing.T) {
	legacy := `{
		"name":"legacy",
		"version":"1.0.0",
		"main":"src/index.js",
		"dependencies":["dep"],
		"path_aliases":{"@app":"src"}
	}`
	info := Decode(legacy)
	if info == nil || info.Name != "legacy" || info.Main != "src/index.js" {
		t.Fatalf("legacy decode = %+v", info)
	}
	encoded := info.Encode()
	if encoded == "" {
		t.Fatal("legacy metadata encoded as empty")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"go_module", "go_version", "python_requires", "python_scripts",
		"rust_edition", "rust_version", "optional_dependencies",
	} {
		if _, present := raw[field]; present {
			t.Fatalf("empty new field %q should remain omitted: %s", field, encoded)
		}
	}

	current := Info{
		Name:           "current",
		GoModule:       "example.com/current",
		PythonRequires: ">=3.12",
		RustEdition:    "2024",
	}
	roundTrip := Decode(current.Encode())
	if roundTrip == nil || !reflect.DeepEqual(*roundTrip, current) {
		t.Fatalf("new metadata round trip = %+v, want %+v", roundTrip, current)
	}
}
