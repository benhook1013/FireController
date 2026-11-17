package reviewprovider

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HostedProcessing is a provider report seen in a retained snapshot, not an
// exact execution-start time, completion, coverage or permission to dispatch.
type HostedProcessing struct {
	ObservedAt, CommentUpdatedAt time.Time
	SourceCommentID              int64
	ProviderRunID, TriggerID     string
}

const hostedProcessingStart = "<!-- This is an auto-generated comment: review in progress by coderabbit.ai -->"
const hostedProcessingEnd = "<!-- end of auto-generated comment: review in progress by coderabbit.ai -->"
const hostedProcessingNotice = "> Currently processing new changes in this PR. This may take a few minutes, please wait..."

var hostedProcessingHeading = regexp.MustCompile(`^<summary><strong>(?:ℹ️|` + standingPicture + `) Review info</strong></summary>$`)

// processingSection requires one root-level, visible, closed progress block.
// Reuse the existing visibility/tag scanner on its prefix; deliberately do not
// promote a quoted/fenced/hidden or nested copy to provider activity.
func processingSection(body string) ([]string, bool) {
	if !validBody(body) || strings.Count(body, hostedProcessingStart) != 1 || strings.Count(body, hostedProcessingEnd) != 1 {
		return nil, false
	}
	start, end := strings.Index(body, hostedProcessingStart), strings.Index(body, hostedProcessingEnd)
	if end <= start || start > 0 && body[start-1] != '\n' || end > 0 && body[end-1] != '\n' {
		return nil, false
	}
	prefix := hostedComparisonScanner{}
	if !prefix.body(body[:start]) {
		return nil, false
	}
	section := body[start+len(hostedProcessingStart) : end]
	if !strings.HasPrefix(section, "\n") && !strings.HasPrefix(section, "\r\n") {
		return nil, false
	}
	after := body[end+len(hostedProcessingEnd):]
	if after != "" && !strings.HasPrefix(after, "\n") && !strings.HasPrefix(after, "\r\n") {
		return nil, false
	}
	var lines []string
	for _, raw := range strings.Split(section, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if strings.TrimRight(line, " ") == ">" {
			line = ">"
		} else if line != strings.TrimSpace(line) {
			return nil, false
		}
		lines = append(lines, line)
	}
	return lines, true
}

func parseHostedProcessing(body string) (run, head string, ok bool) {
	lines, valid := processingSection(body)
	if !valid {
		return "", "", false
	}
	p := standingRecentParser{lines: lines}
	if !p.take("> [!NOTE]") {
		return "", "", false
	}
	for i := 0; i < 3 && p.take(">"); i++ {
	}
	if !p.take(hostedProcessingNotice, "<details>") || p.at >= len(lines) || !hostedProcessingHeading.MatchString(lines[p.at]) {
		return "", "", false
	}
	p.at++
	if !p.openBody() || !p.block("⚙️ Run configuration") {
		return "", "", false
	}
	prefixes := []string{"- **Configuration used**: ", "- **Review profile**: ", "- **Plan**: "}
	configurationCommit, configurationFile := "", ""
	if p.at < len(lines) && strings.HasPrefix(lines[p.at], "- **Source mode**: ") {
		if !p.take("- **Source mode**: Auto") {
			return "", "", false
		}
		prefixes = []string{"- **Repository**: ", "- **Branch**: ", "- **File**: ", "- **Commit**: ", "- **Layers**: ", "- **Review profile**: ", "- **Plan**: "}
	}
	for _, prefix := range prefixes {
		if p.at >= len(lines) || !strings.HasPrefix(lines[p.at], prefix) {
			return "", "", false
		}
		value := strings.TrimPrefix(lines[p.at], prefix)
		if !validPlainText(value, 1000) || strings.TrimSpace(value) == "" {
			return "", "", false
		}
		switch prefix {
		case "- **Commit**: ":
			if configurationCommit, valid = standingConfigurationHead(value, "commit"); !valid {
				return "", "", false
			}
		case "- **File**: ":
			if configurationFile, valid = standingConfigurationHead(value, "blob"); !valid {
				return "", "", false
			}
		}
		p.at++
	}
	if p.at >= len(lines) {
		return "", "", false
	}
	identity := standingRunID.FindStringSubmatch(lines[p.at])
	if identity == nil {
		return "", "", false
	}
	run = identity[1]
	p.at++
	if !p.close() || !p.block("📥 Commits") || p.at >= len(lines) {
		return "", "", false
	}
	comparison := hostedComparisonLine.FindStringSubmatch(lines[p.at])
	if comparison == nil {
		return "", "", false
	}
	head = strings.Trim(comparison[2], "`")
	if configurationCommit != "" && configurationCommit != head || configurationFile != "" && configurationFile != head {
		return "", "", false
	}
	p.at++
	if !p.close() {
		return "", "", false
	}
	ignored, selected := map[string]bool{}, map[string]bool{}
	if p.at+1 >= len(lines) || lines[p.at] != "<details>" || !standingSelected.MatchString(lines[p.at+1]) {
		if _, valid = p.files(standingIgnored, true, ignored); !valid {
			return "", "", false
		}
	}
	if _, valid = p.files(standingSelected, false, selected); !valid {
		return "", "", false
	}
	for path := range selected {
		if ignored[path] {
			return "", "", false
		}
	}
	if p.at+1 < len(lines) && lines[p.at] == "<details>" && standingNoReviewable.MatchString(lines[p.at+1]) && !p.noReviewableFiles(selected) {
		return "", "", false
	}
	return run, head, p.close() && p.at == len(lines)
}

