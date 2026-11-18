package cli

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/client"
)

// varsInvocation is the light operator-variable surface:
//
//	vars list
//	vars get KEY
//	vars set KEY VALUE [--project] [--expect-revision N] [--key IDEMPOTENCY_KEY]
//	vars unset KEY [--project] [--expect-revision N] [--key IDEMPOTENCY_KEY]
//	vars history KEY [--project] [--limit N] [--cursor CURSOR]
type varsInvocation struct {
	operation, name, value, key string
	scope                       string
	expected                    *int64
	page                        client.PageOptions
}

func (in *varsInvocation) write() bool { return in.operation == "set" || in.operation == "unset" }

func parseVars(operation string, args []string) (*varsInvocation, bool) {
	in := &varsInvocation{operation: operation, scope: "instance"}
	positions := 1
	allowed := map[string]bool{}
	switch operation {
	case "list":
		positions = 0
	case "get":
	case "set":
		positions = 2
		allowed["--project"], allowed["--expect-revision"], allowed["--key"] = true, true, true
	case "unset":
		allowed["--project"], allowed["--expect-revision"], allowed["--key"] = true, true, true
	case "history":
		allowed["--project"], allowed["--limit"], allowed["--cursor"] = true, true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		// A set value may itself begin with '-' only after the key; flags are
		// recognized solely by their exact declared names.
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] {
			positional = append(positional, args[i])
			continue
		}
		if _, exists := values[flag]; exists {
			return nil, false
		}
		if flag == "--project" {
			if assigned {
				return nil, false
			}
			values[flag] = "true"
			continue
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if value == "" || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, false
		}
		values[flag] = value
	}
	if len(positional) != positions {
		return nil, false
	}
	if positions > 0 {
		in.name = positional[0]
		if !client.ValidOperatorVariableKey(in.name) {
			return nil, false
		}
	}
	if positions == 2 {
		in.value = positional[1]
		if !client.ValidOperatorVariableValue(in.value) {
			return nil, false
		}
	}
	if _, ok := values["--project"]; ok {
		in.scope = "project"
	}
	if v, ok := values["--expect-revision"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n == math.MaxInt64 {
			return nil, false
		}
		in.expected = &n
	}
	if v, ok := values["--key"]; ok {
		if !validMutationKey(v) {
			return nil, false
		}
		in.key = v
	}
	in.page.Limit = 25
	if v, ok := values["--limit"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return nil, false
		}
		in.page.Limit = n
	}
	if v, ok := values["--cursor"]; ok {
		if len(v) > 4096 || v != strings.TrimSpace(v) {
			return nil, false
		}
		in.page.Cursor = v
	}
	return in, true
}

func executeVars(ctx, parent context.Context, c *client.Client, in *varsInvocation, stdout, stderr io.Writer) int {
	var value any
	var err error
	switch in.operation {
	case "list":
		value, err = c.OperatorVariables(ctx)
	case "get":
		value, err = c.OperatorVariable(ctx, in.name)
	case "history":
		value, err = c.OperatorVariableHistory(ctx, in.name, in.scope, in.page)
	case "set", "unset":
		key := in.key
		if key == "" {
			if key, err = client.NewOperatorVariableKey(); err != nil {
				return report(stderr, ExitConfig, "key_unavailable", "A fresh idempotency key could not be generated.", "Retry, or pass --key KEY.")
			}
		}
		request := client.SetOperatorVariableRequest{Scope: in.scope, ExpectedRevision: in.expected}
		if in.operation == "set" {
			v := in.value
			request.Value = &v
		}
		value, err = c.SetOperatorVariable(ctx, key, in.name, request)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if !in.write() {
		return emit(stdout, stderr, value)
	}
	raw, err := json.Marshal(value)
	if err == nil {
		raw = append(raw, '\n')
		var n int
		n, err = stdout.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The variable receipt could not be written.", Action: "Read the variable with vars get; writes replace state, so repeating the same set is safe.", OutcomeUnknown: true})
	}
	return ExitSuccess
}
