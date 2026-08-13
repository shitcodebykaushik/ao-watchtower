package repopolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

const examplePolicy = `{
  "version": 1,
  "autoFix": {
    "minimumConfidence": 0.9,
    "allowedPaths": ["internal/**", "cmd/**"],
    "deniedPaths": [".github/**", "**/*.tf", "internal/billing/**"],
    "allowedCategories": ["code"],
    "requireEvidenceFile": true,
    "maxEvidenceFiles": 5
  }
}`

// diagnosisWithFiles builds the smallest diagnosis domain accepts for the given
// evidence files, so tests exercise repopolicy rather than domain validation.
func diagnosisWithFiles(category string, confidence float64, files ...string) domain.Diagnosis {
	evidence := make([]domain.DiagnosisEvidence, 0, len(files))
	for _, file := range files {
		evidence = append(evidence, domain.DiagnosisEvidence{File: file, Line: 12})
	}
	return domain.Diagnosis{
		Category:          category,
		Confidence:        confidence,
		Summary:           "unit test failed in the billing totals helper",
		Evidence:          evidence,
		RecommendedAction: "fix_code",
	}
}

func mustParse(t *testing.T, raw string) Policy {
	t.Helper()
	policy, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s) returned %v", raw, err)
	}
	return policy
}

func TestDefaultIsPermissiveAndValid(t *testing.T) {
	policy := Default()
	if err := policy.Validate(); err != nil {
		t.Fatalf("Default is not valid: %v", err)
	}
	decision := policy.EvaluateAutoFix(diagnosisWithFiles("code", 0.85, "internal/billing/total.go"), 0.8)
	if !decision.Allowed || decision.Reason != ReasonAllowed {
		t.Fatalf("Default denied a normal code diagnosis: %+v", decision)
	}
}

func TestZeroPolicyDeniesEverything(t *testing.T) {
	decision := Policy{}.EvaluateAutoFix(diagnosisWithFiles("code", 1, "internal/total.go"), 0)
	if decision.Allowed || decision.Reason != ReasonInvalidPolicy {
		t.Fatalf("zero Policy must deny, got %+v", decision)
	}
}

func TestParseAcceptsDocumentedExample(t *testing.T) {
	policy := mustParse(t, examplePolicy)
	if policy.Version != PolicyVersion {
		t.Fatalf("version = %d", policy.Version)
	}
	if policy.AutoFix.MinimumConfidence != 0.9 || !policy.AutoFix.RequireEvidenceFile || policy.AutoFix.MaxEvidenceFiles != 5 {
		t.Fatalf("scalars = %+v", policy.AutoFix)
	}
	if len(policy.AutoFix.AllowedPaths) != 2 || len(policy.AutoFix.DeniedPaths) != 3 || len(policy.AutoFix.AllowedCategories) != 1 {
		t.Fatalf("lists = %+v", policy.AutoFix)
	}
}

