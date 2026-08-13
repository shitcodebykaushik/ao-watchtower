// Package repopolicy enforces repository-owned guardrails on Watchtower
// automation. A repository owner commits a .watchtower.json file at the
// repository root to narrow what an autonomous fix may touch; the file can only
// tighten operator flags, never loosen them. A repository without the file keeps
// the permissive behaviour auto-fix had before policies existed.
package repopolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

const (
	// PolicyFileName is the repository-root file a repository owner commits to constrain automation.
	PolicyFileName = ".watchtower.json"
	// PolicyVersion is the only repository policy schema version this build understands.
	PolicyVersion = 1
)

const (
	maximumPolicyBytes      = 64 << 10
	maximumPatternCount     = 256
	maximumPatternLength    = 256
	maximumCategoryCount    = 16
	maximumEvidenceFiles    = 32
	maximumPathSegments     = 64
	maximumPathLength       = 1024
	maximumDetailLength     = 256
	maximumDetailPathLength = 120
)

// Machine-readable Decision reasons. Callers may branch on these; the human
// readable explanation always lives in Decision.Detail.
const (
	ReasonAllowed            = "allowed"
	ReasonInvalidPolicy      = "invalid_policy"
	ReasonInvalidDiagnosis   = "invalid_diagnosis"
	ReasonInvalidFloor       = "invalid_floor"
	ReasonCategoryNotAllowed = "category_not_allowed"
	ReasonBelowConfidence    = "below_confidence"
	ReasonNoEvidenceFile     = "no_evidence_file"
	ReasonTooManyFiles       = "too_many_files"
	ReasonInvalidPath        = "invalid_path"
	ReasonDeniedPath         = "denied_path"
	ReasonPathNotAllowed     = "path_not_allowed"
)

// AutoFixPolicy is the repository owner's constraint on automated code changes.
// Every field is optional and its zero value is permissive, so a policy that
// omits a field defers that decision to the operator running Watchtower.
type AutoFixPolicy struct {
	// MinimumConfidence is the repository's own confidence floor. It is combined
	// with the operator's flag by taking the stricter of the two.
	MinimumConfidence float64 `json:"minimumConfidence,omitempty"`
	// AllowedPaths, when non-empty, requires every evidence file to match at
	// least one pattern. An empty list allows any path that is not denied.
	AllowedPaths []string `json:"allowedPaths,omitempty"`
	// DeniedPaths rejects the diagnosis when any evidence file matches. Denial
	// always beats an allow match.
	DeniedPaths []string `json:"deniedPaths,omitempty"`
	// AllowedCategories, when non-empty, restricts which diagnosis categories may
	// be auto-dispatched. An empty list allows every category the operator allows.
	AllowedCategories []string `json:"allowedCategories,omitempty"`
	// RequireEvidenceFile rejects a diagnosis that names no file, because a
	// diagnosis without a path cannot be path-checked at all.
	RequireEvidenceFile bool `json:"requireEvidenceFile,omitempty"`
	// MaxEvidenceFiles bounds the blast radius of one automated fix. Zero means
	// the repository imposes no bound of its own.
	MaxEvidenceFiles int `json:"maxEvidenceFiles,omitempty"`
}

// Constrains reports whether the repository imposes any restriction of its own
// beyond the operator's flags. It exists so startup can tell the user that a
// committed policy is in force rather than leaving it invisible.
func (a AutoFixPolicy) Constrains() bool {
	return a.MinimumConfidence > 0 || len(a.AllowedPaths) > 0 || len(a.DeniedPaths) > 0 ||
		len(a.AllowedCategories) > 0 || a.RequireEvidenceFile || a.MaxEvidenceFiles > 0
}

// Policy is a parsed and validated .watchtower.json. The zero Policy is
// deliberately unusable: it carries an unsupported version, so an ignored Load
// error can never be mistaken for a permissive policy.
type Policy struct {
	Version int           `json:"version"`
	AutoFix AutoFixPolicy `json:"autoFix"`
}

// Decision is the auditable outcome of evaluating one diagnosis against a policy.
type Decision struct {
	// Allowed reports whether the diagnosis may be auto-dispatched.
	Allowed bool
	// Reason is a short machine-readable outcome code, one of the Reason constants.
	Reason string
	// Detail is a bounded, printable explanation that is safe to render on the dashboard.
	Detail string
}

// Default returns the permissive policy used when a repository commits no
// .watchtower.json. It preserves the pre-policy behaviour of auto-fix exactly:
// only the operator's own flags constrain automation.
func Default() Policy {
	return Policy{Version: PolicyVersion}
}

