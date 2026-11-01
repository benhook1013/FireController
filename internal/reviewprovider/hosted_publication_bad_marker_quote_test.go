package reviewprovider

import (
	"strings"
	"testing"
)

func TestHostedPublicationMalformedMarkerInHTMLQuotesIsNotEvidence(t *testing.T) {
	const malformed = "<!-- coderabbit-review-publication malformed -->"
	for _, tag := range []string{"blockquote", "pre"} {
		for _, placement := range []string{"quoted-only", "after-valid", "before-valid", "inline-quoted-only", "inline-after-valid", "inline-before-valid"} {
			t.Run(tag+"/"+placement, func(t *testing.T) {
				separator := "\n"
				if strings.HasPrefix(placement, "inline-") {
					separator = ""
				}
				position := strings.TrimPrefix(placement, "inline-")
				quoted := "<" + tag + ">" + separator + malformed + separator + "</" + tag + ">"
				body := quoted
				valid := publicationTestMarker(1, 1)
				switch position {
				case "after-valid":
					body = valid + "\n" + quoted
				case "before-valid":
					body = quoted + "\n" + valid
				}
				marker, found, bad := publicationBatchMarker(body)
				if bad || found != (position != "quoted-only") || found && marker.publication != publicationTestID {
					t.Fatal("quoted malformed marker affected visible evidence", marker, found, bad)
				}
				// Once outside the explicit quote, the same malformed protocol
				// remains conflicting evidence, even after a valid batch marker.
				_, _, bad = publicationBatchMarker(strings.Join([]string{body, malformed}, "\n"))
				if !bad {
					t.Fatal("visible malformed marker was ignored")
				}
			})
		}
	}
}
