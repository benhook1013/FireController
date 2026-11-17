package reviewprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func checkpointFixture() CheckpointPayload {
	return CheckpointPayload{PublicationID: "review_publication_" + strings.Repeat("a", 32), EvidenceID: "review_evidence_" + strings.Repeat("b", 32), RepositoryID: 4294967296, PullNumber: 7, Channel: "cli", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), BaseRef: "main", ParentSHA: strings.Repeat("e", 40), MergeBaseSHA: strings.Repeat("f", 40), PatchSHA256: strings.Repeat("1", 64), CaptureSHA256: strings.Repeat("2", 64), ParentPullNumber: 5, Found: 3, Accepted: 1, Routed: 1}
}
func checkpointActor() Author { return Author{ID: 17, Login: "original-login", Kind: "User"} }
func checkpointComment(t *testing.T, id int64, p CheckpointPayload) IssueComment {
	t.Helper()
	render, err := RenderCheckpoint(p)
	if err != nil {
		t.Fatal(err)
	}
	return IssueComment{ID: id, Body: render.Body, Author: checkpointActor(), CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestCheckpointRenderDeterministicHistoricalCountsAndPrivateDigest(t *testing.T) {
	p := checkpointFixture()
	original := p
	first, err := RenderCheckpoint(p)
	second, secondErr := RenderCheckpoint(p)
	encoded, _ := json.Marshal(p)
	sum := sha256.Sum256(encoded)
	if err != nil || secondErr != nil || first != second || p != original || first.SemanticSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("noncanonical rendering", first, err, secondErr)
	}
	lines := strings.Split(first.Body, "\n")
	if lines[0] != "**CodeRabbit CLI — 3 Found, 1 Accepted, 1 Routed**" || lines[1] != p.HeadSHA || lines[len(lines)-1] != "<!-- firecontroller-checkpoint:v1 publication="+p.PublicationID+" semantic="+first.SemanticSHA256+" -->" {
		t.Fatal("historical summary/marker changed", first.Body)
	}
	for _, private := range []string{p.EvidenceID, p.BaseRef, p.BaseSHA, p.ParentSHA, p.MergeBaseSHA, p.PatchSHA256, p.CaptureSHA256, "4294967296"} {
		if strings.Contains(first.Body, private) {
			t.Fatal("private source identity leaked into body")
		}
	}
	if got, err := MatchCheckpoint(first.Body, p); err != nil || got != CheckpointMatch {
		t.Fatal("canonical roundtrip", got, err)
	}
	for _, mutate := range []func(*CheckpointPayload){
		func(p *CheckpointPayload) { p.EvidenceID = "review_evidence_" + strings.Repeat("9", 32) }, func(p *CheckpointPayload) { p.RepositoryID++ }, func(p *CheckpointPayload) { p.PullNumber++ }, func(p *CheckpointPayload) { p.BaseRef = "other" }, func(p *CheckpointPayload) { p.BaseSHA = strings.Repeat("9", 40) }, func(p *CheckpointPayload) { p.ParentSHA = strings.Repeat("9", 40) }, func(p *CheckpointPayload) { p.MergeBaseSHA = strings.Repeat("9", 40) }, func(p *CheckpointPayload) { p.PatchSHA256 = strings.Repeat("9", 64) }, func(p *CheckpointPayload) { p.CaptureSHA256 = strings.Repeat("9", 64) }, func(p *CheckpointPayload) { p.ParentPullNumber++ },
	} {
		changed := p
		mutate(&changed)
		render, err := RenderCheckpoint(changed)
		if err != nil || render.SemanticSHA256 == first.SemanticSHA256 || !strings.HasPrefix(render.Body, lines[0]+"\n"+lines[1]) {
			t.Fatal("hidden source identity not digest-bound", err)
		}
		if state, err := MatchCheckpoint(first.Body, changed); err != nil || state != CheckpointConflict {
			t.Fatal("same publication adopted different hidden identity", state, err)
		}
	}
}

func TestCheckpointKnownZeroAndUnboundedNonnegativeOptionalFacts(t *testing.T) {
	p := checkpointFixture()
	p.Channel = "hosted"
	p.Found, p.Accepted, p.Routed = 0, 0, 0
	p.ParentPullNumber = 0
	p.ParentSHA = p.BaseSHA
	unknown, err := RenderCheckpoint(p)
	if err != nil || !strings.HasPrefix(unknown.Body, "**CodeRabbit Hosted — 0 Found, 0/0/0 Accepted (Major/Minor/Trivial), 0 Routed**") || strings.Contains(unknown.Body, " files") || strings.Contains(unknown.Body, " · ") {
		t.Fatal(unknown, err)
	}
	zero := int64(0)
	p.Files = &zero
	p.DurationSeconds = &zero
	known, err := RenderCheckpoint(p)
	if err != nil || known.SemanticSHA256 == unknown.SemanticSHA256 || !strings.Contains(known.Body, "0 files · 0s · ") {
		t.Fatal("unknown rewritten as zero", known, err)
	}
	encoded, _ := json.Marshal(p)
	if !strings.Contains(string(encoded), `"files":0`) || !strings.Contains(string(encoded), `"duration_seconds":0`) {
		t.Fatal("known zero omitted")
	}
	max := int64(math.MaxInt64)
	p.Files = &max
	p.DurationSeconds = &max
	if render, err := RenderCheckpoint(p); err != nil || !strings.Contains(render.Body, "2562047788015215h 30m 07s") {
		t.Fatal("invented universal duration ceiling", err)
	}
	p.Files = nil
	p.DurationSeconds = nil
	encoded, _ = json.Marshal(p)
	if strings.Contains(string(encoded), `"files"`) || strings.Contains(string(encoded), `"duration_seconds"`) {
		t.Fatal("unknown facts serialized")
	}
}

func TestCheckpointRejectsMalformedPayloadWithoutSourceLeaks(t *testing.T) {
	for i, mutate := range []func(*CheckpointPayload){
		func(p *CheckpointPayload) { p.PublicationID = "review_publication_" + strings.Repeat("A", 32) }, func(p *CheckpointPayload) { p.PublicationID = "private-publication" }, func(p *CheckpointPayload) { p.EvidenceID = "review_capture_" + strings.Repeat("b", 32) }, func(p *CheckpointPayload) { p.RepositoryID = 0 }, func(p *CheckpointPayload) { p.PullNumber = 0 }, func(p *CheckpointPayload) { p.PullNumber = 2147483648 }, func(p *CheckpointPayload) { p.Channel = "CLI" }, func(p *CheckpointPayload) { p.HeadSHA = strings.Repeat("A", 40) }, func(p *CheckpointPayload) { p.BaseSHA = "" }, func(p *CheckpointPayload) { p.ParentSHA = "invalid" }, func(p *CheckpointPayload) { p.MergeBaseSHA = "" }, func(p *CheckpointPayload) { p.PatchSHA256 = strings.Repeat("A", 64) }, func(p *CheckpointPayload) { p.CaptureSHA256 = "invalid" }, func(p *CheckpointPayload) { p.ParentPullNumber = -1 }, func(p *CheckpointPayload) { p.ParentPullNumber = 2147483648 }, func(p *CheckpointPayload) { p.BaseRef = "   " }, func(p *CheckpointPayload) { p.BaseRef = strings.Repeat("x", 257) }, func(p *CheckpointPayload) { p.BaseRef = "private\x1bref" }, func(p *CheckpointPayload) { p.BaseRef = string([]byte{255}) }, func(p *CheckpointPayload) { p.Found = 101 }, func(p *CheckpointPayload) { p.Found = -1 }, func(p *CheckpointPayload) { p.Accepted = -1 }, func(p *CheckpointPayload) { p.Routed = -1 }, func(p *CheckpointPayload) { p.Accepted = 3; p.Routed = 1 }, func(p *CheckpointPayload) { v := int64(-1); p.Files = &v }, func(p *CheckpointPayload) { v := int64(-1); p.DurationSeconds = &v },
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p := checkpointFixture()
			mutate(&p)
			render, err := RenderCheckpoint(p)
			if err == nil || render != (CheckpointRender{}) || strings.Contains(fmt.Sprint(err), "private") || strings.Contains(fmt.Sprint(err), p.PublicationID) {
				t.Fatal("malformed payload accepted/leaked", err)
			}
			if _, err := MatchCheckpoint("unrelated", p); err == nil {
				t.Fatal("matcher bypassed payload validation")
			}
			if _, _, err := FindCheckpoint(p, checkpointActor(), nil); err == nil {
				t.Fatal("finder bypassed payload validation")
			}
		})
	}
}