// Load reads .watchtower.json from a repository root. A missing file returns
// Default and no error. A file that exists but cannot be read, parsed, or
// validated is an error and returns the unusable zero Policy: an owner who
// clearly intended a policy must never be silently downgraded to permissive.
func Load(repositoryRoot string) (Policy, error) {
	if strings.TrimSpace(repositoryRoot) == "" {
		return Policy{}, fmt.Errorf("repository root is required")
	}
	path := filepath.Join(repositoryRoot, PolicyFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("read %s: %w", PolicyFileName, err)
	}
	if len(raw) > maximumPolicyBytes {
		return Policy{}, fmt.Errorf("read %s: policy exceeds %d bytes", PolicyFileName, maximumPolicyBytes)
	}
	policy, err := Parse(raw)
	if err != nil {
		return Policy{}, fmt.Errorf("%s: %w", PolicyFileName, err)
	}
	return policy, nil
}

// Parse validates raw policy bytes without touching a filesystem. Unknown
// fields, unknown versions, trailing JSON values, and unusable glob patterns are
// all errors rather than a fallback to permissive defaults.
func Parse(raw []byte) (Policy, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("decode repository policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Policy{}, fmt.Errorf("decode repository policy: multiple JSON values")
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Validate reports whether a policy is a supported version whose every
// constraint is enforceable. Callers that build a Policy by hand should call it;
// Parse, Load, and EvaluateAutoFix already do.
func (p Policy) Validate() error {
	if p.Version != PolicyVersion {
		return fmt.Errorf("unsupported repository policy version %d", p.Version)
	}
	return p.AutoFix.validate()
}

func (a AutoFixPolicy) validate() error {
	if math.IsNaN(a.MinimumConfidence) || a.MinimumConfidence < 0 || a.MinimumConfidence > 1 {
		return fmt.Errorf("autoFix.minimumConfidence must be between 0 and 1")
	}
	if a.MaxEvidenceFiles < 0 || a.MaxEvidenceFiles > maximumEvidenceFiles {
		return fmt.Errorf("autoFix.maxEvidenceFiles must be between 0 and %d", maximumEvidenceFiles)
	}
	if len(a.AllowedPaths) > maximumPatternCount || len(a.DeniedPaths) > maximumPatternCount {
		return fmt.Errorf("autoFix path lists hold at most %d patterns", maximumPatternCount)
	}
	for index, pattern := range a.AllowedPaths {
		if err := validatePattern(pattern); err != nil {
			return fmt.Errorf("autoFix.allowedPaths %d: %w", index, err)
		}
	}
	for index, pattern := range a.DeniedPaths {
		if err := validatePattern(pattern); err != nil {
			return fmt.Errorf("autoFix.deniedPaths %d: %w", index, err)
		}
	}
	if len(a.AllowedCategories) > maximumCategoryCount {
		return fmt.Errorf("autoFix.allowedCategories holds at most %d entries", maximumCategoryCount)
	}
	for index, category := range a.AllowedCategories {
		if !supportedCategory(category) {
			return fmt.Errorf("autoFix.allowedCategories %d: unsupported diagnosis category", index)
		}
	}
	return nil
}

// EvaluateAutoFix decides whether a validated diagnosis may be auto-dispatched.
// floorConfidence is the operator's own command line floor; the effective
// minimum is the stricter of it and the repository's minimumConfidence, so a
// repository may tighten the operator's flag but never loosen it. Evidence file
// paths originate from a language model and are treated as hostile: absolute,
// backslash-separated, traversing, and unrepresentable paths are all denied.
func (p Policy) EvaluateAutoFix(diagnosis domain.Diagnosis, floorConfidence float64) Decision {
	if err := p.Validate(); err != nil {
		return deny(ReasonInvalidPolicy, fmt.Sprintf("repository policy is not enforceable: %v", err))
	}
	if math.IsNaN(floorConfidence) || floorConfidence < 0 || floorConfidence > 1 {
		return deny(ReasonInvalidFloor, "operator confidence floor must be between 0 and 1")
	}
	if err := diagnosis.Validate(); err != nil {
		return deny(ReasonInvalidDiagnosis, fmt.Sprintf("diagnosis is not valid: %v", err))
	}
	if !p.categoryAllowed(diagnosis.Category) {
		return deny(ReasonCategoryNotAllowed, fmt.Sprintf("category %q is not in the repository allow list", diagnosis.Category))
	}
	effectiveConfidence := p.AutoFix.MinimumConfidence
	if floorConfidence > effectiveConfidence {
		effectiveConfidence = floorConfidence
	}
	if diagnosis.Confidence < effectiveConfidence {
		return deny(ReasonBelowConfidence, fmt.Sprintf("confidence %.2f is below the effective minimum %.2f", diagnosis.Confidence, effectiveConfidence))
	}
	claimed := make([]string, 0, len(diagnosis.Evidence))
	for _, evidence := range diagnosis.Evidence {
		if strings.TrimSpace(evidence.File) != "" {
			claimed = append(claimed, evidence.File)
		}
	}
	if p.AutoFix.RequireEvidenceFile && len(claimed) == 0 {
		return deny(ReasonNoEvidenceFile, "policy requires an evidence file and the diagnosis names none")
	}
	files := make([]string, 0, len(claimed))
	seen := make(map[string]bool, len(claimed))
	for _, candidate := range claimed {
		normalized, err := normalizeEvidencePath(candidate)
		if err != nil {
			return deny(ReasonInvalidPath, fmt.Sprintf("evidence path %q is unusable: %v", sanitize(candidate, maximumDetailPathLength), err))
		}
		if !seen[normalized] {
			seen[normalized] = true
			files = append(files, normalized)
		}
	}
	if p.AutoFix.MaxEvidenceFiles > 0 && len(files) > p.AutoFix.MaxEvidenceFiles {
		return deny(ReasonTooManyFiles, fmt.Sprintf("diagnosis names %d files and the policy allows %d", len(files), p.AutoFix.MaxEvidenceFiles))
	}
	// Denial is evaluated across every file before any allow match, so a file
	// covered by both lists is always denied.
	for _, file := range files {
		for _, pattern := range p.AutoFix.DeniedPaths {
			if matchPath(pattern, file) {
				return deny(ReasonDeniedPath, fmt.Sprintf("evidence path %q matches denied pattern %q", sanitize(file, maximumDetailPathLength), pattern))
			}
		}
	}
	if len(p.AutoFix.AllowedPaths) > 0 {
		for _, file := range files {
			if !matchAnyPath(p.AutoFix.AllowedPaths, file) {
				return deny(ReasonPathNotAllowed, fmt.Sprintf("evidence path %q matches no allowed pattern", sanitize(file, maximumDetailPathLength)))
			}
		}
	}
	return Decision{Allowed: true, Reason: ReasonAllowed, Detail: fmt.Sprintf("%d evidence path(s) satisfy the repository auto-fix policy", len(files))}
}

func (p Policy) categoryAllowed(category string) bool {
	if len(p.AutoFix.AllowedCategories) == 0 {
		return true
	}
	for _, allowed := range p.AutoFix.AllowedCategories {
		if allowed == category {
			return true
		}
	}
	return false
}

// supportedCategory defers to domain so the policy file can never name a
// category the diagnosis contract does not itself accept.
func supportedCategory(category string) bool {
	probe := domain.Diagnosis{Category: category, Summary: "probe", RecommendedAction: "probe"}
	return probe.Validate() == nil
}

// validatePattern rejects any glob whose meaning would not be obvious from
// reading it. Only * and ** are metacharacters; every other special glob
// character is refused rather than silently reinterpreted.
func validatePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("pattern is empty")
	}
	if strings.TrimSpace(pattern) != pattern {
		return fmt.Errorf("pattern has surrounding whitespace")
	}
	if len(pattern) > maximumPatternLength {
		return fmt.Errorf("pattern is longer than %d bytes", maximumPatternLength)
	}
	if strings.ContainsAny(pattern, `\?[]{}:`) {
		return fmt.Errorf("pattern may only use the * and ** wildcards")
	}
	for _, character := range pattern {
		if !unicode.IsPrint(character) {
			return fmt.Errorf("pattern contains a non-printable character")
		}
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("pattern must be repository relative")
	}
	segments := strings.Split(pattern, "/")
	if len(segments) > maximumPathSegments {
		return fmt.Errorf("pattern has more than %d segments", maximumPathSegments)
	}
	for _, segment := range segments {
		switch {
		case segment == "":
			return fmt.Errorf("pattern has an empty path segment")
		case segment == "." || segment == "..":
			return fmt.Errorf("pattern must not use . or .. segments")
		case strings.Contains(segment, "**") && segment != "**":
			return fmt.Errorf("** must occupy a whole path segment")
		}
	}
	return nil
}

