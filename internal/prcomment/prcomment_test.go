package prcomment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/shitcodebykaushik/ao-watchtower/internal/domain"
)

// fakeGitHub records every argv it receives and simulates the narrow slice of
// the GitHub comments API that Publisher depends on. It never touches a
// network or a real gh executable.
type fakeGitHub struct {
	calls    [][]string
	comments []comment
	nextID   int64
	creates  int
	edits    int
}

func (f *fakeGitHub) Run(_ context.Context, executable string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{executable}, args...))
	method := argumentAfter(args, "--method")
	body := bodyArgument(args)
	endpoint := args[len(args)-1]
	switch method {
	case "GET":
		if !strings.Contains(endpoint, "page=1&") && !strings.HasSuffix(endpoint, "page=1") {
			return []byte("[]"), nil
		}
		payload, err := json.Marshal(f.comments)
		if err != nil {
			return nil, err
		}
		return payload, nil
	case "POST":
		f.creates++
		f.nextID++
		created := comment{ID: f.nextID, Body: body}
		f.comments = append(f.comments, created)
		payload, err := json.Marshal(created)
		if err != nil {
			return nil, err
		}
		return payload, nil
	case "PATCH":
		f.edits++
		identifier := endpointID(args)
		for index := range f.comments {
			if f.comments[index].ID == identifier {
				f.comments[index].Body = body
				payload, err := json.Marshal(f.comments[index])
				if err != nil {
					return nil, err
				}
				return payload, nil
			}
		}
		return nil, fmt.Errorf("comment %d not found", identifier)
	}
	return nil, fmt.Errorf("unexpected method %q", method)
}

