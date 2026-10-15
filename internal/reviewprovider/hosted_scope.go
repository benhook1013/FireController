package reviewprovider

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

var hostedComparisonLine = regexp.MustCompile("^Reviewing files that changed from the base of the PR and between ([a-f0-9]{40}|`[a-f0-9]{40}`) and ([a-f0-9]{40}|`[a-f0-9]{40}`)\\.$")
var hostedComparisonScopes = regexp.MustCompile(`(?i)</?details(?:[ \t][^<>]*)?>|<summary(?:[ \t][^<>]*)?>.*?</summary>`)
var hostedComparisonDecoration = regexp.MustCompile(`<[^>]+>`)

func hostedComparisonExample(summary string) bool {
	label := html.UnescapeString(hostedComparisonDecoration.ReplaceAllString(summary, ""))
	label = strings.ToLower(strings.Join(strings.Fields(label), " "))
	label = strings.TrimLeftFunc(label, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, prefix := range []string{"prompt", "example", "proposed wording", "suggested change", "proposed fix", "committable suggestion"} {
		if label == prefix || strings.HasPrefix(label, prefix+" ") || strings.HasPrefix(label, prefix+":") {
			return true
		}
	}
	return false
}

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
		// comments. Details themselves are visible: additionally exclude explicit
		// example/proposal scopes so copied instructions cannot prove scope.
		var excluded []bool
		excludedScopes := 0
		for _, line := range publicationVisibleLines(batch.Body) {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "<summary") && hostedComparisonExample(line) && !strings.Contains(lower, "</summary>") {
				return "", false
			}
			for _, tag := range hostedComparisonScopes.FindAllString(line, -1) {
				switch {
				case strings.HasPrefix(strings.ToLower(tag), "<details"):
					excluded = append(excluded, false)
				case strings.HasPrefix(tag, "</"):
					if len(excluded) == 0 {
						return "", false
					}
					if excluded[len(excluded)-1] {
						excludedScopes--
					}
					excluded = excluded[:len(excluded)-1]
				default:
					if !hostedComparisonExample(tag) {
						continue
					}
					if len(excluded) == 0 {
						return "", false
					}
					if !excluded[len(excluded)-1] {
						excludedScopes++
					}
					excluded[len(excluded)-1] = true
				}
			}
			if excludedScopes == 0 && strings.Contains(lower, "reviewing files") {
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
		if len(excluded) != 0 {
			return "", false
		}
	}
	return baseSHA, baseSHA != ""
}
