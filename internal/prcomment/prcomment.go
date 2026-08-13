// Package prcomment publishes Watchtower's findings onto the GitHub pull
// request itself through the user's already authenticated gh CLI, so the
// diagnosis is visible where review happens instead of only on the local
// dashboard.
//
// Every external call is made with discrete argv arguments, never through a
// shell command string, and the published Markdown is rendered by pure
// functions that neutralize untrusted investigator prose and CI log text.
//
// The exact gh invocations used by Upsert are, with OWNER/NAME the validated
// repository and PULL the pull request number:
//
//	gh api --method GET  repos/OWNER/NAME/issues/PULL/comments?per_page=100&page=N
//	gh api --method POST repos/OWNER/NAME/issues/PULL/comments -f body=<content>
//	gh api --method PATCH repos/OWNER/NAME/issues/comments/COMMENT_ID -f body=<content>
//
// The listing call is paginated explicitly with per_page and page arguments
// rather than gh's --paginate flag, because explicit paging yields one plain
// JSON array per call across gh versions. Upsert edits the first comment whose
// body carries its own hidden marker and only creates a comment when no such
// comment exists, so replaying it never produces a second comment. A marker
// found on a comment authored by somebody else is still treated as Watchtower's
// own: the worst case is that the forged comment is overwritten with genuine
// Watchtower content, or that GitHub rejects the edit; neither case can create
// a duplicate.
package prcomment

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

const (
	outputLimit        = 4 << 20
	stderrLimit        = 2048
	summaryLimit       = 4096
	evidenceFieldLimit = 1024
	evidenceLimit      = 32
	actionLimit        = 128
	categoryLimit      = 64
	modeLimit          = 64
	detailLimit        = 4096
	markerKeyLimit     = 512
	markerLimit        = 1024
	bodyLimit          = 16384
	contentLimit       = 65536
	commentPageSize    = 100
	commentPageLimit   = 10
)

// Runner executes one external command from discrete arguments. It mirrors
// polling.Runner so tests can inject a fake process runner and stay offline.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// ExecRunner runs commands through os/exec with bounded stdout and truncated
// stderr, so a hostile or broken gh invocation cannot exhaust memory or leak an
// unbounded error body into logs.
type ExecRunner struct{}

// Run executes the named executable with the given arguments and returns its
// standard output. The bound is applied while the process writes rather than
// afterwards, so an unbounded response never reaches memory in the first place.
func (ExecRunner) Run(ctx context.Context, executable string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, executable, args...)
	stdout := &boundedBuffer{limit: outputLimit}
	stderr := &boundedBuffer{limit: stderrLimit}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("run %s: %s", executable, strings.TrimSpace(string(stderr.data)))
	}
	if stdout.truncated {
		return nil, fmt.Errorf("run %s: output exceeds limit", executable)
	}
	return stdout.data, nil
}

