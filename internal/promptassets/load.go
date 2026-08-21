package promptassets

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

type Mode string

const (
	ModeDefault Mode = "default"
	ModeOff     Mode = "off"
	ModeCustom  Mode = "custom"
)

type Value struct {
	Mode Mode   `json:"mode"`
	Text string `json:"text,omitempty"`
}

type FileState struct {
	ID         string           `json:"id"`
	Path       string           `json:"path"`
	Exists     bool             `json:"exists"`
	Hash       string           `json:"hash"`
	Customized bool             `json:"customized"`
	Sections   map[string]Value `json:"sections"`
}

type Snapshot struct {
	root  string
	hash  string
	files map[string]FileState
}

func Empty(root string) *Snapshot {
	root = filepath.Clean(root)
	s := &Snapshot{root: root, files: make(map[string]FileState)}
	for _, spec := range Catalog() {
		state := defaultFileState(filepath.Join(root, filepath.FromSlash(spec.RelativePath)), spec)
		state.Hash = hashBytes(nil)
		s.files[spec.ID] = state
	}
	s.hash = hashSnapshot(s.files)
	return s
}

func Load(root string) (*Snapshot, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "." || root == "" {
		return nil, fmt.Errorf("prompt root is required")
	}
	rooted, exists, err := openPromptRoot(root)
	if err != nil {
		return nil, err
	}
	if rooted != nil {
		defer rooted.Close()
	}
	s := &Snapshot{root: root, files: make(map[string]FileState)}
	for _, spec := range Catalog() {
		state, err := loadFile(root, rooted, exists, spec)
		if err != nil {
			return nil, err
		}
		s.files[spec.ID] = state
	}
	s.hash = hashSnapshot(s.files)
	return s, nil
}

func openPromptRoot(path string) (*os.Root, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect prompt root %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, false, fmt.Errorf("invalid prompt root: %s must be a directory, not a symlink or special file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return nil, false, fmt.Errorf("invalid prompt root: %s must not be group- or world-writable", path)
	}
	if !promptPathOwnedByCurrentUser(info) {
		return nil, false, fmt.Errorf("invalid prompt root: %s must be owned by the current user", path)
	}
	rooted, err := os.OpenRoot(path)
	if err != nil {
		return nil, false, fmt.Errorf("open prompt root %s: %w", path, err)
	}
	openedInfo, err := rooted.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		_ = rooted.Close()
		if err != nil {
			return nil, false, fmt.Errorf("verify prompt root %s: %w", path, err)
		}
		return nil, false, fmt.Errorf("verify prompt root %s: directory changed while opening", path)
	}
	return rooted, true, nil
}

