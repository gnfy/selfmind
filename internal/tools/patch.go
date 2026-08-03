package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// =============================================================================
// V4A Patch Format
//
// Format:
//   *** Begin Patch
//   *** Update File: path/to/file.py
//   @@ optional context hint @@
//   context line (space prefix)
//   -removed line (minus prefix)
//   +added line (plus prefix)
//   *** Add File: path/to/new.py
//   +new file content
//   *** Delete File: path/to/old.py
//   *** Move File: old/path.py -> new/path.py
//   *** End Patch
// =============================================================================

// OperationType represents the type of patch operation
type OperationType int

const (
	OpAdd OperationType = iota
	OpUpdate
	OpDelete
	OpMove
)

func (o OperationType) String() string {
	switch o {
	case OpAdd:
		return "add"
	case OpUpdate:
		return "update"
	case OpDelete:
		return "delete"
	case OpMove:
		return "move"
	}
	return "unknown"
}

// HunkLine represents a single line within a hunk
type HunkLine struct {
	Prefix  string // ' ', '-', '+'
	Content string
}

// Hunk represents a group of changes within a file
type Hunk struct {
	ContextHint string
	Lines       []HunkLine
}

// PatchOperation represents a single V4A patch operation
type PatchOperation struct {
	Operation OperationType
	FilePath  string
	NewPath   string // For move operations
	Hunks     []Hunk // For update/add operations
	Content   string // For add operations (inline content)
}

// PatchResult represents the result of applying a patch
type PatchResult struct {
	Success       bool
	Diff          string
	FilesModified []string `json:",omitempty"`
	FilesCreated  []string `json:",omitempty"`
	FilesDeleted  []string `json:",omitempty"`
	Error         string   `json:",omitempty"`
}