func TestCheckpointMatcherOnlyNeutralWhitespaceAndExactOwnedSemantic(t *testing.T) {
	p := checkpointFixture()
	render, _ := RenderCheckpoint(p)
	for _, body := range []string{render.Body, " \n" + render.Body + "\t", strings.Join(strings.Fields(render.Body), "\u2003\u00a0\r\n\t"), strings.ReplaceAll(render.Body, "\n", "\r\n")} {
		if state, err := MatchCheckpoint(body, p); err != nil || state != CheckpointMatch {
			t.Fatal("neutral whitespace changed semantics", state, err)
		}
	}
	for _, body := range []string{
		strings.Replace(render.Body, "3 Found", "2 Found", 1), strings.Replace(render.Body, "1 Accepted", "0 Accepted", 1), strings.Replace(render.Body, "1 Routed", "0 Routed", 1), strings.Replace(render.Body, p.HeadSHA, strings.Repeat("9", 40), 1), strings.Replace(render.Body, render.SemanticSHA256, strings.Repeat("9", 64), 1), strings.Replace(render.Body, "CodeRabbit", "OtherProvider", 1), strings.Replace(render.Body, "checkpoint:v1", "checkpoint:v2", 1), strings.Replace(render.Body, "checkpoint:v1", "checkpoint", 1), "<!-- example " + render.Body + " -->", strings.Replace(render.Body, p.PublicationID+" semantic=", p.PublicationID+"semantic=", 1), render.Body + "\n" + render.Body, "```\n" + render.Body + "\n```", "> " + render.Body, "<pre>" + render.Body + "</pre>", strings.Replace(render.Body, " -->", "", 1), strings.Replace(render.Body, "**CodeRabbit CLI", "CodeRabbit CLI", 1), strings.Replace(render.Body, "3 Found", "03 Found", 1), strings.Replace(render.Body, "Routed**", "Routed", 1),
	} {
		if state, err := MatchCheckpoint(body, p); err != nil || state != CheckpointConflict {
			t.Fatal("edited semantic/markup adopted or lost owned identity", state, err)
		}
	}
	for _, body := range []string{"", "No checkpoint", "Visible publication=" + p.PublicationID, strings.ReplaceAll(render.Body, p.PublicationID, "review_publication_"+strings.Repeat("9", 32)), strings.Split(render.Body, "\n\n")[0]} {
		if state, err := MatchCheckpoint(body, p); err != nil || state != CheckpointUnrelated {
			t.Fatal("unrelated publication claimed", state, err)
		}
	}
	for _, body := range []string{string([]byte{255}), render.Body + "\x00", render.Body + "\x1b", strings.Repeat("x", githubMaxText+1)} {
		if _, err := MatchCheckpoint(body, p); err == nil || strings.Contains(fmt.Sprint(err), p.PublicationID) {
			t.Fatal("body bounds ignored/leaked", err)
		}
	}
}