func loadFile(root string, rooted *os.Root, rootExists bool, spec FileSpec) (FileState, error) {
	path := filepath.Join(root, filepath.FromSlash(spec.RelativePath))
	state := defaultFileState(path, spec)
	if !rootExists || rooted == nil {
		state.Hash = hashBytes(nil)
		return state, nil
	}
	rel := filepath.FromSlash(spec.RelativePath)
	dirsExist, err := validatePromptDirectories(rooted, rel)
	if err != nil {
		return state, fmt.Errorf("invalid prompt %s: %w", spec.ID, err)
	}
	if !dirsExist {
		state.Hash = hashBytes(nil)
		return state, nil
	}
	info, err := rooted.Lstat(rel)
	if os.IsNotExist(err) {
		state.Hash = hashBytes(nil)
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("inspect prompt %s: %w", spec.ID, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return state, fmt.Errorf("invalid prompt %s: %s must be a regular file, not a symlink or special file", spec.ID, path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return state, fmt.Errorf("invalid prompt %s: %s must not be group- or world-writable", spec.ID, path)
	}
	if !promptPathOwnedByCurrentUser(info) {
		return state, fmt.Errorf("invalid prompt %s: %s must be owned by the current user", spec.ID, path)
	}
	if info.Size() > int64(spec.MaxBytes) {
		return state, fmt.Errorf("invalid prompt %s: file is %d bytes, limit is %d", spec.ID, info.Size(), spec.MaxBytes)
	}
	file, err := rooted.Open(rel)
	if err != nil {
		return state, fmt.Errorf("read prompt %s: %w", spec.ID, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return state, fmt.Errorf("inspect opened prompt %s: %w", spec.ID, err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return state, fmt.Errorf("invalid prompt %s: %s changed while opening", spec.ID, path)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(spec.MaxBytes)+1))
	if err != nil {
		return state, fmt.Errorf("read prompt %s: %w", spec.ID, err)
	}
	if len(data) > spec.MaxBytes {
		return state, fmt.Errorf("invalid prompt %s: file exceeds %d byte limit while reading", spec.ID, spec.MaxBytes)
	}
	if !utf8.Valid(data) {
		return state, fmt.Errorf("invalid prompt %s: file is not valid UTF-8", spec.ID)
	}
	sections, err := parseSections(string(data), spec)
	if err != nil {
		return state, fmt.Errorf("invalid prompt %s: %w", spec.ID, err)
	}
	state.Exists = true
	state.Hash = hashBytes(data)
	for name, value := range sections {
		state.Sections[name] = value
		if value.Mode != ModeDefault {
			state.Customized = true
		}
	}
	return state, nil
}

func validatePromptDirectories(rooted *os.Root, relativeFile string) (bool, error) {
	dir := filepath.Dir(relativeFile)
	if dir == "." || dir == "" {
		return true, nil
	}
	current := ""
	for _, component := range strings.Split(dir, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := rooted.Lstat(current)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("directory %s must not be a symlink or special file", current)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return false, fmt.Errorf("directory %s must not be group- or world-writable", current)
		}
		if !promptPathOwnedByCurrentUser(info) {
			return false, fmt.Errorf("directory %s must be owned by the current user", current)
		}
	}
	return true, nil
}

func defaultFileState(path string, spec FileSpec) FileState {
	state := FileState{ID: spec.ID, Path: path, Sections: make(map[string]Value)}
	for _, section := range spec.Sections {
		state.Sections[section.Name] = Value{Mode: ModeDefault}
	}
	return state
}

func parseSections(content string, spec FileSpec) (map[string]Value, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	marked, err := markedSectionGrammar(normalized, spec.MaxBytes)
	if err != nil {
		return nil, err
	}
	if marked {
		return parseMarkedSections(normalized, spec)
	}
	return parseLegacySections(normalized, spec, false)
}

// MigrateLegacyContent converts a markerless prompt file to the current marked
// grammar while preserving every resolved section value. Unknown level-two
// headings inside a known legacy section are treated as ordinary Markdown for
// this explicit migration only; normal startup validation remains strict so a
// misspelled reserved section cannot silently attach to the previous section.
func MigrateLegacyContent(fileID, content string) (string, bool, error) {
	spec, ok := Spec(fileID)
	if !ok {
		return "", false, fmt.Errorf("unknown prompt %q", fileID)
	}
	if len([]byte(content)) > spec.MaxBytes {
		return "", false, fmt.Errorf("prompt %s is %d bytes, limit is %d", fileID, len([]byte(content)), spec.MaxBytes)
	}
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	marked, err := markedSectionGrammar(normalized, spec.MaxBytes)
	if err != nil {
		return "", false, err
	}
	if marked {
		return content, false, nil
	}
	values, err := parseLegacySections(normalized, spec, true)
	if err != nil {
		return "", false, err
	}
	return renderTemplate(spec, values), true, nil
}

func parseLegacySections(content string, spec FileSpec, allowBodyHeadings bool) (map[string]Value, error) {
	policies := make(map[string]SectionPolicy, len(spec.Sections))
	for _, policy := range spec.Sections {
		policies[policy.Name] = policy
	}
	raw := make(map[string]*strings.Builder)
	var current string
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024), spec.MaxBytes+1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "## "))
			policy, ok := policies[name]
			if !ok {
				if allowBodyHeadings && current != "" {
					raw[current].WriteString(line)
					raw[current].WriteByte('\n')
					continue
				}
				return nil, fmt.Errorf("unknown section %q at line %d", name, lineNo)
			}
			if _, duplicate := raw[name]; duplicate {
				return nil, fmt.Errorf("duplicate section %q at line %d", name, lineNo)
			}
			raw[name] = &strings.Builder{}
			current = policy.Name
			continue
		}
		if current == "" {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "<!--") || strings.HasSuffix(trimmed, "-->") {
				continue
			}
			return nil, fmt.Errorf("content before the first known section at line %d", lineNo)
		}
		raw[current].WriteString(line)
		raw[current].WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return resolveSectionValues(raw, policies)
}