// parseV4APatch parses a V4A format patch string
func parseV4APatch(patchContent string) ([]PatchOperation, error) {
	if patchContent == "" {
		return nil, nil
	}

	lines := strings.Split(patchContent, "\n")
	var ops []PatchOperation

	// Find patch boundaries
	startIdx, endIdx := -1, len(lines)
	for i, line := range lines {
		if strings.Contains(line, "*** Begin Patch") || strings.Contains(line, "***Begin Patch") {
			startIdx = i
		}
		if strings.Contains(line, "*** End Patch") || strings.Contains(line, "***End Patch") {
			endIdx = i
			break
		}
	}

	if startIdx == -1 {
		startIdx = -1 // allow implicit start
	}
	if endIdx == len(lines) && startIdx == -1 {
		endIdx = len(lines)
	}

	i := startIdx + 1
	var currentOp *PatchOperation
	var currentHunk *Hunk

	parseError := func(msg string) error {
		return fmt.Errorf("parse error: %s", msg)
	}

	for i < endIdx {
		line := lines[i]

		// File operation markers
		updateMatch := regexp.MustCompile(`^\*\*\*\s*Update\s+File:\s*(.+)`).FindStringSubmatch(line)
		addMatch := regexp.MustCompile(`^\*\*\*\s*Add\s+File:\s*(.+)`).FindStringSubmatch(line)
		deleteMatch := regexp.MustCompile(`^\*\*\*\s*Delete\s+File:\s*(.+)`).FindStringSubmatch(line)
		moveMatch := regexp.MustCompile(`^\*\*\*\s*Move\s+File:\s*(.+?)\s*->\s*(.+)`).FindStringSubmatch(line)

		if updateMatch != nil {
			if currentOp != nil {
				if currentHunk != nil && len(currentHunk.Lines) > 0 {
					currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
				}
				if len(currentOp.Hunks) > 0 || currentOp.Operation == OpAdd {
					ops = append(ops, *currentOp)
				}
			}
			currentOp = &PatchOperation{
				Operation: OpUpdate,
				FilePath:  strings.TrimSpace(updateMatch[1]),
			}
			currentHunk = nil

		} else if addMatch != nil {
			if currentOp != nil {
				if currentHunk != nil && len(currentHunk.Lines) > 0 {
					currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
				}
				if len(currentOp.Hunks) > 0 || currentOp.Operation == OpAdd {
					ops = append(ops, *currentOp)
				}
			}
			currentOp = &PatchOperation{
				Operation: OpAdd,
				FilePath:  strings.TrimSpace(addMatch[1]),
			}
			currentHunk = &Hunk{}

		} else if deleteMatch != nil {
			if currentOp != nil {
				if currentHunk != nil && len(currentHunk.Lines) > 0 {
					currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
				}
				if len(currentOp.Hunks) > 0 || currentOp.Operation == OpAdd {
					ops = append(ops, *currentOp)
				}
			}
			ops = append(ops, PatchOperation{
				Operation: OpDelete,
				FilePath:  strings.TrimSpace(deleteMatch[1]),
			})
			currentOp = nil
			currentHunk = nil

		} else if moveMatch != nil {
			if currentOp != nil {
				if currentHunk != nil && len(currentHunk.Lines) > 0 {
					currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
				}
				if len(currentOp.Hunks) > 0 || currentOp.Operation == OpAdd {
					ops = append(ops, *currentOp)
				}
			}
			ops = append(ops, PatchOperation{
				Operation: OpMove,
				FilePath:  strings.TrimSpace(moveMatch[1]),
				NewPath:   strings.TrimSpace(moveMatch[2]),
			})
			currentOp = nil
			currentHunk = nil

		} else if strings.HasPrefix(line, "@@") {
			// Hunk marker
			hint := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "@@"), "@"))
			if currentOp != nil {
				if currentHunk != nil && len(currentHunk.Lines) > 0 {
					currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
				}
				currentHunk = &Hunk{ContextHint: strings.TrimSpace(hint)}
			}

		} else if currentOp != nil && line != "" {
			if currentHunk == nil {
				currentHunk = &Hunk{}
			}
			if strings.HasPrefix(line, "+") {
				currentHunk.Lines = append(currentHunk.Lines, HunkLine{"+", line[1:]})
			} else if strings.HasPrefix(line, "-") {
				currentHunk.Lines = append(currentHunk.Lines, HunkLine{"-", line[1:]})
			} else if strings.HasPrefix(line, " ") {
				currentHunk.Lines = append(currentHunk.Lines, HunkLine{" ", line[1:]})
			} else if strings.HasPrefix(line, `\`) {
				// "\ No newline at end of file" — skip
			} else {
				// Implicit context line (no leading space)
				currentHunk.Lines = append(currentHunk.Lines, HunkLine{" ", line})
			}
		}
		i++
	}

	// Flush last operation
	if currentOp != nil {
		if currentHunk != nil && len(currentHunk.Lines) > 0 {
			currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
		}
		if len(currentOp.Hunks) > 0 || currentOp.Operation == OpAdd {
			ops = append(ops, *currentOp)
		}
	}

	// Validate
	for _, op := range ops {
		if op.FilePath == "" {
			return nil, parseError("operation with empty file path")
		}
		if op.Operation == OpUpdate && len(op.Hunks) == 0 {
			return nil, parseError(fmt.Sprintf("UPDATE %s: no hunks found", op.FilePath))
		}
		if op.Operation == OpMove && op.NewPath == "" {
			return nil, parseError(fmt.Sprintf("MOVE %s: missing destination path", op.FilePath))
		}
	}

	return ops, nil
}

// =============================================================================
// Bounded find-and-replace
// =============================================================================

var patchWhitespaceRE = regexp.MustCompile(`\s+`)

// maxPatchFuzzyComparisons prevents a malformed or stale hunk from turning a
// patch miss into an unbounded CPU job. Exact matching is attempted first; the
// bounded fallback compares only same-length line windows.
const maxPatchFuzzyComparisons = 2_000_000

// fuzzyFindAndReplace finds searchPattern in text and replaces it with replacement.
// Returns (newText, count, strategy, error).
// Strategy is a description of how the match was found.
func fuzzyFindAndReplace(ctx context.Context, text, searchPattern, replacement string) (string, int, string, error) {
	if searchPattern == "" {
		return text, 0, "", fmt.Errorf("search pattern cannot be empty")
	}
	if err := ctx.Err(); err != nil {
		return text, 0, "", err
	}

	searchLines := strings.Split(searchPattern, "\n")
	replLines := strings.Split(replacement, "\n")

	// Exact replacement is safe only when the hunk identifies one location.
	// Picking the first of several equal blocks silently edits the wrong code.
	exactCount := strings.Count(text, searchPattern)
	if exactCount > 1 {
		return text, 0, "", fmt.Errorf("pattern is ambiguous (%d exact matches)", exactCount)
	}
	if exactCount == 1 {
		exactIdx := strings.Index(text, searchPattern)
		newText := text[:exactIdx] + replacement + text[exactIdx+len(searchPattern):]
		return newText, 1, "exact", nil
	}

	// The only fallback normalizes whitespace while preserving line count and
	// order. It is O(file lines * hunk lines), cancellable, and bounded.
	lines := strings.Split(text, "\n")
	if len(searchLines) > len(lines) {
		return text, 0, "", fmt.Errorf("pattern not found")
	}
	comparisons := (len(lines) - len(searchLines) + 1) * len(searchLines)
	if comparisons > maxPatchFuzzyComparisons {
		return text, 0, "", fmt.Errorf("fuzzy match exceeds safety limit (%d line comparisons)", comparisons)
	}
	matchStart := -1
	for i := 0; i <= len(lines)-len(searchLines); i++ {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return text, 0, "", err
			}
		}
		found := true
		for j := 0; j < len(searchLines); j++ {
			if !fuzzyEqual(lines[i+j], searchLines[j]) {
				found = false
				break
			}
		}
		if found {
			if matchStart >= 0 {
				return text, 0, "", fmt.Errorf("pattern is ambiguous (multiple normalized matches)")
			}
			matchStart = i
		}
	}
	if matchStart >= 0 {
		newLines := make([]string, 0, len(lines)-len(searchLines)+len(replLines))
		newLines = append(newLines, lines[:matchStart]...)
		newLines = append(newLines, replLines...)
		newLines = append(newLines, lines[matchStart+len(searchLines):]...)
		return strings.Join(newLines, "\n"), 1, "normalized-whitespace", nil
	}

	return text, 0, "", fmt.Errorf("pattern not found")
}

// fuzzyEqual checks if two lines are equal or semantically similar
func fuzzyEqual(a, b string) bool {
	if a == b {
		return true
	}
	// Strip leading/trailing whitespace and compare
	aa := strings.TrimSpace(a)
	bb := strings.TrimSpace(b)
	if aa == bb {
		return true
	}
	// Check if they're the same after removing whitespace
	normalize := func(s string) string { return patchWhitespaceRE.ReplaceAllString(strings.TrimSpace(s), " ") }
	return normalize(a) == normalize(b)
}

// =============================================================================
// Validation phase
// =============================================================================

func validateOperations(ops []PatchOperation) []string {
	return validateOperationsContext(context.Background(), ops)
}

func validateOperationsContext(ctx context.Context, ops []PatchOperation) []string {
	var errors []string

	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return append(errors, fmt.Sprintf("patch cancelled: %v", err))
		}
		if op.Operation == OpUpdate {
			data, err := os.ReadFile(op.FilePath)
			if err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", op.FilePath, err))
				continue
			}
			content := string(data)

			for _, hunk := range op.Hunks {
				// Collect search lines (context + removed)
				var searchLines []string
				for _, l := range hunk.Lines {
					if l.Prefix != "+" {
						searchLines = append(searchLines, l.Content)
					}
				}
				if len(searchLines) == 0 {
					// Addition-only hunk: validate context hint uniqueness
					if hunk.ContextHint != "" {
						count := strings.Count(content, hunk.ContextHint)
						if count == 0 {
							errors = append(errors,
								fmt.Sprintf("%s: addition-only hunk context hint %q not found",
									op.FilePath, hunk.ContextHint))
						} else if count > 1 {
							errors = append(errors,
								fmt.Sprintf("%s: addition-only hunk context hint %q is ambiguous (%d occurrences)",
									op.FilePath, hunk.ContextHint, count))
						}
					}
					continue
				}

				searchPattern := strings.Join(searchLines, "\n")
				replaceLines := make([]string, 0)
				for _, l := range hunk.Lines {
					if l.Prefix != "-" {
						replaceLines = append(replaceLines, l.Content)
					}
				}
				replacement := strings.Join(replaceLines, "\n")

				newContent, count, _, matchErr := fuzzyFindAndReplace(ctx, content, searchPattern, replacement)
				if count == 0 || matchErr != nil {
					label := fmt.Sprintf("%q", hunk.ContextHint)
					if hunk.ContextHint == "" {
						label = "(no hint)"
					}
					detail := "not found"
					if matchErr != nil {
						detail = matchErr.Error()
					}
					errors = append(errors, fmt.Sprintf("%s: hunk %s %s", op.FilePath, label, detail))
				} else {
					content = newContent
				}
			}

		} else if op.Operation == OpDelete {
			_, err := os.ReadFile(op.FilePath)
			if err != nil {
				errors = append(errors, fmt.Sprintf("%s: file not found for deletion", op.FilePath))
			}

		} else if op.Operation == OpMove {
			if op.NewPath == "" {
				errors = append(errors, fmt.Sprintf("%s: MOVE operation missing destination path", op.FilePath))
			}
			_, err := os.ReadFile(op.FilePath)
			if err != nil {
				errors = append(errors, fmt.Sprintf("%s: source file not found for move", op.FilePath))
			}
			if _, err := os.Stat(op.NewPath); err == nil {
				errors = append(errors, fmt.Sprintf("%s: destination already exists — move would overwrite", op.NewPath))
			}
		}
	}

	return errors
}

// =============================================================================
// Apply phase
// =============================================================================

func applyAdd(op PatchOperation) (string, error) {
	// Collect content from hunks (+ lines)
	var contentLines []string
	for _, hunk := range op.Hunks {
		for _, l := range hunk.Lines {
			if l.Prefix == "+" {
				contentLines = append(contentLines, l.Content)
			}
		}
	}
	content := strings.Join(contentLines, "\n")

	// Ensure parent directory exists
	parent := filepath.Dir(op.FilePath)
	if parent != "" && parent != "." {
		if err := os.MkdirAll(parent, 0755); err != nil {
			return "", fmt.Errorf("failed to create directory %s: %w", parent, err)
		}
	}

	if err := atomicWriteBytes(op.FilePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", op.FilePath, err)
	}

	diff := fmt.Sprintf("--- /dev/null\n+++ b/%s\n%s", op.FilePath,
		strings.Join(func() []string {
			var lines []string
			for _, c := range contentLines {
				lines = append(lines, "+"+c)
			}
			return lines
		}(), "\n"))
	return diff, nil
}

func applyDelete(op PatchOperation) (string, error) {
	data, err := os.ReadFile(op.FilePath)
	if err != nil {
		return "", fmt.Errorf("cannot delete %s: %v", op.FilePath, err)
	}

	if err := os.Remove(op.FilePath); err != nil {
		return "", fmt.Errorf("failed to delete %s: %w", op.FilePath, err)
	}

	// Produce a unified diff (approximation)
	diff := fmt.Sprintf("# Deleted: %s", op.FilePath)
	_ = data // suppress unused warning
	return diff, nil
}

func applyMove(op PatchOperation) (string, error) {
	if err := os.Rename(op.FilePath, op.NewPath); err != nil {
		return "", fmt.Errorf("failed to move %s: %w", op.FilePath, err)
	}
	return fmt.Sprintf("# Moved: %s -> %s", op.FilePath, op.NewPath), nil
}

func applyUpdate(op PatchOperation) (string, error) {
	return applyUpdateContext(context.Background(), op)
}

func applyUpdateContext(ctx context.Context, op PatchOperation) (string, error) {
	data, err := os.ReadFile(op.FilePath)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", op.FilePath, err)
	}
	content := string(data)

	var allDiffs []string
	for _, hunk := range op.Hunks {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var searchLines, replaceLines []string
		for _, l := range hunk.Lines {
			if l.Prefix == "-" || l.Prefix == " " {
				searchLines = append(searchLines, l.Content)
			}
			if l.Prefix == "+" || l.Prefix == " " {
				replaceLines = append(replaceLines, l.Content)
			}
		}
		searchPattern := strings.Join(searchLines, "\n")
		replacement := strings.Join(replaceLines, "\n")

		newContent, count, strategy, err := fuzzyFindAndReplace(ctx, content, searchPattern, replacement)
		if err != nil {
			return "", fmt.Errorf("hunk apply failed: %w", err)
		}
		if count == 0 {
			return "", fmt.Errorf("hunk not found in %s", op.FilePath)
		}

		if hunk.ContextHint != "" {
			allDiffs = append(allDiffs, fmt.Sprintf("# hunk @%s (%s)", hunk.ContextHint, strategy))
		}
		content = newContent
	}

	// Write the modified content
	if err := atomicWriteBytes(op.FilePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("failed to write %s: %w", op.FilePath, err)
	}

	return strings.Join(allDiffs, "\n"), nil
}

func applyV4AOperations(ops []PatchOperation) *PatchResult {
	return applyV4AOperationsContext(context.Background(), ops)
}

func applyV4AOperationsContext(ctx context.Context, ops []PatchOperation) *PatchResult {
	result := &PatchResult{}

	// Phase 1: validate
	validationErrors := validateOperationsContext(ctx, ops)
	if len(validationErrors) > 0 {
		var buf bytes.Buffer
		buf.WriteString("Patch validation failed (no files were modified):\n")
		for _, e := range validationErrors {
			buf.WriteString(fmt.Sprintf("  • %s\n", e))
		}
		result.Success = false
		result.Error = buf.String()
		return result
	}

	// Phase 2: apply
	var allDiffs []string
	var applyErrors []string

	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("patch cancelled: %v", err))
			break
		}
		var diff string
		var err error

		switch op.Operation {
		case OpAdd:
			diff, err = applyAdd(op)
			if err == nil {
				result.FilesCreated = append(result.FilesCreated, op.FilePath)
			}
		case OpDelete:
			diff, err = applyDelete(op)
			if err == nil {
				result.FilesDeleted = append(result.FilesDeleted, op.FilePath)
			}
		case OpMove:
			diff, err = applyMove(op)
			if err == nil {
				result.FilesModified = append(result.FilesModified, fmt.Sprintf("%s -> %s", op.FilePath, op.NewPath))
			}
		case OpUpdate:
			diff, err = applyUpdateContext(ctx, op)
			if err == nil {
				result.FilesModified = append(result.FilesModified, op.FilePath)
			}
		}

		if err != nil {
			applyErrors = append(applyErrors, fmt.Sprintf("%s: %v", op.FilePath, err))
		} else if diff != "" {
			allDiffs = append(allDiffs, diff)
		}
	}

	result.Diff = strings.Join(allDiffs, "\n")

	if len(applyErrors) > 0 {
		var buf bytes.Buffer
		buf.WriteString("Apply phase errors (state may be inconsistent — run `git diff` to assess):\n")
		for _, e := range applyErrors {
			buf.WriteString(fmt.Sprintf("  • %s\n", e))
		}
		result.Success = false
		result.Error = buf.String()
		return result
	}

	result.Success = true
	return result
}

// =============================================================================
// PatchTool
// =============================================================================

type PatchTool struct {
	BaseTool
}

func NewPatchTool() *PatchTool {
	return &PatchTool{
		BaseTool: BaseTool{
			name:        "patch",
			description: "Apply V4A-format patches to multiple files. Use this instead of sed/awk for precise file edits. Supports: Update (find+replace with fuzzy matching), Add (create new file), Delete, and Move. Performs two-phase validation before applying any changes.",
			schema: ToolSchema{
				Type: "object",
				Properties: map[string]PropertyDef{
					"patch": {
						Type:        "string",
						Description: "V4A patch content. Format:\n*** Begin Patch\n*** Update File: path/to/file\n@@ context hint @@\n context line\n-removed line\n+added line\n*** Add File: path/to/new.py\n+new content\n*** Delete File: path/to/old.py\n*** Move File: old.py -> new.py\n*** End Patch",
					},
					"mode": {
						Type:        "string",
						Description: "Execution mode: 'apply' (default) or 'validate' only",
						Default:     "apply",
					},
				},
				Required: []string{"patch"},
			},
		},
	}
}

func (t *PatchTool) Execute(args map[string]interface{}) (string, error) {
	return t.ExecuteContext(ContextFromArgs(args), args)
}

func (t *PatchTool) ExecuteContext(ctx context.Context, args map[string]interface{}) (string, error) {
	patchContent, ok := args["patch"].(string)
	if !ok || patchContent == "" {
		return "", fmt.Errorf("patch content is required")
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "apply"
	}

	ops, err := parseV4APatch(patchContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse patch: %w", err)
	}

	if len(ops) == 0 {
		return "", fmt.Errorf("no operations found in patch")
	}

	if mode == "validate" {
		errors := validateOperationsContext(ctx, ops)
		if len(errors) > 0 {
			var buf bytes.Buffer
			buf.WriteString("Validation failed:\n")
			for _, e := range errors {
				buf.WriteString(fmt.Sprintf("  • %s\n", e))
			}
			return "", fmt.Errorf("%s", buf.String())
		}
		return fmt.Sprintf("Validation passed for %d operation(s)", len(ops)), nil
	}

	result := applyV4AOperationsContext(ctx, ops)
	b, _ := json.Marshal(result)
	return string(b), nil
}