// boundedBuffer accumulates at most limit bytes and records that it dropped the
// rest. Writes always report full acceptance so the child process is never
// blocked or killed by a short write.
type boundedBuffer struct {
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if remaining <= 0 {
		b.truncated = true
		return len(data), nil
	}
	if len(data) > remaining {
		b.data = append(b.data, data[:remaining]...)
		b.truncated = true
		return len(data), nil
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

// Publisher writes Watchtower comments onto pull requests through the gh CLI.
type Publisher struct {
	executable string
	runner     Runner
}

// NewPublisher validates the gh executable path and returns a Publisher. A nil
// runner falls back to ExecRunner.
func NewPublisher(ghExecutable string, runner Runner) (*Publisher, error) {
	if strings.TrimSpace(ghExecutable) == "" || strings.TrimSpace(ghExecutable) != ghExecutable {
		return nil, fmt.Errorf("GitHub CLI executable is required")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Publisher{executable: ghExecutable, runner: runner}, nil
}

// Body is the rendered comment content plus the hidden HTML marker that makes
// republishing idempotent. Text never contains the marker; Upsert prepends it
// when composing the published comment.
type Body struct {
	Marker string
	Text   string
}

// Validate reports whether the body carries a usable marker and visible text.
func (b Body) Validate() error {
	if b.Marker == "" || !strings.HasPrefix(b.Marker, "<!--") || !strings.HasSuffix(b.Marker, "-->") {
		return fmt.Errorf("comment marker must be an HTML comment")
	}
	if strings.ContainsAny(b.Marker, "\n\r") || len(b.Marker) > markerLimit {
		return fmt.Errorf("comment marker must be a single bounded line")
	}
	if strings.TrimSpace(b.Text) == "" {
		return fmt.Errorf("comment text is required")
	}
	if len(b.Text) > contentLimit {
		return fmt.Errorf("comment text is too long")
	}
	return nil
}

// content renders the published comment: marker first, visible Markdown after.
func (b Body) content() string {
	return b.Marker + "\n\n" + b.Text + "\n"
}

// comment is the narrow slice of a GitHub issue comment Watchtower reads.
type comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// Upsert publishes the body on the pull request, editing Watchtower's existing
// comment in place when one already carries the same marker and creating a new
// comment otherwise. Calling it twice with the same marker never creates a
// second comment.
func (p *Publisher) Upsert(ctx context.Context, repository domain.Repository, pullNumber int64, body Body) error {
	if err := repository.Validate(); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	if pullNumber <= 0 {
		return fmt.Errorf("pull number must be positive")
	}
	if err := body.Validate(); err != nil {
		return err
	}
	existingID, err := p.findMarkedComment(ctx, repository, pullNumber, body.Marker)
	if err != nil {
		return err
	}
	content := body.content()
	if existingID > 0 {
		endpoint := fmt.Sprintf("repos/%s/issues/comments/%d", repository, existingID)
		if _, err := p.runner.Run(ctx, p.executable, "api", "--method", "PATCH", endpoint, "-f", "body="+content); err != nil {
			return fmt.Errorf("edit pull request comment %d: %w", existingID, err)
		}
		return nil
	}
	endpoint := fmt.Sprintf("repos/%s/issues/%d/comments", repository, pullNumber)
	if _, err := p.runner.Run(ctx, p.executable, "api", "--method", "POST", endpoint, "-f", "body="+content); err != nil {
		return fmt.Errorf("create pull request comment on %s#%d: %w", repository, pullNumber, err)
	}
	return nil
}

// findMarkedComment returns the identifier of the first comment carrying the
// marker, or zero when the pull request has none.
func (p *Publisher) findMarkedComment(ctx context.Context, repository domain.Repository, pullNumber int64, marker string) (int64, error) {
	for page := 1; page <= commentPageLimit; page++ {
		endpoint := fmt.Sprintf("repos/%s/issues/%d/comments?per_page=%d&page=%d", repository, pullNumber, commentPageSize, page)
		payload, err := p.runner.Run(ctx, p.executable, "api", "--method", "GET", endpoint)
		if err != nil {
			return 0, fmt.Errorf("list pull request comments on %s#%d: %w", repository, pullNumber, err)
		}
		var comments []comment
		if err := json.Unmarshal(payload, &comments); err != nil {
			return 0, fmt.Errorf("decode pull request comments on %s#%d: %w", repository, pullNumber, err)
		}
		for _, existing := range comments {
			if existing.ID > 0 && strings.Contains(existing.Body, marker) {
				return existing.ID, nil
			}
		}
		if len(comments) < commentPageSize {
			return 0, nil
		}
	}
	return 0, nil
}

// RenderDiagnosis renders the validated diagnosis as a Markdown comment body.
// Investigator prose is untrusted, so the summary and every evidence field are
// stripped of control characters, bounded, and prevented from forging or
// terminating Watchtower's hidden marker.
func RenderDiagnosis(key domain.TriggerKey, diagnosis domain.Diagnosis, headSHA string, mode string) Body {
	var builder strings.Builder
	builder.WriteString("### AO Watchtower - CI failure diagnosis\n\n")
	builder.WriteString(fmt.Sprintf("- **Category:** %s\n", fallback(neutralizeInline(diagnosis.Category, categoryLimit), "unknown")))
	builder.WriteString(fmt.Sprintf("- **Confidence:** %s\n", formatConfidence(diagnosis.Confidence)))
	builder.WriteString(fmt.Sprintf("- **Recommended action:** %s\n", fallback(neutralizeInline(diagnosis.RecommendedAction, actionLimit), "none")))
	builder.WriteString(fmt.Sprintf("- **Head commit:** `%s`\n", shortSHA(headSHA)))
	builder.WriteString(fmt.Sprintf("- **Mode:** %s\n", fallback(neutralizeInline(mode, modeLimit), "advisory")))
	builder.WriteString("\n**Summary**\n\n")
	builder.WriteString(blockquote(fallback(neutralize(diagnosis.Summary, summaryLimit), "No summary was reported.")))
	builder.WriteString("\n")
	if lines := renderEvidence(diagnosis.Evidence); lines != "" {
		builder.WriteString("\n**Evidence**\n\n")
		builder.WriteString(lines)
	}
	builder.WriteString("\nInvestigation is advisory. Code changes require a recorded human approval.\n")
	return Body{Marker: markerFor("diagnosis", key), Text: capBody(builder.String())}
}

// RenderOutcome renders the post-fix result. verified reports whether CI went
// green after Watchtower's fix. detail is untrusted CI text and is neutralized.
func RenderOutcome(key domain.TriggerKey, verified bool, headSHA string, detail string) Body {
	var builder strings.Builder
	builder.WriteString("### AO Watchtower - fix verification\n\n")
	if verified {
		builder.WriteString("- **Result:** CI is green after the Watchtower fix.\n")
	} else {
		builder.WriteString("- **Result:** CI is still failing after the Watchtower fix.\n")
	}
	builder.WriteString(fmt.Sprintf("- **Head commit:** `%s`\n", shortSHA(headSHA)))
	if cleaned := neutralize(detail, detailLimit); strings.TrimSpace(cleaned) != "" {
		builder.WriteString("\n**Detail**\n\n")
		builder.WriteString(blockquote(cleaned))
		builder.WriteString("\n")
	}
	return Body{Marker: markerFor("outcome", key), Text: capBody(builder.String())}
}

// renderEvidence renders bounded evidence entries as a Markdown list and
// returns an empty string when the diagnosis carries no usable observation.
func renderEvidence(evidence []domain.DiagnosisEvidence) string {
	var builder strings.Builder
	rendered := 0
	for _, item := range evidence {
		if rendered >= evidenceLimit {
			break
		}
		file := neutralizeInline(item.File, evidenceFieldLimit)
		check := neutralizeInline(item.Check, evidenceFieldLimit)
		if file == "" && check == "" {
			continue
		}
		parts := make([]string, 0, 3)
		if file != "" {
			parts = append(parts, file)
		}
		if item.Line > 0 {
			parts = append(parts, fmt.Sprintf("line %d", item.Line))
		}
		if check != "" {
			parts = append(parts, "check "+check)
		}
		builder.WriteString("- " + strings.Join(parts, ", ") + "\n")
		rendered++
	}
	return builder.String()
}

// markerFor builds the hidden HTML marker that identifies one Watchtower
// comment kind for one trigger key.
func markerFor(kind string, key domain.TriggerKey) string {
	cleaned := neutralizeInline(string(key), markerKeyLimit)
	if cleaned == "" {
		cleaned = "unknown"
	}
	return "<!-- ao-watchtower:" + kind + ":" + cleaned + " -->"
}

// neutralize makes untrusted text safe to embed in a Watchtower comment: ASCII
// control characters other than newline and tab are removed, HTML comment
// delimiters are escaped so the text can neither open nor close a marker, and
// the result is truncated to limit bytes.
func neutralize(value string, limit int) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, character := range strings.ToValidUTF8(value, "") {
		if character == '\n' || character == '\t' {
			builder.WriteRune(character)
			continue
		}
		if character < 0x20 || character == 0x7f {
			continue
		}
		builder.WriteRune(character)
	}
	cleaned := strings.ReplaceAll(builder.String(), "<!--", "&lt;!--")
	cleaned = strings.ReplaceAll(cleaned, "-->", "--&gt;")
	return truncate(strings.TrimSpace(cleaned), limit)
}

// neutralizeInline is neutralize for values that must stay on a single line.
func neutralizeInline(value string, limit int) string {
	cleaned := neutralize(value, limit)
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = strings.ReplaceAll(cleaned, "\t", " ")
	return strings.TrimSpace(cleaned)
}

// truncate bounds a neutralized string without splitting a UTF-8 sequence.
func truncate(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := strings.ToValidUTF8(value[:limit], "")
	return strings.TrimRight(cut, " \t\n") + " ...(truncated)"
}

// capBody bounds the whole rendered comment so one gh argument stays small
// enough for every supported platform's command line limit.
func capBody(value string) string {
	return truncate(value, bodyLimit)
}

// blockquote renders untrusted prose as a Markdown quote so it cannot be read
// as Watchtower's own structure.
func blockquote(value string) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimRight("> "+line, " \t")
	}
	return strings.Join(lines, "\n")
}

// shortSHA reduces a head commit to a displayable hexadecimal prefix.
func shortSHA(headSHA string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(headSHA)) {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			builder.WriteRune(character)
		}
		if builder.Len() == 12 {
			break
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	return builder.String()
}

// formatConfidence renders a diagnosis confidence as a bounded percentage.
func formatConfidence(confidence float64) string {
	if confidence < 0 || confidence > 1 {
		return "unknown"
	}
	return fmt.Sprintf("%.0f%%", confidence*100)
}

// fallback returns replacement when value is empty.
func fallback(value string, replacement string) string {
	if value == "" {
		return replacement
	}
	return value
}
