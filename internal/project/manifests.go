package project

import (
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Gere2/neurofs/internal/fsutil"
)

// ─── go.mod ──────────────────────────────────────────────────────────────────

type goManifest struct {
	Module       string
	GoVersion    string
	Dependencies []string
}

func readGoMod(path string) *goManifest {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxManifestBytes)
	if err != nil {
		return nil
	}

	var out goManifest
	inRequireBlock := false
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(stripGoModComment(rawLine))
		if line == "" {
			continue
		}
		if inRequireBlock {
			if line == ")" {
				inRequireBlock = false
				continue
			}
			if dep := goRequirePath(line); dep != "" {
				out.Dependencies = append(out.Dependencies, dep)
			}
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "module":
			if len(fields) < 2 {
				return nil
			}
			out.Module = unquoteToken(fields[1])
		case "go":
			if len(fields) >= 2 {
				out.GoVersion = unquoteToken(fields[1])
			}
		case "require":
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require"))
			if rest == "(" {
				inRequireBlock = true
				continue
			}
			if dep := goRequirePath(rest); dep != "" {
				out.Dependencies = append(out.Dependencies, dep)
			}
		}
	}
	if inRequireBlock || out.Module == "" {
		return nil
	}
	out.Dependencies = mergeSorted(nil, out.Dependencies)
	return &out
}

func stripGoModComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

func goRequirePath(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	dep := unquoteToken(fields[0])
	if dep == "" || dep == ")" {
		return ""
	}
	return dep
}

func unquoteToken(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') ||
		(s[0] == '`' && s[len(s)-1] == '`') {
		if value, err := strconv.Unquote(s); err == nil {
			return value
		}
	}
	return s
}

func moduleBase(module string) string {
	module = strings.TrimSuffix(strings.TrimSpace(module), "/")
	if module == "" {
		return ""
	}
	return pathpkg.Base(module)
}

// ─── minimal TOML reader ─────────────────────────────────────────────────────

// The project only needs a narrow, read-only TOML subset: table headers,
// scalar strings, booleans and arrays of strings. Unsupported values remain
// opaque, while structurally incomplete headers/arrays cause that manifest to
// be ignored. This keeps scan tolerant without introducing a parser dependency.
type tomlAssignment struct {
	Key   string
	Value string
}

type tomlDocument struct {
	sections map[string][]tomlAssignment
}

func parseTOML(data []byte) *tomlDocument {
	doc := &tomlDocument{sections: make(map[string][]tomlAssignment)}
	section := ""
	lines := strings.Split(string(data), "\n")

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripTOMLComment(lines[i]))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[[") {
			if !strings.HasSuffix(line, "]]") || len(line) < 4 {
				return nil
			}
			section = normaliseTOMLSection(line[2 : len(line)-2])
			if section == "" {
				return nil
			}
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") || len(line) < 3 {
				return nil
			}
			section = normaliseTOMLSection(line[1 : len(line)-1])
			if section == "" {
				return nil
			}
			continue
		}

		eq := indexOutsideTOMLString(line, '=')
		if eq < 0 {
			// This can be a line inside a TOML construct we do not need
			// (for example a multiline description). Ignore it conservatively.
			continue
		}
		key := normaliseTOMLKey(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		if key == "" || value == "" {
			continue
		}

		if delimiter := multilineTOMLDelimiter(value); delimiter != "" &&
			strings.Count(value, delimiter) < 2 {
			closed := false
			for i+1 < len(lines) {
				i++
				if strings.Contains(lines[i], delimiter) {
					closed = true
					break
				}
			}
			if !closed {
				return nil
			}
		}

		if strings.HasPrefix(strings.TrimSpace(value), "[") {
			depth := tomlSquareBracketDelta(value)
			for depth > 0 && i+1 < len(lines) {
				i++
				next := strings.TrimSpace(stripTOMLComment(lines[i]))
				value += "\n" + next
				depth += tomlSquareBracketDelta(next)
			}
			if depth != 0 {
				return nil
			}
		}
		doc.sections[section] = append(doc.sections[section], tomlAssignment{
			Key:   key,
			Value: value,
		})
	}
	return doc
}

func stripTOMLComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '#' {
			return line[:i]
		}
	}
	return line
}

func indexOutsideTOMLString(s string, target byte) int {
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == target {
			return i
		}
	}
	return -1
}

func multilineTOMLDelimiter(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, `"""`):
		return `"""`
	case strings.HasPrefix(value, `'''`):
		return `'''`
	default:
		return ""
	}
}

func tomlSquareBracketDelta(s string) int {
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && c == '\\' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		switch c {
		case '[':
			depth++
		case ']':
			depth--
		}
	}
	return depth
}

func normaliseTOMLSection(section string) string {
	return strings.ToLower(strings.TrimSpace(section))
}

func normaliseTOMLKey(key string) string {
	key = strings.TrimSpace(key)
	if value, ok := parseTOMLString(key); ok {
		return value
	}
	return key
}

func (d *tomlDocument) assignments(section string) []tomlAssignment {
	if d == nil {
		return nil
	}
	return d.sections[strings.ToLower(section)]
}

func (d *tomlDocument) stringValue(section, key string) string {
	assignments := d.assignments(section)
	for i := len(assignments) - 1; i >= 0; i-- {
		if assignments[i].Key != key {
			continue
		}
		if value, ok := parseTOMLString(assignments[i].Value); ok {
			return value
		}
	}
	return ""
}

func (d *tomlDocument) boolValue(section, key string) (bool, bool) {
	assignments := d.assignments(section)
	for i := len(assignments) - 1; i >= 0; i-- {
		if assignments[i].Key != key {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(assignments[i].Value)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func parseTOMLString(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return "", false
	}
	switch raw[0] {
	case '"':
		if raw[len(raw)-1] != '"' || strings.HasPrefix(raw, `"""`) {
			return "", false
		}
		value, err := strconv.Unquote(raw)
		return value, err == nil
	case '\'':
		if raw[len(raw)-1] != '\'' || strings.HasPrefix(raw, `'''`) {
			return "", false
		}
		return raw[1 : len(raw)-1], true
	default:
		return "", false
	}
}

func parseTOMLStringArray(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, false
	}
	body := raw[1 : len(raw)-1]
	var out []string
	for i := 0; i < len(body); {
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' ||
			body[i] == '\r' || body[i] == '\n' || body[i] == ',') {
			i++
		}
		if i >= len(body) {
			break
		}
		quote := body[i]
		if quote != '"' && quote != '\'' {
			return nil, false
		}
		start := i
		i++
		escaped := false
		for i < len(body) {
			c := body[i]
			if quote == '"' && escaped {
				escaped = false
				i++
				continue
			}
			if quote == '"' && c == '\\' {
				escaped = true
				i++
				continue
			}
			if c == quote {
				i++
				break
			}
			i++
		}
		if i > len(body) || body[i-1] != quote {
			return nil, false
		}
		value, ok := parseTOMLString(body[start:i])
		if !ok {
			return nil, false
		}
		out = append(out, value)
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' ||
			body[i] == '\r' || body[i] == '\n') {
			i++
		}
		if i < len(body) && body[i] != ',' {
			return nil, false
		}
	}
	return out, true
}

// ─── pyproject.toml ──────────────────────────────────────────────────────────

type pythonManifest struct {
	Name                 string
	Version              string
	RequiresPython       string
	Scripts              map[string]string
	Dependencies         []string
	DevDependencies      []string
	OptionalDependencies []string
}

