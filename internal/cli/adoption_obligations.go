package cli

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

type adoptionObligationsInvocation struct {
	operation string
	id        string
	pull      int64
	status    string
	limit     int
	cursor    string
	file      string
	preview   string
	key       string
	document  json.RawMessage
	resolve   client.AdoptionObligationResolveRequest
}

func parseAdoptionObligations(args []string) (*adoptionObligationsInvocation, bool) {
	if len(args) == 0 {
		return nil, false
	}
	in := &adoptionObligationsInvocation{operation: args[0]}
	switch in.operation {
	case "list":
		return parseAdoptionObligationsList(in, args[1:])
	case "read":
		if len(args) != 2 || client.ValidateAdoptionObligationID(args[1]) != nil {
			return nil, false
		}
		in.id = args[1]
		return in, true
	case "adopt":
		return parseAdoptionObligationsAdopt(in, args[1:])
	case "resolve":
		return parseAdoptionObligationsResolve(in, args[1:])
	default:
		return nil, false
	}
}

func parseAdoptionObligationsList(in *adoptionObligationsInvocation, args []string) (*adoptionObligationsInvocation, bool) {
	values, ok := uniqueFlags(args, map[string]bool{"--pr": true, "--status": true, "--limit": true, "--cursor": true})
	if !ok {
		return nil, false
	}
	if raw := values["--pr"]; raw != "" {
		number, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || number < 1 || number > 2147483647 || strconv.FormatInt(number, 10) != raw {
			return nil, false
		}
		in.pull = number
	}
	if raw := values["--status"]; raw != "" {
		if raw != "open" && raw != "resolved" {
			return nil, false
		}
		in.status = raw
	}
	if raw := values["--limit"]; raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			return nil, false
		}
		in.limit = limit
	}
	if raw := values["--cursor"]; raw != "" {
		if len(raw) > 4096 || raw != strings.TrimSpace(raw) || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
			return nil, false
		}
		in.cursor = raw
	}
	return in, true
}

func parseAdoptionObligationsAdopt(in *adoptionObligationsInvocation, args []string) (*adoptionObligationsInvocation, bool) {
	if len(args) == 0 || args[0] != "preview" && args[0] != "apply" {
		return nil, false
	}
	in.operation = "adopt-" + args[0]
	allowed := map[string]bool{"--file": true}
	if in.operation == "adopt-apply" {
		allowed["--preview"], allowed["--key"] = true, true
	}
	values, ok := uniqueFlags(args[1:], allowed)
	if !ok || len(values) != len(allowed) {
		return nil, false
	}
	in.file, in.preview, in.key = values["--file"], values["--preview"], values["--key"]
	if !filepath.IsAbs(in.file) || len(in.file) > 4096 {
		return nil, false
	}
	if in.operation == "adopt-apply" && (!client.ValidMutationKey(in.key) || !lowerHex(in.preview, 64)) {
		return nil, false
	}
	return in, true
}

func parseAdoptionObligationsResolve(in *adoptionObligationsInvocation, args []string) (*adoptionObligationsInvocation, bool) {
	if len(args) < 1 || client.ValidateAdoptionObligationID(args[0]) != nil {
		return nil, false
	}
	in.id = args[0]
	values := map[string]string{}
	evidence := []string{}
	allowed := map[string]bool{"--expected-revision": true, "--reason": true, "--evidence": true, "--key": true}
	for index := 1; index < len(args); index++ {
		flag, value, assigned := strings.Cut(args[index], "=")
		if !allowed[flag] || flag != "--evidence" && values[flag] != "" {
			return nil, false
		}
		if !assigned {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return nil, false
			}
			value = args[index]
		}
		if value == "" {
			return nil, false
		}
		if flag == "--evidence" {
			evidence = append(evidence, value)
		} else {
			values[flag] = value
		}
	}
	if len(values) != 3 || len(evidence) == 0 || !client.ValidMutationKey(values["--key"]) {
		return nil, false
	}
	revision, err := strconv.ParseInt(values["--expected-revision"], 10, 64)
	if err != nil || revision < 1 || revision == math.MaxInt64 || strconv.FormatInt(revision, 10) != values["--expected-revision"] {
		return nil, false
	}
	in.key = values["--key"]
	in.resolve = client.AdoptionObligationResolveRequest{ExpectedRevision: &revision, Reason: values["--reason"], Evidence: evidence}
	normalized, err := client.NormalizeAdoptionObligationResolution(in.resolve)
	if err != nil {
		return nil, false
	}
	in.resolve = normalized
	return in, true
}

func uniqueFlags(args []string, allowed map[string]bool) (map[string]string, bool) {
	values := map[string]string{}
	for index := 0; index < len(args); index++ {
		flag, value, assigned := strings.Cut(args[index], "=")
		if !allowed[flag] || values[flag] != "" {
			return nil, false
		}
		if !assigned {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") {
				return nil, false
			}
			value = args[index]
		}
		if value == "" {
			return nil, false
		}
		values[flag] = value
	}
	return values, true
}

func lowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

func prepareAdoptionObligations(ctx context.Context, in *adoptionObligationsInvocation) error {
	if in.operation != "adopt-preview" && in.operation != "adopt-apply" {
		return nil
	}
	document, err := readOwnerPrivateJSON(ctx, in.file)
	if err != nil {
		return &client.Error{Code: "invalid_import_file", Message: "The selected adoption batch must be one owner-private regular JSON file within 256 KiB.", Action: "Correct the private file before loading credentials or contacting the service.", Status: 400}
	}
	canonical, err := client.NormalizeAdoptionObligationDocument(document)
	if err != nil {
		return err
	}
	if in.operation == "adopt-apply" {
		if err = client.ValidateAdoptionObligationApply(in.key, in.preview, canonical); err != nil {
			return err
		}
	}
	in.document = canonical
	return nil
}

func executeAdoptionObligations(ctx, parent context.Context, service *client.Client, in *adoptionObligationsInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	mutation := false
	switch in.operation {
	case "list":
		value, err = service.AdoptionObligations(ctx, in.pull, in.status, client.PageOptions{Limit: in.limit, Cursor: in.cursor})
	case "read":
		value, err = service.AdoptionObligation(ctx, in.id)
	case "adopt-preview":
		value, err = service.PreviewAdoptionObligations(ctx, in.document)
	case "adopt-apply":
		mutation = true
		value, err = service.ApplyAdoptionObligations(ctx, in.key, in.preview, in.document)
	case "resolve":
		mutation = true
		value, err = service.ResolveAdoptionObligation(ctx, in.id, in.key, in.resolve)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if !mutation {
		return emit(stdout, stderr, value)
	}
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var written int
		written, err = stdout.Write(raw)
		if err == nil && written != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The carried-obligation receipt could not be written.", Action: "Retry the exact same file or resolution arguments, preview fingerprint when applicable, key and bound instance; do not invent a replacement key.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
