package reviewprovider

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

var hostedComparisonLine = regexp.MustCompile("^Reviewing files that changed from the base of the PR and between ([a-f0-9]{40}|`[a-f0-9]{40}`) and ([a-f0-9]{40}|`[a-f0-9]{40}`)\\.$")
var hostedComparisonTokens = regexp.MustCompile(`<!--|-->|<[^>]*>`)
var hostedComparisonTag = regexp.MustCompile(`(?i)^</?([a-z][a-z0-9-]*)(?:[ \t/>]|$)`)
var hostedComparisonDecoration = regexp.MustCompile(`<[^>]+>`)

func hostedComparisonLabel(summary string) string {
	label := html.UnescapeString(hostedComparisonDecoration.ReplaceAllString(summary, ""))
	label = strings.ToLower(strings.Join(strings.Fields(label), " "))
	return strings.TrimLeftFunc(label, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsSymbol(r) || r == 'ℹ' || r == '\ufe0f' || r == '\ufe0e'
	})
}

type hostedComparisonFrame struct {
	tag                string
	metadata, labelled bool
}

// One scanner owns both visibility and HTML scopes: removing blockquote lines
// before matching their details closers would corrupt the surrounding context.
type hostedComparisonScanner struct {
	standing                  bool
	frames                    []hostedComparisonFrame
	blocked                   int
	comment, summary          bool
	label                     strings.Builder
	fence                     byte
	fenceLength               int
	head, parent, merge, base string
}

func (s *hostedComparisonScanner) inPre() bool {
	return len(s.frames) > 0 && s.frames[len(s.frames)-1].tag == "pre"
}

func (s *hostedComparisonScanner) text(text, line string) bool {
	if s.comment || s.inPre() {
		return true
	}
	if s.summary {
		s.label.WriteString(text)
		return true
	}
	if s.blocked != 0 || !strings.Contains(strings.ToLower(text), "reviewing files") {
		return true
	}
	declaration := hostedComparisonLine.FindStringSubmatch(line)
	if declaration == nil {
		return false
	}
	start, end := strings.Trim(declaration[1], "`"), strings.Trim(declaration[2], "`")
	if end != s.head || start != s.parent && start != s.merge || s.base != "" && start != s.base {
		return false
	}
	s.base = start
	return true
}

func hostedComparisonVoid(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	}
	return false
}

func (s *hostedComparisonScanner) token(token string) bool {
	if s.inPre() {
		match := hostedComparisonTag.FindStringSubmatch(token)
		if match == nil || strings.ToLower(match[1]) != "pre" || !strings.HasPrefix(token, "</") {
			return true
		}
	}
	if s.comment {
		if token == "-->" {
			s.comment = false
		}
		return true
	}
	if token == "<!--" {
		s.comment = true
		return true
	}
	if token == "-->" {
		return false
	}
	match := hostedComparisonTag.FindStringSubmatch(token)
	if match == nil {
		return true
	}
	tag := strings.ToLower(match[1])
	closing := strings.HasPrefix(token, "</")
	if s.inPre() && !(tag == "pre" && closing) {
		return true
	}
	if s.summary {
		if tag == "summary" && closing {
			frame := &s.frames[len(s.frames)-1]
			label := hostedComparisonLabel(s.label.String())
			frame.metadata = label == "review info" || label == "commits" || s.standing && label == "recent review info"
			frame.labelled = true
			if frame.metadata {
				s.blocked--
			}
			s.summary = false
			s.label.Reset()
			return true
		}
		if tag == "details" || tag == "summary" || tag == "blockquote" || tag == "pre" {
			return false
		}
		s.label.WriteString(token)
		return true
	}
	if tag == "summary" {
		if closing || len(s.frames) == 0 || s.frames[len(s.frames)-1].tag != "details" || s.frames[len(s.frames)-1].labelled {
			return false
		}
		s.summary = true
		s.label.Reset()
		return true
	}
	if hostedComparisonVoid(tag) {
		return !closing
	}
	if closing {
		if len(s.frames) == 0 || s.frames[len(s.frames)-1].tag != tag {
			return false
		}
		if !s.frames[len(s.frames)-1].metadata {
			s.blocked--
		}
		s.frames = s.frames[:len(s.frames)-1]
		return true
	}
	if strings.HasSuffix(token, "/>") && tag != "details" && tag != "blockquote" && tag != "pre" {
		return true
	}
	frame := hostedComparisonFrame{tag: tag}
	if s.standing && (tag == "dl" || tag == "dd") && len(s.frames) > 0 && s.frames[len(s.frames)-1].metadata {
		frame.metadata = true
	} else {
		s.blocked++
	}
	s.frames = append(s.frames, frame)
	return true
}

func (s *hostedComparisonScanner) body(body string) bool {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if s.fence != 0 {
			n := 0
			for n < len(trimmed) && trimmed[n] == s.fence {
				n++
			}
			if indent <= 3 && n >= s.fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
				s.fence = 0
			}
			continue
		}
		if !s.comment && !s.inPre() && !s.summary {
			if indent > 3 || strings.HasPrefix(trimmed, "\t") || strings.HasPrefix(trimmed, ">") {
				continue
			}
			if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
				n := 0
				for n < len(trimmed) && trimmed[n] == trimmed[0] {
					n++
				}
				if n >= 3 {
					s.fence = trimmed[0]
					s.fenceLength = n
					continue
				}
			}
		}
		offset := 0
		for _, span := range hostedComparisonTokens.FindAllStringIndex(line, -1) {
			if !s.text(line[offset:span[0]], line) || !s.token(line[span[0]:span[1]]) {
				return false
			}
			offset = span[1]
		}
		if !s.text(line[offset:], line) {
			return false
		}
		if s.summary {
			s.label.WriteByte('\n')
		}
	}
	return s.fence == 0 && !s.comment && !s.summary && len(s.frames) == 0
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
	if publication.State != "attributed" {
		return "", false
	}
	if standing := publication.StandingZero; standing != nil {
		if len(publication.Batches) != 0 || len(publication.Roots) != 0 || publication.PublicationID != "" || publication.ProviderAttemptID != "" || !validBody(standing.RecentReviewBody) {
			return "", false
		}
		scanner := hostedComparisonScanner{standing: true, head: headSHA, parent: parentSHA, merge: mergeBaseSHA}
		if !scanner.body(standing.RecentReviewBody) {
			return "", false
		}
		return scanner.base, scanner.base != ""
	}
	if len(publication.Batches) == 0 || len(publication.Batches) > 200 {
		return "", false
	}
	scanner := hostedComparisonScanner{head: headSHA, parent: parentSHA, merge: mergeBaseSHA}
	for _, batch := range publication.Batches {
		if !validBody(batch.Body) {
			return "", false
		}
		if !scanner.body(batch.Body) {
			return "", false
		}
	}
	return scanner.base, scanner.base != ""
}