func readPyProject(path string) *pythonManifest {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxManifestBytes)
	if err != nil {
		return nil
	}
	doc := parseTOML(data)
	if doc == nil {
		return nil
	}

	out := &pythonManifest{
		Name:           firstNonEmpty(doc.stringValue("project", "name"), doc.stringValue("tool.poetry", "name")),
		Version:        firstNonEmpty(doc.stringValue("project", "version"), doc.stringValue("tool.poetry", "version")),
		RequiresPython: doc.stringValue("project", "requires-python"),
	}

	if assignments := doc.assignments("project"); len(assignments) > 0 {
		for _, assignment := range assignments {
			if assignment.Key != "dependencies" {
				continue
			}
			if values, ok := parseTOMLStringArray(assignment.Value); ok {
				for _, value := range values {
					if dep := pythonDependencyName(value); dep != "" {
						out.Dependencies = append(out.Dependencies, dep)
					}
				}
			}
		}
	}

	for _, assignment := range doc.assignments("project.optional-dependencies") {
		if values, ok := parseTOMLStringArray(assignment.Value); ok {
			for _, value := range values {
				if dep := pythonDependencyName(value); dep != "" {
					out.OptionalDependencies = append(out.OptionalDependencies, dep)
				}
			}
		}
	}

	for _, assignment := range doc.assignments("tool.poetry.dependencies") {
		if strings.EqualFold(assignment.Key, "python") {
			if out.RequiresPython == "" {
				out.RequiresPython, _ = parseTOMLString(assignment.Value)
			}
			continue
		}
		out.Dependencies = append(out.Dependencies, assignment.Key)
	}
	for section, assignments := range doc.sections {
		if !isPoetryDevDependencySection(section) {
			continue
		}
		for _, assignment := range assignments {
			out.DevDependencies = append(out.DevDependencies, assignment.Key)
		}
	}

	out.Scripts = collectPythonScripts(doc)
	out.Dependencies = mergeSorted(nil, out.Dependencies)
	out.DevDependencies = mergeSorted(nil, out.DevDependencies)
	out.OptionalDependencies = mergeSorted(nil, out.OptionalDependencies)
	return out
}

func pythonDependencyName(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	for i, r := range spec {
		if strings.ContainsRune("[<>=!~;@() \t", r) {
			return strings.TrimSpace(spec[:i])
		}
	}
	return spec
}

func isPoetryDevDependencySection(section string) bool {
	if section == "tool.poetry.dev-dependencies" {
		return true
	}
	const prefix = "tool.poetry.group."
	const suffix = ".dependencies"
	if !strings.HasPrefix(section, prefix) || !strings.HasSuffix(section, suffix) {
		return false
	}
	group := strings.TrimSuffix(strings.TrimPrefix(section, prefix), suffix)
	return strings.Contains(group, "dev") ||
		strings.Contains(group, "test") ||
		strings.Contains(group, "lint") ||
		strings.Contains(group, "docs")
}

func collectPythonScripts(doc *tomlDocument) map[string]string {
	var scripts map[string]string
	for _, section := range []string{"project.scripts", "project.gui-scripts", "tool.poetry.scripts"} {
		for _, assignment := range doc.assignments(section) {
			value, ok := parseTOMLString(assignment.Value)
			if !ok || value == "" {
				continue
			}
			if scripts == nil {
				scripts = make(map[string]string)
			}
			if _, exists := scripts[assignment.Key]; !exists {
				scripts[assignment.Key] = value
			}
		}
	}
	return scripts
}

func resolvePythonEntries(repoRoot string, scripts map[string]string) []string {
	if len(scripts) == 0 {
		return nil
	}
	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sort.Strings(names)

	var entries []string
	for _, name := range names {
		target := strings.TrimSpace(strings.SplitN(scripts[name], ":", 2)[0])
		if target == "" {
			continue
		}
		var candidates []string
		if strings.HasSuffix(target, ".py") || strings.Contains(target, "/") {
			candidates = append(candidates, filepath.FromSlash(target))
		} else {
			modulePath := filepath.FromSlash(strings.ReplaceAll(target, ".", "/"))
			candidates = append(candidates,
				modulePath+".py",
				filepath.Join(modulePath, "__main__.py"),
				filepath.Join("src", modulePath+".py"),
				filepath.Join("src", modulePath, "__main__.py"),
			)
		}
		for _, candidate := range candidates {
			if entry, ok := safeExistingEntry(repoRoot, candidate); ok {
				entries = appendUnique(entries, entry)
				break
			}
		}
	}
	return entries
}

// ─── Cargo.toml ──────────────────────────────────────────────────────────────

type cargoManifest struct {
	Name            string
	Version         string
	Edition         string
	RustVersion     string
	Dependencies    []string
	DevDependencies []string
	EntryPoints     []string
}