func TestParseRejectsUnsafePolicies(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  string
	}{
		{"malformed JSON", `{"version": 1,`},
		{"not an object", `[]`},
		{"empty input", ``},
		{"unknown top level field", `{"version":1,"autoFixx":{}}`},
		{"unknown nested field", `{"version":1,"autoFix":{"allowPaths":["internal/**"]}}`},
		{"missing version", `{"autoFix":{}}`},
		{"unknown version", `{"version":2,"autoFix":{}}`},
		{"trailing JSON value", `{"version":1,"autoFix":{}} {"version":1}`},
		{"negative confidence", `{"version":1,"autoFix":{"minimumConfidence":-0.1}}`},
		{"confidence above one", `{"version":1,"autoFix":{"minimumConfidence":1.5}}`},
		{"negative max files", `{"version":1,"autoFix":{"maxEvidenceFiles":-1}}`},
		{"max files above domain bound", `{"version":1,"autoFix":{"maxEvidenceFiles":33}}`},
		{"unknown category", `{"version":1,"autoFix":{"allowedCategories":["billing"]}}`},
		{"empty pattern", `{"version":1,"autoFix":{"deniedPaths":[""]}}`},
		{"absolute pattern", `{"version":1,"autoFix":{"deniedPaths":["/etc/**"]}}`},
		{"traversing pattern", `{"version":1,"autoFix":{"deniedPaths":["../secrets/**"]}}`},
		{"backslash pattern", `{"version":1,"autoFix":{"deniedPaths":["internal\\billing\\**"]}}`},
		{"partial double star segment", `{"version":1,"autoFix":{"allowedPaths":["internal**/x.go"]}}`},
		{"unsupported wildcard", `{"version":1,"autoFix":{"allowedPaths":["internal/?.go"]}}`},
		{"brace expansion", `{"version":1,"autoFix":{"allowedPaths":["internal/{a,b}.go"]}}`},
		{"empty segment", `{"version":1,"autoFix":{"allowedPaths":["internal//x.go"]}}`},
		{"padded pattern", `{"version":1,"autoFix":{"allowedPaths":[" internal/**"]}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			policy, err := Parse([]byte(testCase.raw))
			if err == nil {
				t.Fatalf("expected rejection, got %+v", policy)
			}
			if !reflect.DeepEqual(policy, Policy{}) {
				t.Fatalf("a rejected policy must not be usable, got %+v", policy)
			}
		})
	}
}

func TestLoadMissingFileReturnsDefault(t *testing.T) {
	policy, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if !reflect.DeepEqual(policy, Default()) {
		t.Fatalf("policy = %+v, want Default", policy)
	}
}

func TestLoadReadsCommittedPolicy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PolicyFileName), []byte(examplePolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := Load(root)
	if err != nil {
		t.Fatalf("Load returned %v", err)
	}
	if policy.AutoFix.MinimumConfidence != 0.9 || policy.AutoFix.MaxEvidenceFiles != 5 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestLoadRejectsMalformedFileWithoutFallingBack(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, PolicyFileName), []byte(`{"version":1,"autoFix":{"nope":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := Load(root)
	if err == nil {
		t.Fatal("expected a malformed policy to be an error")
	}
	if !reflect.DeepEqual(policy, Policy{}) {
		t.Fatalf("policy = %+v, want the unusable zero Policy", policy)
	}
	if !strings.Contains(err.Error(), PolicyFileName) {
		t.Fatalf("error should name the policy file: %v", err)
	}
}

func TestLoadRequiresRepositoryRoot(t *testing.T) {
	if _, err := Load("   "); err == nil {
		t.Fatal("expected an empty repository root to be an error")
	}
}

func TestMatchPath(t *testing.T) {
	for _, testCase := range []struct {
		pattern string
		value   string
		want    bool
	}{
		// ** spans zero, one, and many segments.
		{"internal/**", "internal", true},
		{"internal/**", "internal/config.go", true},
		{"internal/**", "internal/billing/ledger/rate.go", true},
		{"**/*.tf", "main.tf", true},
		{"**/*.tf", "deploy/main.tf", true},
		{"**/*.tf", "deploy/aws/prod/main.tf", true},
		{"**", "anything/at/all.go", true},
		{"**/billing/**", "internal/billing/total.go", true},
		{"**/billing/**", "internal/ledger/total.go", false},
		{"internal/**", "cmd/watchtower/main.go", false},
		{"internal/**", "internalx/config.go", false},
		// * stays inside one segment.
		{"internal/*.go", "internal/config.go", true},
		{"internal/*.go", "internal/billing/config.go", false},
		{"*.tf", "main.tf", true},
		{"*.tf", "deploy/main.tf", false},
		{"internal/*", "internal/config.go", true},
		{"internal/*", "internal", false},
		{"internal/*", "internal/billing/total.go", false},
		// * accepts the empty string and repeats within a segment.
		{"*.go", ".go", true},
		{"a*b*c.go", "abc.go", true},
		{"a*b*c.go", "axxbyyc.go", true},
		{"a*b*c.go", "acb.go", false},
		// Literal segments are matched exactly and case sensitively.
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", "docs/.github/ci.yml", false},
		{"internal/Billing/**", "internal/billing/total.go", false},
		{"internal/billing/**", "internal/billing/total.go", true},
	} {
		t.Run(testCase.pattern+" vs "+testCase.value, func(t *testing.T) {
			if err := validatePattern(testCase.pattern); err != nil {
				t.Fatalf("pattern is not valid: %v", err)
			}
			if got := matchPath(testCase.pattern, testCase.value); got != testCase.want {
				t.Fatalf("matchPath = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestNormalizeEvidencePath(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		raw   string
		want  string
		valid bool
	}{
		{name: "plain", raw: "internal/config.go", want: "internal/config.go", valid: true},
		{name: "padded", raw: "  internal/config.go  ", want: "internal/config.go", valid: true},
		{name: "leading dot slash", raw: "./internal/config.go", want: "internal/config.go", valid: true},
		{name: "repeated leading dot slash", raw: "././internal/config.go", want: "internal/config.go", valid: true},
		{name: "dot dot prefixed name is a real file", raw: "internal/..config.go", want: "internal/..config.go", valid: true},
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "dot", raw: "."},
		{name: "traversal", raw: "../etc/passwd"},
		{name: "interior traversal", raw: "internal/../../etc/passwd"},
		{name: "trailing traversal", raw: "internal/.."},
		{name: "absolute posix", raw: "/etc/passwd"},
		{name: "absolute windows", raw: "C:/Windows/System32/config"},
		{name: "backslash", raw: `internal\billing\total.go`},
		{name: "backslash traversal", raw: `..\..\etc\passwd`},
		{name: "interior dot segment", raw: "internal/./config.go"},
		{name: "trailing separator", raw: "internal/billing/"},
		{name: "double separator", raw: "internal//config.go"},
		{name: "control character", raw: "internal/con\nfig.go"},
		{name: "too long", raw: strings.Repeat("a", maximumPathLength+1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := normalizeEvidencePath(testCase.raw)
			if testCase.valid {
				if err != nil || got != testCase.want {
					t.Fatalf("normalizeEvidencePath = %q, %v; want %q, nil", got, err, testCase.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rejection, got %q", got)
			}
		})
	}
}

func TestEvaluateAutoFix(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		policy     string
		diagnosis  domain.Diagnosis
		floor      float64
		wantAllow  bool
		wantReason string
	}{
		{
			name:      "example policy allows an internal code fix",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.95, "internal/ledger/total.go", "cmd/watchtower/main.go"),
			floor:     0.8, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:      "denied path wins over allowed path",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.95, "internal/billing/invoice.go"),
			floor:     0.8, wantReason: ReasonDeniedPath,
		},
		{
			name:      "one denied file poisons an otherwise allowed set",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.95, "internal/ledger/total.go", "internal/billing/invoice.go"),
			floor:     0.8, wantReason: ReasonDeniedPath,
		},
		{
			name:      "deny pattern is checked before the allow list rejects a later file",
			policy:    `{"version":1,"autoFix":{"allowedPaths":["internal/**"],"deniedPaths":["internal/billing/**"]}}`,
			diagnosis: diagnosisWithFiles("code", 0.95, "docs/readme.md", "internal/billing/invoice.go"),
			floor:     0, wantReason: ReasonDeniedPath,
		},
		{
			name:      "path outside the allow list is refused",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.95, "docs/architecture.md"),
			floor:     0.8, wantReason: ReasonPathNotAllowed,
		},
		{
			name:      "terraform anywhere is denied",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.99, "internal/deploy/main.tf"),
			floor:     0.8, wantReason: ReasonDeniedPath,
		},
		{
			name:      "empty allow list permits any undenied path",
			policy:    `{"version":1,"autoFix":{"deniedPaths":[".github/**"]}}`,
			diagnosis: diagnosisWithFiles("code", 0.5, "anywhere/at/all.go"),
			floor:     0, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:      "policy confidence is stricter than the operator flag",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.85, "internal/ledger/total.go"),
			floor:     0.8, wantReason: ReasonBelowConfidence,
		},
		{
			name:      "operator flag is stricter than the policy",
			policy:    `{"version":1,"autoFix":{"minimumConfidence":0.5}}`,
			diagnosis: diagnosisWithFiles("code", 0.85, "internal/ledger/total.go"),
			floor:     0.95, wantReason: ReasonBelowConfidence,
		},
		{
			name:      "a diagnosis meeting both floors is allowed",
			policy:    `{"version":1,"autoFix":{"minimumConfidence":0.9}}`,
			diagnosis: diagnosisWithFiles("code", 0.9, "internal/ledger/total.go"),
			floor:     0.8, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:      "category outside the allow list is refused",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("configuration", 0.99, "internal/ledger/total.go"),
			floor:     0.8, wantReason: ReasonCategoryNotAllowed,
		},
		{
			name:      "empty category list allows every category",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: diagnosisWithFiles("infrastructure", 0.99, "internal/ledger/total.go"),
			floor:     0, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:   "requireEvidenceFile refuses a check only diagnosis",
			policy: examplePolicy,
			diagnosis: domain.Diagnosis{
				Category: "code", Confidence: 0.99, Summary: "the build broke",
				Evidence:          []domain.DiagnosisEvidence{{Check: "build"}},
				RecommendedAction: "fix_code",
			},
			floor: 0.8, wantReason: ReasonNoEvidenceFile,
		},
		{
			name:   "a check only diagnosis passes when no file is required",
			policy: `{"version":1,"autoFix":{}}`,
			diagnosis: domain.Diagnosis{
				Category: "code", Confidence: 0.99, Summary: "the build broke",
				Evidence:          []domain.DiagnosisEvidence{{Check: "build"}},
				RecommendedAction: "fix_code",
			},
			floor: 0, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:      "maxEvidenceFiles bounds the blast radius",
			policy:    `{"version":1,"autoFix":{"maxEvidenceFiles":2}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, "a.go", "b.go", "c.go"),
			floor:     0, wantReason: ReasonTooManyFiles,
		},
		{
			name:      "repeated files count once against maxEvidenceFiles",
			policy:    `{"version":1,"autoFix":{"maxEvidenceFiles":2}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, "a.go", "./a.go", "b.go"),
			floor:     0, wantAllow: true, wantReason: ReasonAllowed,
		},
		{
			name:      "traversal is denied even under a permissive policy",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, "../../etc/passwd"),
			floor:     0, wantReason: ReasonInvalidPath,
		},
		{
			name:      "an absolute path is denied",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, "/etc/passwd"),
			floor:     0, wantReason: ReasonInvalidPath,
		},
		{
			name:      "a backslash path is denied",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, `internal\billing\total.go`),
			floor:     0, wantReason: ReasonInvalidPath,
		},
		{
			name:      "traversal is not laundered into an allowed path",
			policy:    examplePolicy,
			diagnosis: diagnosisWithFiles("code", 0.99, "internal/../../../etc/passwd"),
			floor:     0.8, wantReason: ReasonInvalidPath,
		},
		{
			name:      "an invalid diagnosis is refused",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: domain.Diagnosis{Category: "code", Confidence: 0.99, Summary: "", RecommendedAction: "fix_code"},
			floor:     0, wantReason: ReasonInvalidDiagnosis,
		},
		{
			name:      "an out of range operator floor is refused",
			policy:    `{"version":1,"autoFix":{}}`,
			diagnosis: diagnosisWithFiles("code", 0.99, "internal/total.go"),
			floor:     1.5, wantReason: ReasonInvalidFloor,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decision := mustParse(t, testCase.policy).EvaluateAutoFix(testCase.diagnosis, testCase.floor)
			if decision.Allowed != testCase.wantAllow || decision.Reason != testCase.wantReason {
				t.Fatalf("decision = %+v, want allowed=%t reason=%s", decision, testCase.wantAllow, testCase.wantReason)
			}
			if decision.Detail == "" || len(decision.Detail) > 4*maximumDetailLength {
				t.Fatalf("detail is not a bounded explanation: %q", decision.Detail)
			}
		})
	}
}

func TestDecisionDetailIsBoundedAndPrintable(t *testing.T) {
	hostile := "internal/" + strings.Repeat("z", 900) + "\x07/../secret.go"
	decision := Default().EvaluateAutoFix(diagnosisWithFiles("code", 0.99, hostile), 0)
	if decision.Allowed || decision.Reason != ReasonInvalidPath {
		t.Fatalf("decision = %+v", decision)
	}
	if len([]rune(decision.Detail)) > maximumDetailLength+len("...") {
		t.Fatalf("detail is unbounded: %d runes", len([]rune(decision.Detail)))
	}
	if strings.ContainsRune(decision.Detail, '\x07') {
		t.Fatalf("detail leaked a control character: %q", decision.Detail)
	}
}