func markedSectionGrammar(content string, maxBytes int) (bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024), maxBytes+1024)
	lineNo := 0
	inFence := false
	for scanner.Scan() {
		lineNo++
		trimmed := strings.TrimSpace(scanner.Text())
		if markdownFenceLine(trimmed) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if _, ok := exactSectionStart(trimmed); ok || trimmed == "<!-- selfmind:end -->" {
			return true, nil
		}
		if malformedSectionMarker(trimmed) {
			return false, fmt.Errorf("malformed selfmind section marker at line %d; expected <!-- selfmind:section Name --> or <!-- selfmind:end -->", lineNo)
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func exactSectionStart(trimmed string) (string, bool) {
	const prefix = "<!-- selfmind:section "
	const suffix = " -->"
	if !strings.HasPrefix(trimmed, prefix) || !strings.HasSuffix(trimmed, suffix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, prefix), suffix))
	return name, name != ""
}

func malformedSectionMarker(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "<!--") {
		return false
	}
	body := strings.TrimSpace(strings.TrimPrefix(trimmed, "<!--"))
	body = strings.TrimSpace(strings.TrimSuffix(body, "-->"))
	return strings.HasPrefix(body, "selfmind:section") || strings.HasPrefix(body, "selfmind:end")
}

func markdownFenceLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func parseMarkedSections(content string, spec FileSpec) (map[string]Value, error) {
	policies := make(map[string]SectionPolicy, len(spec.Sections))
	for _, policy := range spec.Sections {
		policies[policy.Name] = policy
	}
	raw := make(map[string]*strings.Builder)
	current := ""
	expectDisplayHeading := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024), spec.MaxBytes+1024)
	lineNo := 0
	inFence := false
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if markdownFenceLine(trimmed) {
			if current == "" {
				return nil, fmt.Errorf("content outside a section marker at line %d", lineNo)
			}
			raw[current].WriteString(line)
			raw[current].WriteByte('\n')
			inFence = !inFence
			expectDisplayHeading = false
			continue
		}
		if !inFence {
			if name, ok := exactSectionStart(trimmed); ok {
				if current != "" {
					return nil, fmt.Errorf("nested section marker at line %d", lineNo)
				}
				if _, ok := policies[name]; !ok {
					return nil, fmt.Errorf("unknown section %q at line %d", name, lineNo)
				}
				if _, duplicate := raw[name]; duplicate {
					return nil, fmt.Errorf("duplicate section %q at line %d", name, lineNo)
				}
				raw[name] = &strings.Builder{}
				current = name
				expectDisplayHeading = true
				continue
			}
			if trimmed == "<!-- selfmind:end -->" {
				if current == "" {
					return nil, fmt.Errorf("section end without a start at line %d", lineNo)
				}
				current = ""
				expectDisplayHeading = false
				continue
			}
			if malformedSectionMarker(trimmed) {
				return nil, fmt.Errorf("malformed selfmind section marker at line %d; expected <!-- selfmind:section Name --> or <!-- selfmind:end -->", lineNo)
			}
		}
		if current == "" {
			if trimmed == "" || strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "<!--") || strings.HasSuffix(trimmed, "-->") {
				continue
			}
			return nil, fmt.Errorf("content outside a section marker at line %d", lineNo)
		}
		if expectDisplayHeading && trimmed == "" {
			continue
		}
		if expectDisplayHeading && trimmed == "## "+current {
			expectDisplayHeading = false
			continue
		}
		expectDisplayHeading = false
		raw[current].WriteString(line)
		raw[current].WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if inFence {
		return nil, fmt.Errorf("section %q contains an unterminated Markdown fence", current)
	}
	if current != "" {
		return nil, fmt.Errorf("section %q is missing <!-- selfmind:end -->", current)
	}
	return resolveSectionValues(raw, policies)
}

