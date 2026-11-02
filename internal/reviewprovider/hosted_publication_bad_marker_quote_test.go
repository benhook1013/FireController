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

func TestHostedPublicationHTMLDepthTracksOnlyOutsideCommentText(t *testing.T) {
	const malformed = "<!-- coderabbit-review-publication malformed -->"
	for _, tag := range []string{"blockquote", "pre"} {
		for _, tc := range []struct {
			name   string
			prefix string
			quoted bool
		}{
			{"close-before-comment", "<" + tag + ">\n</" + tag + "><!-- note -->\n", false},
			{"close-after-comment", "<" + tag + ">\n<!-- note --></" + tag + ">\n", false},
			{"close-after-multiline-comment", "<" + tag + ">\n<!-- note\n--></" + tag + ">\n", false},
			{"close-between-comments", "<" + tag + ">\n<!-- note\n--></" + tag + "><!-- another note -->\n", false},
			{"open-before-comment", "<" + tag + "><!-- note -->\n", true},
			{"open-after-comment", "<!-- note --><" + tag + ">\n", true},
			{"open-after-multiline-comment", "<!-- note\n--><" + tag + ">\n", true},
			{"comment-opening-tag", "<!-- <" + tag + "> -->\n", false},
			{"comment-closing-tag", "<" + tag + ">\n<!-- </" + tag + "> -->\n", true},
			{"multiline-comment-opening-tag", "<!--\n<" + tag + ">\n-->\n", false},
			{"multiline-comment-closing-tag", "<" + tag + ">\n<!--\n</" + tag + ">\n-->\n", true},
		} {
			for _, marker := range []string{malformed, publicationTestMarker(1, 1)} {
				t.Run(tag+"/"+tc.name+"/"+map[bool]string{true: "malformed", false: "valid"}[marker == malformed], func(t *testing.T) {
					_, found, bad := publicationBatchMarker(tc.prefix + marker)
					if tc.quoted && (found || bad) || !tc.quoted && marker == malformed && !bad || !tc.quoted && marker != malformed && (!found || bad) {
						t.Fatal("comment boundary changed visible marker attribution", found, bad, tc.quoted)
					}
				})
			}
		}
	}
}

func TestHostedPublicationEarlierValidBatchCannotHideLaterVisibleConflict(t *testing.T) {
	const malformed = "<!-- coderabbit-review-publication malformed -->"
	for _, body := range []string{
		"<blockquote>\nquoted prose\n</blockquote><!-- note -->\n" + malformed,
		"<pre>\nquoted prose\n<!-- note\n--></pre>\n" + malformed,
		"<!-- note --> coderabbit-review-publication malformed",
		"coderabbit-review-publication malformed <!-- note -->",
		"<!-- note\n--> coderabbit-review-publication malformed",
	} {
		_, found, bad := publicationBatchMarker(publicationTestMarker(1, 1) + "\n" + body)
		if !bad || found {
			t.Fatal("earlier valid batch survived visible conflicting metadata", body, found, bad)
		}
	}
}

func TestHostedPublicationFenceLanguageCannotExposeProtocolExamples(t *testing.T) {
	const malformed = "<!-- coderabbit-review-publication malformed -->"
	for _, fence := range []string{"```", "~~~"} {
		body := fence + "coderabbit-review-publication\n" + publicationTestMarker(1, 1) + "\n" + malformed + "\n" + fence
		_, found, bad := publicationBatchMarker(body)
		if found || bad {
			t.Fatal("fence info string exposed example metadata", fence, found, bad)
		}
		_, found, bad = publicationBatchMarker(body + "\n" + publicationTestMarker(1, 1))
		if !found || bad {
			t.Fatal("closing fence lost later visible batch", fence, found, bad)
		}
		_, _, bad = publicationBatchMarker(body + "\n" + malformed)
		if !bad {
			t.Fatal("closing fence hid later malformed marker", fence)
		}
	}
}

func TestHostedPublicationCommentTerminatorsIgnoreCommentInternalQuoteAndIndent(t *testing.T) {
	const malformed = "<!-- coderabbit-review-publication malformed -->"
	valid := publicationTestMarker(1, 1)
	for name, closing := range map[string]string{"plain": "-->", "quoted": "> -->", "indented": "    -->", "tabbed": "\t-->"} {
		for _, following := range []string{valid, malformed} {
			kind := "valid"
			if following == malformed {
				kind = "malformed"
			}
			t.Run(name+"/"+kind, func(t *testing.T) {
				body := "<!-- ordinary provider note\n" + closing + "\n" + following
				batch, found, bad := publicationBatchMarker(body)
				if following == valid && (!found || bad || batch.publication != publicationTestID) || following == malformed && (!bad || found) {
					t.Fatal("comment-internal decoration hid following protocol evidence", found, bad)
				}
				if following == malformed {
					_, found, bad = publicationBatchMarker(valid + "\n" + body)
					if !bad || found {
						t.Fatal("comment terminator hid conflict after earlier valid batch")
					}
				}
			})
		}
		// Quoting/code begins after the ordinary comment ends. Those examples
		// remain suppressed; only the later standalone marker is evidence.
		quoted := "> " + malformed + "\n    " + malformed + "\n```text\n" + malformed + "\n```\n"
		if batch, found, bad := publicationBatchMarker("<!-- note\n" + closing + "\n" + quoted + valid); !found || bad || batch.publication != publicationTestID {
			t.Fatal("comment fix exposed quoted/code protocol examples", name, found, bad)
		}
		if count, valid := publicationRootCount("<!-- note\n" + closing + "\n**Actionable comments posted: 1**"); !valid || count != 1 {
			t.Fatal("comment terminator hid actionable count", name, count, valid)
		}
	}
}

func TestHostedPublicationCommentTerminatorSuffixRemainsVisibleWithoutExposingCommentBody(t *testing.T) {
	const malformed = "coderabbit-review-publication malformed"
	valid := publicationTestMarker(1, 1)
	for _, prefix := range []string{"> ", "    ", "\t"} {
		_, found, bad := publicationBatchMarker("<!-- ordinary note\n" + prefix + "--> " + malformed)
		if found || !bad {
			t.Fatal("same-line visible suffix was hidden by comment-internal decoration", prefix, found, bad)
		}
		// A protocol-shaped substring still inside the ordinary comment cannot
		// become evidence merely because its terminator ends that comment.
		body := "<!-- ordinary note\n" + prefix + valid + "\n" + valid
		if batch, found, bad := publicationBatchMarker(body); !found || bad || batch.publication != publicationTestID {
			t.Fatal("comment body supplied an extra protocol marker", prefix, found, bad)
		}
		// The same opener outside an existing comment stays ordinary quoted or
		// indented content and cannot hide the following standalone marker.
		if batch, found, bad := publicationBatchMarker(prefix + "<!-- note\n" + valid); !found || bad || batch.publication != publicationTestID {
			t.Fatal("quoted/indented opener changed outer comment state", prefix, found, bad)
		}
	}
}