func readCargoManifest(path, repoRoot string) *cargoManifest {
	data, _, err := fsutil.ReadRegularFileBounded(path, maxManifestBytes)
	if err != nil {
		return nil
	}
	doc := parseTOML(data)
	if doc == nil {
		return nil
	}

	out := &cargoManifest{
		Name:        doc.stringValue("package", "name"),
		Version:     doc.stringValue("package", "version"),
		Edition:     doc.stringValue("package", "edition"),
		RustVersion: doc.stringValue("package", "rust-version"),
	}

	for section, assignments := range doc.sections {
		dev, table, nested := cargoDependencySection(section)
		if !table && nested == "" {
			continue
		}
		var names []string
		if nested != "" {
			names = []string{nested}
		} else {
			names = make([]string, 0, len(assignments))
			for _, assignment := range assignments {
				names = append(names, assignment.Key)
			}
		}
		if dev {
			out.DevDependencies = append(out.DevDependencies, names...)
		} else {
			out.Dependencies = append(out.Dependencies, names...)
		}
	}

	if lib := doc.stringValue("lib", "path"); lib != "" {
		out.EntryPoints = appendUnique(out.EntryPoints, normaliseEntry(lib))
	} else if regularFile(filepath.Join(repoRoot, "src", "lib.rs")) {
		out.EntryPoints = appendUnique(out.EntryPoints, "src/lib.rs")
	}

	var binNames []string
	for _, assignment := range doc.assignments("bin") {
		switch assignment.Key {
		case "path":
			if value, ok := parseTOMLString(assignment.Value); ok {
				out.EntryPoints = appendUnique(out.EntryPoints, normaliseEntry(value))
			}
		case "name":
			if value, ok := parseTOMLString(assignment.Value); ok {
				binNames = append(binNames, value)
			}
		}
	}
	autobins, hasAutobins := doc.boolValue("package", "autobins")
	if !hasAutobins || autobins {
		if regularFile(filepath.Join(repoRoot, "src", "main.rs")) {
			out.EntryPoints = appendUnique(out.EntryPoints, "src/main.rs")
		}
		for _, name := range binNames {
			for _, candidate := range []string{
				filepath.Join("src", "bin", name+".rs"),
				filepath.Join("src", "bin", name, "main.rs"),
			} {
				if regularFile(filepath.Join(repoRoot, candidate)) {
					out.EntryPoints = appendUnique(out.EntryPoints, filepath.ToSlash(candidate))
					break
				}
			}
		}
	}

	out.Dependencies = mergeSorted(nil, out.Dependencies)
	out.DevDependencies = mergeSorted(nil, out.DevDependencies)
	return out
}

func cargoDependencySection(section string) (dev bool, table bool, nested string) {
	kinds := []struct {
		name string
		dev  bool
	}{
		{name: "dev-dependencies", dev: true},
		{name: "build-dependencies"},
		{name: "dependencies"},
	}
	for _, kind := range kinds {
		if section == kind.name || strings.HasSuffix(section, "."+kind.name) {
			return kind.dev, true, ""
		}
		if strings.HasPrefix(section, kind.name+".") {
			return kind.dev, false, strings.TrimPrefix(section, kind.name+".")
		}
		marker := "." + kind.name + "."
		if i := strings.LastIndex(section, marker); i >= 0 {
			return kind.dev, false, section[i+len(marker):]
		}
	}
	return false, false, ""
}

func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func safeExistingEntry(repoRoot, candidate string) (string, bool) {
	entry := normaliseEntry(candidate)
	if entry == "" {
		return "", false
	}
	if !regularFile(filepath.Join(repoRoot, filepath.FromSlash(entry))) {
		return "", false
	}
	return entry, true
}

// ─── shared aggregation helpers ──────────────────────────────────────────────

func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]bool, len(base)+len(values))
	out := make([]string, 0, len(base)+len(values))
	for _, value := range append(append([]string(nil), base...), values...) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func mergeSorted(base, values []string) []string {
	out := appendUnique(base, values...)
	sort.Strings(out)
	return out
}