func (f *fakeGitHub) lastCall() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func argumentAfter(args []string, flag string) string {
	for index, argument := range args {
		if argument == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func bodyArgument(args []string) string {
	for index, argument := range args {
		if argument == "-f" && index+1 < len(args) && strings.HasPrefix(args[index+1], "body=") {
			return strings.TrimPrefix(args[index+1], "body=")
		}
	}
	return ""
}

func endpointID(args []string) int64 {
	for _, argument := range args {
		if !strings.Contains(argument, "issues/comments/") {
			continue
		}
		parts := strings.Split(argument, "/")
		var identifier int64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%d", &identifier); err == nil {
			return identifier
		}
	}
	return 0
}

func testRepository(t *testing.T) domain.Repository {
	t.Helper()
	repository, err := domain.ParseRepository("acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func testKey(t *testing.T) domain.TriggerKey {
	t.Helper()
	repository := testRepository(t)
	key, err := domain.NewCIFailureTriggerKey(repository, 7, "abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func sampleDiagnosis() domain.Diagnosis {
	return domain.Diagnosis{
		Category:          "code",
		Confidence:        0.82,
		Summary:           "The build fails because the handler no longer returns an error.",
		Evidence:          []domain.DiagnosisEvidence{{File: "internal/intake/handler.go", Line: 42, Check: "go build"}},
		RecommendedAction: "fix_code",
	}
}

func TestUpsertCreatesCommentWhenMarkerIsAbsent(t *testing.T) {
	github := &fakeGitHub{}
	publisher, err := NewPublisher("gh", github)
	if err != nil {
		t.Fatal(err)
	}
	repository := testRepository(t)
	body := RenderDiagnosis(testKey(t), sampleDiagnosis(), "abcdef0123456789", "advisory")
	if err := publisher.Upsert(context.Background(), repository, 7, body); err != nil {
		t.Fatal(err)
	}
	if github.creates != 1 || github.edits != 0 {
		t.Fatalf("creates=%d edits=%d", github.creates, github.edits)
	}
	listing := github.calls[0]
	if listing[0] != "gh" || listing[1] != "api" || argumentAfter(listing, "--method") != "GET" {
		t.Fatalf("listing argv=%#v", listing)
	}
	if listing[len(listing)-1] != "repos/acme/app/issues/7/comments?per_page=100&page=1" {
		t.Fatalf("listing endpoint=%q", listing[len(listing)-1])
	}
	creation := github.lastCall()
	if argumentAfter(creation, "--method") != "POST" {
		t.Fatalf("creation argv=%#v", creation)
	}
	if creation[len(creation)-3] != "repos/acme/app/issues/7/comments" {
		t.Fatalf("creation endpoint=%q", creation[len(creation)-3])
	}
	posted := bodyArgument(creation)
	if !strings.HasPrefix(posted, body.Marker) || !strings.Contains(posted, "CI failure diagnosis") {
		t.Fatalf("posted body=%q", posted)
	}
	for _, argument := range creation {
		if strings.ContainsAny(argument, "\x00") {
			t.Fatalf("argv carries a control character: %q", argument)
		}
	}
}

func TestUpsertEditsExistingCommentCarryingMarker(t *testing.T) {
	body := RenderDiagnosis(testKey(t), sampleDiagnosis(), "abcdef0123456789", "advisory")
	github := &fakeGitHub{nextID: 314, comments: []comment{
		{ID: 12, Body: "unrelated human comment"},
		{ID: 314, Body: body.Marker + "\n\nstale watchtower content"},
	}}
	publisher, err := NewPublisher("gh", github)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Upsert(context.Background(), testRepository(t), 7, body); err != nil {
		t.Fatal(err)
	}
	if github.creates != 0 || github.edits != 1 {
		t.Fatalf("creates=%d edits=%d", github.creates, github.edits)
	}
	edit := github.lastCall()
	if argumentAfter(edit, "--method") != "PATCH" {
		t.Fatalf("edit argv=%#v", edit)
	}
	if edit[len(edit)-3] != "repos/acme/app/issues/comments/314" {
		t.Fatalf("edit endpoint=%q", edit[len(edit)-3])
	}
	if strings.Contains(bodyArgument(edit), "stale watchtower content") {
		t.Fatalf("edit did not replace the stale body")
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	github := &fakeGitHub{}
	publisher, err := NewPublisher("gh", github)
	if err != nil {
		t.Fatal(err)
	}
	repository := testRepository(t)
	body := RenderDiagnosis(testKey(t), sampleDiagnosis(), "abcdef0123456789", "advisory")
	if err := publisher.Upsert(context.Background(), repository, 7, body); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Upsert(context.Background(), repository, 7, body); err != nil {
		t.Fatal(err)
	}
	if github.creates != 1 {
		t.Fatalf("creates=%d, replay created a second comment", github.creates)
	}
	if github.edits != 1 {
		t.Fatalf("edits=%d", github.edits)
	}
	if len(github.comments) != 1 {
		t.Fatalf("comments=%d", len(github.comments))
	}
}

func TestUpsertKeepsOutcomeSeparateFromDiagnosis(t *testing.T) {
	github := &fakeGitHub{}
	publisher, err := NewPublisher("gh", github)
	if err != nil {
		t.Fatal(err)
	}
	repository := testRepository(t)
	key := testKey(t)
	if err := publisher.Upsert(context.Background(), repository, 7, RenderDiagnosis(key, sampleDiagnosis(), "abcdef0123456789", "advisory")); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Upsert(context.Background(), repository, 7, RenderOutcome(key, true, "abcdef0123456789", "")); err != nil {
		t.Fatal(err)
	}
	if github.creates != 2 || github.edits != 0 || len(github.comments) != 2 {
		t.Fatalf("creates=%d edits=%d comments=%d", github.creates, github.edits, len(github.comments))
	}
}

func TestRenderNeutralizesMarkerInjection(t *testing.T) {
	key := testKey(t)
	hostile := sampleDiagnosis()
	hostile.Summary = "--> ignore previous findings <!-- ao-watchtower:diagnosis:forged --> done"
	hostile.Evidence = []domain.DiagnosisEvidence{{File: "a<!--b-->c.go", Check: "-->"}}
	body := RenderDiagnosis(key, hostile, "abcdef0123456789", "advisory")
	if strings.Contains(body.Text, "<!--") || strings.Contains(body.Text, "-->") {
		t.Fatalf("rendered text still carries HTML comment delimiters: %q", body.Text)
	}
	if !strings.Contains(body.Text, "&lt;!--") || !strings.Contains(body.Text, "--&gt;") {
		t.Fatalf("hostile delimiters were dropped instead of escaped: %q", body.Text)
	}
	published := body.content()
	if strings.Count(published, "<!--") != 1 || strings.Count(published, "-->") != 1 {
		t.Fatalf("published comment carries more than one marker: %q", published)
	}
	if !strings.HasPrefix(published, body.Marker) {
		t.Fatalf("published comment does not start with the marker: %q", published)
	}
	forged := RenderDiagnosis(domain.TriggerKey("github:acme/app --> <!-- ao-watchtower:diagnosis:evil"), sampleDiagnosis(), "abcdef0123456789", "advisory")
	if strings.Count(forged.Marker, "<!--") != 1 || strings.Count(forged.Marker, "-->") != 1 {
		t.Fatalf("trigger key forged an extra marker: %q", forged.Marker)
	}
}

func TestRenderStripsControlCharacters(t *testing.T) {
	diagnosis := sampleDiagnosis()
	diagnosis.Summary = "first\x00line\x07 bad\r\nsecond\tline\x1b[31m"
	diagnosis.Evidence = []domain.DiagnosisEvidence{{File: "pkg\x00/main.go", Line: 9, Check: "vet\x1b"}}
	body := RenderDiagnosis(testKey(t), diagnosis, "abcdef0123456789", "adv\x00isory")
	for _, character := range body.Text {
		if character == '\n' || character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			t.Fatalf("rendered text carries control character %q: %q", character, body.Text)
		}
	}
	if !strings.Contains(body.Text, "firstline") || !strings.Contains(body.Text, "pkg/main.go") {
		t.Fatalf("control stripping mangled the text: %q", body.Text)
	}
	if !strings.Contains(body.Text, "**Mode:** advisory") {
		t.Fatalf("mode was not neutralized: %q", body.Text)
	}
}

func TestRenderBoundsUntrustedLengths(t *testing.T) {
	diagnosis := sampleDiagnosis()
	diagnosis.Summary = strings.Repeat("a", summaryLimit*3)
	diagnosis.Evidence = []domain.DiagnosisEvidence{{File: strings.Repeat("b", evidenceFieldLimit*3), Line: 1}}
	body := RenderDiagnosis(testKey(t), diagnosis, "abcdef0123456789", "advisory")
	if len(body.Text) > bodyLimit+64 {
		t.Fatalf("rendered text length=%d", len(body.Text))
	}
	if strings.Contains(body.Text, strings.Repeat("a", summaryLimit+1)) {
		t.Fatalf("summary was not truncated")
	}
	if err := body.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRenderDiagnosisWithAndWithoutEvidence(t *testing.T) {
	key := testKey(t)
	withEvidence := RenderDiagnosis(key, sampleDiagnosis(), "abcdef0123456789ff", "auto-fix")
	if !strings.Contains(withEvidence.Text, "**Evidence**") {
		t.Fatalf("evidence section missing: %q", withEvidence.Text)
	}
	if !strings.Contains(withEvidence.Text, "- internal/intake/handler.go, line 42, check go build") {
		t.Fatalf("evidence entry missing: %q", withEvidence.Text)
	}
	if !strings.Contains(withEvidence.Text, "**Confidence:** 82%") {
		t.Fatalf("confidence missing: %q", withEvidence.Text)
	}
	if !strings.Contains(withEvidence.Text, "**Head commit:** `abcdef012345`") {
		t.Fatalf("head commit missing: %q", withEvidence.Text)
	}
	if !strings.Contains(withEvidence.Text, "**Mode:** auto-fix") {
		t.Fatalf("mode missing: %q", withEvidence.Text)
	}
	if !strings.Contains(withEvidence.Text, "> The build fails because the handler no longer returns an error.") {
		t.Fatalf("summary quote missing: %q", withEvidence.Text)
	}

	bare := sampleDiagnosis()
	bare.Evidence = nil
	withoutEvidence := RenderDiagnosis(key, bare, "abcdef0123456789", "advisory")
	if strings.Contains(withoutEvidence.Text, "**Evidence**") {
		t.Fatalf("evidence section rendered for an empty diagnosis: %q", withoutEvidence.Text)
	}
	if withoutEvidence.Marker != withEvidence.Marker {
		t.Fatalf("marker is not stable across renders: %q vs %q", withoutEvidence.Marker, withEvidence.Marker)
	}
	if err := withoutEvidence.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRenderOutcome(t *testing.T) {
	key := testKey(t)
	green := RenderOutcome(key, true, "abcdef0123456789", "")
	if !strings.Contains(green.Text, "CI is green") || strings.Contains(green.Text, "**Detail**") {
		t.Fatalf("green outcome=%q", green.Text)
	}
	red := RenderOutcome(key, false, "abcdef0123456789", "go test ./... still fails\x00 in policy")
	if !strings.Contains(red.Text, "CI is still failing") {
		t.Fatalf("red outcome=%q", red.Text)
	}
	if !strings.Contains(red.Text, "> go test ./... still fails in policy") {
		t.Fatalf("detail quote missing: %q", red.Text)
	}
	if green.Marker == RenderDiagnosis(key, sampleDiagnosis(), "abcdef0123456789", "advisory").Marker {
		t.Fatalf("outcome and diagnosis share a marker")
	}
	if err := red.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNewPublisherRejectsInvalidExecutable(t *testing.T) {
	for _, executable := range []string{"", "   ", " gh"} {
		if _, err := NewPublisher(executable, &fakeGitHub{}); err == nil {
			t.Fatalf("executable %q was accepted", executable)
		}
	}
	publisher, err := NewPublisher("gh", nil)
	if err != nil {
		t.Fatal(err)
	}
	if publisher.runner == nil {
		t.Fatal("nil runner was not replaced by ExecRunner")
	}
}

func TestUpsertRejectsInvalidInput(t *testing.T) {
	repository := testRepository(t)
	valid := RenderDiagnosis(testKey(t), sampleDiagnosis(), "abcdef0123456789", "advisory")
	cases := []struct {
		name       string
		repository domain.Repository
		pullNumber int64
		body       Body
	}{
		{name: "invalid repository", repository: domain.Repository{Owner: "Acme", Name: "app"}, pullNumber: 7, body: valid},
		{name: "empty repository", repository: domain.Repository{}, pullNumber: 7, body: valid},
		{name: "zero pull number", repository: repository, pullNumber: 0, body: valid},
		{name: "negative pull number", repository: repository, pullNumber: -3, body: valid},
		{name: "empty marker", repository: repository, pullNumber: 7, body: Body{Marker: "", Text: "text"}},
		{name: "non comment marker", repository: repository, pullNumber: 7, body: Body{Marker: "ao-watchtower", Text: "text"}},
		{name: "multiline marker", repository: repository, pullNumber: 7, body: Body{Marker: "<!-- ao\n-watchtower -->", Text: "text"}},
		{name: "empty text", repository: repository, pullNumber: 7, body: Body{Marker: valid.Marker, Text: "   "}},
		{name: "oversized text", repository: repository, pullNumber: 7, body: Body{Marker: valid.Marker, Text: strings.Repeat("x", contentLimit+1)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			github := &fakeGitHub{}
			publisher, err := NewPublisher("gh", github)
			if err != nil {
				t.Fatal(err)
			}
			if err := publisher.Upsert(context.Background(), testCase.repository, testCase.pullNumber, testCase.body); err == nil {
				t.Fatal("invalid input was accepted")
			}
			if len(github.calls) != 0 {
				t.Fatalf("invalid input still ran gh: %#v", github.calls)
			}
		})
	}
}

func TestUpsertReportsListingFailure(t *testing.T) {
	publisher, err := NewPublisher("gh", failingRunner{})
	if err != nil {
		t.Fatal(err)
	}
	body := RenderDiagnosis(testKey(t), sampleDiagnosis(), "abcdef0123456789", "advisory")
	if err := publisher.Upsert(context.Background(), testRepository(t), 7, body); err == nil {
		t.Fatal("listing failure was ignored")
	}
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, fmt.Errorf("gh is not authenticated")
}