// normalizeEvidencePath turns an untrusted, model-authored file reference into a
// clean repository-relative slash path, or refuses it. Only a leading ./ is
// normalized away; a backslash separator, an absolute path, or a .. segment is
// refused outright so that a path pointing outside the repository is denied
// rather than quietly cleaned into something that looks safe.
func normalizeEvidencePath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("path is empty")
	}
	if len(value) > maximumPathLength {
		return "", fmt.Errorf("path is longer than %d bytes", maximumPathLength)
	}
	if strings.Contains(value, `\`) {
		return "", errors.New("path must use / separators")
	}
	if strings.Contains(value, ":") {
		return "", errors.New("path must not name a drive or stream")
	}
	for _, character := range value {
		if !unicode.IsPrint(character) {
			return "", errors.New("path contains a non-printable character")
		}
	}
	if strings.HasPrefix(value, "/") {
		return "", errors.New("path must be repository relative")
	}
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if value == "" || value == "." {
		return "", errors.New("path is empty")
	}
	segments := strings.Split(value, "/")
	if len(segments) > maximumPathSegments {
		return "", fmt.Errorf("path has more than %d segments", maximumPathSegments)
	}
	for _, segment := range segments {
		switch segment {
		case "":
			return "", errors.New("path has an empty segment")
		case ".":
			return "", errors.New("path has a . segment")
		case "..":
			return "", errors.New("path escapes the repository")
		}
	}
	return strings.Join(segments, "/"), nil
}

func matchAnyPath(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if matchPath(pattern, value) {
			return true
		}
	}
	return false
}

// matchPath reports whether a validated pattern matches a normalized path.
// Matching is case sensitive and slash separated: a ** segment matches zero or
// more whole segments, a * matches zero or more characters inside a single
// segment and never crosses /, and every other segment matches literally.
func matchPath(pattern, value string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(value, "/"))
}

// matchSegments is the linear greedy wildcard match: it remembers the most
// recent ** and backtracks to it by one segment on failure, which avoids the
// exponential blowup a naive recursion suffers on patterns with several **.
func matchSegments(patternSegments, valueSegments []string) bool {
	patternIndex, valueIndex := 0, 0
	starPatternIndex, starValueIndex := -1, -1
	for valueIndex < len(valueSegments) {
		switch {
		case patternIndex < len(patternSegments) && patternSegments[patternIndex] == "**":
			starPatternIndex = patternIndex
			starValueIndex = valueIndex
			patternIndex++
		case patternIndex < len(patternSegments) && matchSegment(patternSegments[patternIndex], valueSegments[valueIndex]):
			patternIndex++
			valueIndex++
		case starPatternIndex >= 0:
			starValueIndex++
			patternIndex = starPatternIndex + 1
			valueIndex = starValueIndex
		default:
			return false
		}
	}
	for patternIndex < len(patternSegments) && patternSegments[patternIndex] == "**" {
		patternIndex++
	}
	return patternIndex == len(patternSegments)
}

// matchSegment applies the same greedy wildcard match inside one path segment,
// where * stands for zero or more characters other than the separator.
func matchSegment(pattern, value string) bool {
	patternRunes, valueRunes := []rune(pattern), []rune(value)
	patternIndex, valueIndex := 0, 0
	starPatternIndex, starValueIndex := -1, -1
	for valueIndex < len(valueRunes) {
		switch {
		case patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*':
			starPatternIndex = patternIndex
			starValueIndex = valueIndex
			patternIndex++
		case patternIndex < len(patternRunes) && patternRunes[patternIndex] == valueRunes[valueIndex]:
			patternIndex++
			valueIndex++
		case starPatternIndex >= 0:
			starValueIndex++
			patternIndex = starPatternIndex + 1
			valueIndex = starValueIndex
		default:
			return false
		}
	}
	for patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(patternRunes)
}

func deny(reason, detail string) Decision {
	return Decision{Allowed: false, Reason: reason, Detail: sanitize(detail, maximumDetailLength)}
}

// sanitize bounds untrusted text and replaces anything unprintable, so a
// dashboard never renders model-authored control characters or unbounded prose.
func sanitize(value string, limit int) string {
	var builder strings.Builder
	count := 0
	for _, character := range value {
		if count >= limit {
			builder.WriteString("...")
			break
		}
		if !unicode.IsPrint(character) {
			character = '?'
		}
		builder.WriteRune(character)
		count++
	}
	return builder.String()
}