func TestFindCheckpointStableAuthorCopiesAndNeutralEdits(t *testing.T) {
	p := checkpointFixture()
	match := checkpointComment(t, 31, p)
	match.Author.Login = "renamed-login"
	match.Body = strings.Join(strings.Fields(match.Body), "\u2003\n")
	match.UpdatedAt = match.CreatedAt.Add(time.Hour)
	foreign := checkpointComment(t, 32, p)
	foreign.Author.ID = 99
	foreign.Body = strings.Replace(foreign.Body, "3 Found", "99 Found", 1)
	sameLogin := checkpointComment(t, 33, p)
	sameLogin.Author.ID = 98
	sameLogin.Author.Login = checkpointActor().Login
	wrongKind := checkpointComment(t, 34, p)
	wrongKind.Author.Kind = "Bot"
	unrelated := checkpointComment(t, 35, p)
	unrelated.Body = "No checkpoint on this unrelated comment."
	comments := []IssueComment{foreign, sameLogin, wrongKind, unrelated, match}
	before := append([]IssueComment(nil), comments...)
	got, found, err := FindCheckpoint(p, checkpointActor(), comments)
	if err != nil || !found || got != match || !reflect.DeepEqual(comments, before) {
		t.Fatal("authored neutral edit not adopted or caller mutated", got, found, err)
	}
	got.Body = "local copy"
	if comments[4].Body == got.Body {
		t.Fatal("result aliases caller-owned comment")
	}
	if _, found, err := FindCheckpoint(p, checkpointActor(), comments[:4]); err != nil || found {
		t.Fatal("foreign author copy attributed", found, err)
	}
	future := match
	future.CreatedAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	future.UpdatedAt = future.CreatedAt
	if _, found, err := FindCheckpoint(p, checkpointActor(), []IssueComment{future}); err != nil || !found {
		t.Fatal("invented posting time window", found, err)
	}
	if got, found, err := FindCheckpoint(p, checkpointActor(), nil); err != nil || found || got != (IssueComment{}) {
		t.Fatal("zero matches became proof", got, found, err)
	}
}

