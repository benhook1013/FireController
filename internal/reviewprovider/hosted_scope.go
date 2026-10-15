package reviewprovider

import (
	"regexp"
	"strings"
)

var hostedComparisonLine = regexp.MustCompile("^Reviewing files that changed from the base of the PR and between ([a-f0-9]{40}|`[a-f0-9]{40}`) and ([a-f0-9]{40}|`[a-f0-9]{40}`)\\.$")
var hostedComparisonScopes = regexp.MustCompile(`(?i)</?details(?:[ \t][^<>]*)?>|<summary(?:[ \t][^<>]*)?>.*?prompt.*?</summary>`)

// ResolveHostedComparison identifies only an explicit full-PR comparison in an
// already resolved publication. It does not establish coverage or admission.
// Unknown, malformed or contradictory declarations never become a guessed base.
func ResolveHostedComparison(publication HostedPublication, headSHA, parentSHA, mergeBaseSHA string) (baseSHA string, proven bool) {
	for _, sha := range []string{headSHA, parentSHA, mergeBaseSHA} {
		if !validSHA(sha) || sha != strings.ToLower(sha) {
			return "", false
		}
	}
	if publication.State != "attributed" || len(publication.Batches) == 0 || len(publication.Batches) > 200 {
		return "", false
	}
	for _, batch := range publication.Batches {
		if !validBody(batch.Body) {
			return "", false
		}
		// Publication visibility excludes quotes, fences, pre and generic HTML
		// comments. Details themselves are visible: additionally exclude prompt
		// scopes so an unfenced copied instruction cannot supply a declaration.
		var prompts []bool
		promptScopes := 0
		for _, line := range publicationVisibleLines(batch.Body) {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "<summary") && strings.Contains(lower, "prompt") && !strings.Contains(lower, "</summary>") {
				return "", false
			}
			for _, tag := range hostedComparisonScopes.FindAllString(line, -1) {
				switch {
				case strings.HasPrefix(strings.ToLower(tag), "<details"):
					prompts = append(prompts, false)
				case strings.HasPrefix(tag, "</"):
					if len(prompts) == 0 {
						return "", false
					}
					if prompts[len(prompts)-1] {
						promptScopes--
					}
					prompts = prompts[:len(prompts)-1]
				default:
					if len(prompts) == 0 {
						return "", false
					}
					if !prompts[len(prompts)-1] {
						promptScopes++
					}
					prompts[len(prompts)-1] = true
				}
			}
			if promptScopes == 0 && strings.Contains(lower, "reviewing files") {
				declaration := hostedComparisonLine.FindStringSubmatch(line)
				if declaration == nil {
					return "", false
				}
				start, end := strings.Trim(declaration[1], "`"), strings.Trim(declaration[2], "`")
				if end != headSHA || start != parentSHA && start != mergeBaseSHA || baseSHA != "" && start != baseSHA {
					return "", false
				}
				baseSHA = start
			}
		}
		if len(prompts) != 0 {
			return "", false
		}
	}
	return baseSHA, baseSHA != ""
}