// ExtractHostedProcessing consumes already-retained observations. The caller
// must bind baseline to this attempt's original complete pre-POST capture.
// Numeric bot identity, original trigger, new provider Run ID, current head and
// open command window all have to agree. No external read or mutation occurs.
func ExtractHostedProcessing(snapshot, baseline HostedPublicationSnapshot, intent HostedTriggerIntent, postingActor, providerActor Author, earliestPosting time.Time) (*HostedProcessing, error) {
	if !publicationValidActor(providerActor) || providerActor.Kind != "Bot" || publicationActor(providerActor, postingActor) {
		return nil, invalidSelector()
	}
	trigger, err := RecoverHostedTrigger(snapshot.IssueComments, snapshot.Complete, intent, postingActor, earliestPosting, snapshot.ObservedAt)
	if err != nil {
		return nil, err
	}
	// GitHub command timestamps have second precision. The runtime separately
	// binds this baseline to the exact local pre-POST boundary; a baseline read
	// earlier within the command's second is not evidence of a late baseline.
	if trigger.State != "adopted" || !snapshot.Complete || !baseline.Complete || snapshot.IssueComments == nil || baseline.IssueComments == nil || snapshot.Reviews == nil || baseline.Reviews == nil || snapshot.ReviewComments == nil || baseline.ReviewComments == nil || !hostedTriggerTime(baseline.ObservedAt) || !validPublicationSnapshot(snapshot) || !validPublicationSnapshot(baseline) || baseline.ObservedAt.Truncate(time.Second).After(trigger.CreatedAt) {
		return nil, nil
	}
	closing := publicationClosingTime(snapshot, trigger, postingActor, providerActor)
	if !closing.IsZero() && !snapshot.ObservedAt.Before(closing) {
		return nil, nil
	}
	var result *HostedProcessing
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, providerActor) || !comment.UpdatedAt.After(trigger.CreatedAt) {
			continue
		}
		if comment.CreatedAt.After(trigger.CreatedAt) && standingFinishedAck(comment.Body) {
			return nil, nil
		}
		run, head, valid := parseHostedProcessing(comment.Body)
		if !valid || head != intent.HeadSHA {
			continue
		}
		if result != nil {
			return nil, nil
		}
		prior := false
		for _, old := range baseline.IssueComments {
			if !publicationActor(old.Author, providerActor) {
				continue
			}
			if strings.Contains(strings.ToLower(old.Body), run) {
				return nil, nil
			}
			if old.ID == comment.ID {
				if old.CreatedAt != comment.CreatedAt || !old.UpdatedAt.Before(trigger.CreatedAt) {
					return nil, nil
				}
				prior = true
			}
		}
		if !prior && !comment.CreatedAt.After(trigger.CreatedAt) {
			return nil, nil
		}
		for _, old := range baseline.Reviews {
			if publicationActor(old.Author, providerActor) && strings.Contains(strings.ToLower(old.Body), run) {
				return nil, nil
			}
		}
		for _, old := range baseline.ReviewComments {
			if publicationActor(old.Author, providerActor) && strings.Contains(strings.ToLower(old.Body), run) {
				return nil, nil
			}
		}
		result = &HostedProcessing{ObservedAt: snapshot.ObservedAt, CommentUpdatedAt: comment.UpdatedAt, SourceCommentID: comment.ID, ProviderRunID: run, TriggerID: strconv.FormatInt(trigger.CommentID, 10)}
	}
	return result, nil
}