func TestFindCheckpointIgnoresProvenForeignCommentContent(t *testing.T) {
	p := checkpointFixture()
	own := checkpointComment(t, 31, p)
	foreign := IssueComment{ID: 32, Author: Author{ID: 99, Kind: "User", Login: "bad login"}, Body: "unrelated\x00\x1b"}
	for _, comments := range [][]IssueComment{{foreign, own}, {own, foreign}} {
		got, found, err := FindCheckpoint(p, checkpointActor(), comments)
		if err != nil || !found || got != own {
			t.Fatal("proven foreign content blocked original checkpoint", found, err)
		}
	}
	for _, mutate := range []func(*IssueComment){
		func(c *IssueComment) { c.ID = own.ID },
		func(c *IssueComment) { c.Author.ID = 0 },
		func(c *IssueComment) { c.Author.Kind = "" },
		func(c *IssueComment) { c.Author.Kind = "bad kind" },
	} {
		bad := foreign
		mutate(&bad)
		if _, found, err := FindCheckpoint(p, checkpointActor(), []IssueComment{own, bad}); err == nil || found {
			t.Fatal("unproven identity or duplicate ignored", found, err)
		}
	}
	for _, count := range []int{githubMaxItems + 1, githubMaxAggregate/githubMaxText + 1} {
		comments := make([]IssueComment, count)
		body := strings.Repeat("x", githubMaxText)
		for i := range comments {
			comments[i] = foreign
			comments[i].ID = int64(i + 1)
			comments[i].Body = body
		}
		if _, found, err := FindCheckpoint(p, checkpointActor(), comments); err == nil || found {
			t.Fatal("foreign projection bypassed inventory capacity", found, err)
		}
	}
}