func resolveSectionValues(raw map[string]*strings.Builder, policies map[string]SectionPolicy) (map[string]Value, error) {
	values := make(map[string]Value, len(raw))
	for name, body := range raw {
		policy := policies[name]
		text := strings.TrimSpace(body.String())
		switch strings.ToLower(text) {
		case "", "default":
			values[name] = Value{Mode: ModeDefault}
		case "off":
			if !policy.AllowOff {
				return nil, fmt.Errorf("section %q cannot be disabled", name)
			}
			values[name] = Value{Mode: ModeOff}
		default:
			if len([]byte(text)) > policy.MaxBytes {
				return nil, fmt.Errorf("section %q is %d bytes, limit is %d", name, len([]byte(text)), policy.MaxBytes)
			}
			values[name] = Value{Mode: ModeCustom, Text: text}
		}
	}
	return values, nil
}

func (s *Snapshot) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *Snapshot) Hash() string {
	if s == nil {
		return hashSnapshot(nil)
	}
	return s.hash
}

// SectionHash identifies the resolved operator value for one catalog section.
// It deliberately excludes file formatting and unrelated prompt roles so
// checkpoints only change when the guidance they actually consume changes.
func (s *Snapshot) SectionHash(fileID, section string) string {
	value := s.Value(fileID, section)
	var b strings.Builder
	fmt.Fprintf(&b, "catalog=%d\nfile=%s\nsection=%s:%s:%s\n", CatalogVersion, fileID, section, value.Mode, value.Text)
	return hashBytes([]byte(b.String()))
}

func (s *Snapshot) Value(fileID, section string) Value {
	if s == nil {
		return Value{Mode: ModeDefault}
	}
	state, ok := s.files[fileID]
	if !ok {
		return Value{Mode: ModeDefault}
	}
	value, ok := state.Sections[section]
	if !ok {
		return Value{Mode: ModeDefault}
	}
	return value
}

func (s *Snapshot) Custom(fileID, section string) string {
	value := s.Value(fileID, section)
	if value.Mode != ModeCustom {
		return ""
	}
	return value.Text
}

func (s *Snapshot) Off(fileID, section string) bool {
	return s.Value(fileID, section).Mode == ModeOff
}

// Compose applies catalog-owned replace/append semantics to one built-in
// prompt slot. Callers provide the locked/default base and the ordered section
// ids they inject there; they do not reimplement SectionPolicy.Replace.
func (s *Snapshot) Compose(fileID, base string, sections ...string) string {
	spec, ok := Spec(fileID)
	if !ok {
		return base
	}
	policies := make(map[string]SectionPolicy, len(spec.Sections))
	for _, policy := range spec.Sections {
		policies[policy.Name] = policy
	}
	result := base
	var appended []string
	for _, section := range sections {
		policy, ok := policies[section]
		if !ok {
			continue
		}
		value := s.Value(fileID, section)
		if policy.Replace {
			switch value.Mode {
			case ModeCustom:
				result = value.Text
			case ModeOff:
				result = ""
			}
			continue
		}
		if value.Mode == ModeCustom {
			appended = append(appended, value.Text)
		}
	}
	return AppendOperatorGuidance(result, appended...)
}

func (s *Snapshot) File(fileID string) (FileState, bool) {
	if s == nil {
		return FileState{}, false
	}
	state, ok := s.files[fileID]
	if !ok {
		return FileState{}, false
	}
	copy := state
	copy.Sections = make(map[string]Value, len(state.Sections))
	for key, value := range state.Sections {
		copy.Sections[key] = value
	}
	return copy, true
}

func (s *Snapshot) Files() []FileState {
	if s == nil {
		return nil
	}
	ids := make([]string, 0, len(s.files))
	for id := range s.files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]FileState, 0, len(ids))
	for _, id := range ids {
		state, _ := s.File(id)
		out = append(out, state)
	}
	return out
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hashSnapshot(files map[string]FileState) string {
	var b strings.Builder
	fmt.Fprintf(&b, "catalog=%d\n", CatalogVersion)
	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		state := files[id]
		fmt.Fprintf(&b, "file=%s\n", id)
		names := make([]string, 0, len(state.Sections))
		for name := range state.Sections {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := state.Sections[name]
			fmt.Fprintf(&b, "section=%s:%s:%s\n", name, value.Mode, value.Text)
		}
	}
	return hashBytes([]byte(b.String()))
}

func PromptRoot(configPath, dataDir string) string {
	if path := strings.TrimSpace(configPath); path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		return filepath.Join(filepath.Dir(path), "prompts")
	}
	if dir := strings.TrimSpace(dataDir); dir != "" {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		return filepath.Join(filepath.Dir(dir), "prompts")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".selfmind", "prompts")
}