func TestFindCheckpointAmbiguityConflictAndMalformedInventory(t *testing.T) {
	p := checkpointFixture()
	a := checkpointComment(t, 31, p)
	b := checkpointComment(t, 32, p)
	if _, found, err := FindCheckpoint(p, checkpointActor(), []IssueComment{a, b}); found || !hasCode(err, "checkpoint_ambiguous") {
		t.Fatal("multiple matches chose one", found, err)
	}
	b.Body = strings.Replace(b.Body, "3 Found", "2 Found", 1)
	for _, comments := range [][]IssueComment{{a, b}, {b, a}} {
		if _, found, err := FindCheckpoint(p, checkpointActor(), comments); found || !hasCode(err, "checkpoint_conflict") || strings.Contains(fmt.Sprint(err), p.PublicationID) {
			t.Fatal("attributable conflict ignored/leaked", found, err)
		}
	}
	for name, mutate := range map[string]func(*IssueComment){"id": func(v *IssueComment) { v.ID = 0 }, "actor": func(v *IssueComment) { v.Author.ID = 0 }, "login": func(v *IssueComment) { v.Author.Login = "bad login" }, "created": func(v *IssueComment) { v.CreatedAt = time.Time{} }, "updated": func(v *IssueComment) { v.UpdatedAt = v.CreatedAt.Add(-time.Second) }, "timezone": func(v *IssueComment) { v.CreatedAt = v.CreatedAt.In(time.FixedZone("other", 3600)) }, "text": func(v *IssueComment) { v.Body = "private-body\x00" }} {
		t.Run(name, func(t *testing.T) {
			bad := a
			mutate(&bad)
			if _, found, err := FindCheckpoint(p, checkpointActor(), []IssueComment{bad}); err == nil || found || strings.Contains(fmt.Sprint(err), "private-body") {
				t.Fatal(found, err)
			}
		})
	}
	if _, found, err := FindCheckpoint(p, checkpointActor(), []IssueComment{a, a}); err == nil || found {
		t.Fatal("duplicate comment ID accepted")
	}
	if _, _, err := FindCheckpoint(p, Author{Login: "private"}, nil); err == nil || strings.Contains(fmt.Sprint(err), "private") {
		t.Fatal("invalid expected actor accepted/leaked")
	}
	for _, kind := range []string{"items", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			count := githubMaxItems + 1
			body := "unrelated"
			if kind == "bytes" {
				count = githubMaxAggregate/githubMaxText + 1
				body = strings.Repeat("x", githubMaxText)
			}
			comments := make([]IssueComment, count)
			for i := range comments {
				comments[i] = a
				comments[i].ID = int64(i + 1)
				comments[i].Body = body
			}
			if _, found, err := FindCheckpoint(p, checkpointActor(), comments); err == nil || found {
				t.Fatal("inventory bound ignored", found, err)
			}
		})
	}
}

func TestCheckpointFrozenSeverityAndHumanDuration(t *testing.T) {
	p := checkpointFixture()
	p.Found, p.Accepted, p.Routed = 6, 5, 1
	p.AcceptedSeverity = &CheckpointAcceptedSeverity{Major: 1, Minor: 1, Trivial: 1, Critical: 1, Unknown: 1}
	r, err := RenderCheckpoint(p)
	if err != nil || !strings.Contains(r.Body, "1/1/1 Accepted (Major/Minor/Trivial) + 1 Critical + 1 Unclassified") || strings.Contains(r.Body, "readiness") {
		t.Fatal(r, err)
	}
	if got, err := MatchCheckpoint(r.Body, p); err != nil || got != CheckpointMatch {
		t.Fatal(got, err)
	}
	for _, bad := range []CheckpointAcceptedSeverity{{Major: -1}, {Major: 6}, {Major: 4}, {Major: math.MaxInt64, Minor: math.MaxInt64}} {
		p.AcceptedSeverity = &bad
		if _, err := RenderCheckpoint(p); err == nil {
			t.Fatal("invalid accepted partition", bad)
		}
	}
	for seconds, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m 00s", 260: "4m 20s", 3599: "59m 59s", 3600: "1h 00m 00s", 3661: "1h 01m 01s"} {
		if got := checkpointDuration(seconds); got != want {
			t.Fatal(seconds, got, want)
		}
	}
}
