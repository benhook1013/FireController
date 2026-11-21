package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Extract registrations, not arbitrary strings or a second hand-maintained
// command list. Rules identify the selector used by each existing dispatcher.
// An added command in these switches/conditions must have help, and help for a
// removed registration fails in the other direction. No operation is executed.
var commandDispatchSelectors = map[string]map[string]string{
	"parseInvocation":               {`call.command + " " + call.subcommand`: "", "operationArgs[0]": "reviews rounds "},
	"isMutation":                    {`command + " " + operation`: ""},
	"ownerManagementOperation":      {"operation": ""},
	"parseReview":                   {"call.subcommand": "reviews "},
	"parseProfileSetup":             {"command": "profile "},
	"parseProjectMap":               {`command + " " + operation`: ""},
	"parseTracking":                 {`command + " " + operation`: ""},
	"parseHistory":                  {"operation": "history "},
	"parseMapImport":                {"operation": "map "},
	"parseNotes":                    {"operation": "notes "},
	"parsePresentation":             {"operation": "presentation "},
	"parsePublication":              {"operation": "publication "},
	"parseVars":                     {"operation": "vars "},
	"parseReviewRounds":             {"in.operation": "reviews rounds "},
	"parseReviewScheduling":         {"in.action": "reviews scheduling "},
	"parseAdoptionObligations":      {"in.operation": "reviews obligations "},
	"parseRoundAdoption":            {"args[0]": "reviews rounds adopt "},
	"parseAdoptionObligationsAdopt": {"args[0]": "reviews obligations adopt "},
	"parseCoordinationImport":       {"call.subcommand": "import "},
}

func catalogueExpr(n ast.Node) string {
	var b bytes.Buffer
	// Formatting an expression is independent of source positions/comments.
	if n != nil {
		_ = format.Node(&b, token.NewFileSet(), n)
	}
	return b.String()
}

func catalogueLiteral(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func cliCatalogueSources(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	functions := map[string]*ast.FuncDecl{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				functions[fn.Name.Name] = fn
			}
		}
	}
	return functions
}

func dispatchCommandCatalogue(t *testing.T, functions map[string]*ast.FuncDecl) map[string]bool {
	t.Helper()
	commands, problems := collectDispatchCommandCatalogue(functions)
	for _, problem := range problems {
		t.Error(problem)
	}
	return commands
}

func collectDispatchCommandCatalogue(functions map[string]*ast.FuncDecl) (map[string]bool, []string) {
	commands := map[string]bool{}
	var problems []string
	admissionSwitches := map[string]token.Pos{}
	for name, selectors := range commandDispatchSelectors {
		fn := functions[name]
		if fn == nil {
			problems = append(problems, "missing dispatcher "+name)
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n, ok := n.(*ast.SwitchStmt); ok {
				if prefix, ok := selectors[catalogueExpr(n.Tag)]; ok {
					if admissionSwitches[name] != token.NoPos {
						// A later switch can admit new commands just as the initial
						// one can. Require explicit classification, never skip it.
						problems = append(problems, "additional admission switch in "+name+": "+catalogueExpr(n.Tag))
						return true
					}
					admissionSwitches[name] = n.Pos()
					for _, clause := range n.Body.List {
						for _, value := range clause.(*ast.CaseClause).List {
							if word, ok := catalogueLiteral(value); ok {
								commands[prefix+word] = true
							}
						}
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			e, ok := n.(*ast.BinaryExpr)
			if !ok || e.Op != token.EQL && e.Op != token.NEQ {
				return true
			}
			for _, pair := range [][2]ast.Expr{{e.X, e.Y}, {e.Y, e.X}} {
				if prefix, ok := selectors[catalogueExpr(pair[0])]; ok {
					if word, ok := catalogueLiteral(pair[1]); ok {
						if admissionSwitches[name] == token.NoPos || e.Pos() < admissionSwitches[name] {
							commands[prefix+word] = true
						} else if !commands[prefix+word] {
							// Later validation may reference admitted commands, but
							// cannot silently introduce another public spelling.
							problems = append(problems, "unclassified later dispatch condition in "+name+": "+prefix+word)
						}
					}
				}
			}
			return true
		})
	}
	// These two dispatchers use conjunctions instead of a string switch for
	// several operations. Keep command/operation pairing (not just word parity).
	for _, name := range []string{"parseInvocation", "isMutation"} {
		command, operation := "call.command", "call.subcommand"
		if name == "isMutation" {
			command, operation = "command", "operation"
		}
		ast.Inspect(functions[name].Body, func(n ast.Node) bool {
			var condition ast.Expr
			branch, isBranch := n.(*ast.IfStmt)
			if isBranch {
				condition = branch.Cond
			} else if e, ok := n.(*ast.BinaryExpr); ok {
				// Include routing in direct return/assignment expressions.
				condition = e
			} else {
				return true
			}
			roots, words := []string{}, []string{}
			ast.Inspect(condition, func(n ast.Node) bool {
				if e, ok := n.(*ast.BinaryExpr); ok && e.Op == token.EQL {
					if word, ok := catalogueLiteral(e.Y); ok {
						switch catalogueExpr(e.X) {
						case command:
							roots = append(roots, word)
						case operation:
							words = append(words, word)
						}
					}
				}
				return true
			})
			// Leaf local commands return before subcommand dispatch and use an
			// exact one-word grammar. Derive them too, rather than hard-code an
			// exemption that would hide the next local command.
			if name == "parseInvocation" && isBranch && len(words) == 0 && (admissionSwitches[name] == token.NoPos || branch.Pos() < admissionSwitches[name]) {
				ast.Inspect(branch.Body, func(n ast.Node) bool {
					if e, ok := n.(*ast.BinaryExpr); ok && catalogueExpr(e) == "len(remaining) == 1" {
						for _, root := range roots {
							commands[root] = true
						}
					}
					return true
				})
			}
			for _, root := range roots {
				if catalogueCondition(condition, command, operation, root, "__not_an_operation__") {
					continue
				}
				for _, word := range words {
					if catalogueCondition(condition, command, operation, root, word) {
						path := root + " " + word
						if admissionSwitches[name] == token.NoPos || condition.Pos() < admissionSwitches[name] {
							commands[path] = true
						} else if !commands[path] {
							problems = append(problems, "unclassified later dispatch condition in "+name+": "+path)
						}
					}
				}
			}
			return true
		})
	}
	// Group orientation spellings are help aliases, not runnable operations.
	for command := range commands {
		if strings.HasSuffix(command, " help") || strings.HasSuffix(command, " --help") {
			delete(commands, command)
		}
	}
	return commands, problems
}

// Existential evaluation of non-routing argument predicates preserves routing
// conjunctions and disjunctions without interpreting flag validation or I/O.
func catalogueCondition(e ast.Expr, command, operation, root, word string) bool {
	if p, ok := e.(*ast.ParenExpr); ok {
		return catalogueCondition(p.X, command, operation, root, word)
	}
	if b, ok := e.(*ast.BinaryExpr); ok {
		switch b.Op {
		case token.LAND:
			return catalogueCondition(b.X, command, operation, root, word) && catalogueCondition(b.Y, command, operation, root, word)
		case token.LOR:
			return catalogueCondition(b.X, command, operation, root, word) || catalogueCondition(b.Y, command, operation, root, word)
		case token.EQL, token.NEQ:
			if value, ok := catalogueLiteral(b.Y); ok {
				actual := ""
				switch catalogueExpr(b.X) {
				case command:
					actual = root
				case operation:
					actual = word
				default:
					return true
				}
				return (actual == value) == (b.Op == token.EQL)
			}
		}
	}
	return true
}

// The root admission guard includes the trackingCommand predicate. Derive both
// sides of root/group parity instead of assuming every metadata root is routed.
func dispatchRootCatalogue(functions map[string]*ast.FuncDecl) map[string]bool {
	roots := map[string]bool{}
	for name, selector := range map[string]string{"parseInvocation": "call.command", "trackingCommand": "command"} {
		ast.Inspect(functions[name].Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BinaryExpr:
				if n.Op == token.EQL || n.Op == token.NEQ {
					for _, pair := range [][2]ast.Expr{{n.X, n.Y}, {n.Y, n.X}} {
						if word, ok := catalogueLiteral(pair[1]); ok && catalogueExpr(pair[0]) == selector {
							roots[word] = true
						}
					}
				}
			case *ast.SwitchStmt:
				if catalogueExpr(n.Tag) == selector {
					for _, clause := range n.Body.List {
						for _, value := range clause.(*ast.CaseClause).List {
							if word, ok := catalogueLiteral(value); ok {
								roots[word] = true
							}
						}
					}
				}
			}
			return true
		})
	}
	delete(roots, "--help")
	delete(roots, "-h")
	return roots
}

func TestJobsCreateDiscovery(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"help"}, nil, &out, &errOut); code != ExitSuccess {
		t.Fatal(code, errOut.String())
	}
	var root helpResponse
	if err := json.Unmarshal(out.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, group := range root.CommandGroups {
		for _, command := range group.Commands {
			if command.Command == "jobs create --title TEXT --key KEY" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("root help does not advertise runnable jobs create")
	}
	for _, args := range [][]string{{"jobs", "help"}, {"jobs", "--help"}, {"help", "jobs"}} {
		out.Reset()
		errOut.Reset()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if code := RunContext(ctx, args, map[string]string{"FC_PROFILE": "missing"}, &out, &errOut); code != ExitSuccess || errOut.Len() != 0 {
			t.Fatal(args, code, errOut.String())
		}
		var group operationHelpResponse
		if err := json.Unmarshal(out.Bytes(), &group); err != nil || !slices.Contains(group.Optional, "jobs create --help") {
			t.Fatal(args, out.String(), err)
		}
	}
	call, ok := parseInvocation([]string{"jobs", "create", "--title", "Discovery fixture", "--key", "discovery-create"})
	if !ok || call.mutation == nil || call.mutation.operation != "jobs create" {
		t.Fatal("advertised creation does not select the job mutation")
	}
}

func TestCommandCatalogueParity(t *testing.T) {
	functions := cliCatalogueSources(t)
	checkCatalogueDispatchSources(t, functions)
	commands := dispatchCommandCatalogue(t, functions)
	for command := range commands {
		if command == "help" {
			continue
		}
		if _, ok := operationHelp(command); !ok {
			t.Errorf("registered command lacks local help: %s", command)
		}
	}
	for _, name := range []string{"operationHelp", "ownerManagementHelp", "reviewFollowingHelp", "discoveryOperationHelp"} {
		ast.Inspect(functions[name].Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || catalogueExpr(sw.Tag) != "operation" {
				return true
			}
			for _, clause := range sw.Body.List {
				for _, value := range clause.(*ast.CaseClause).List {
					command, ok := catalogueLiteral(value)
					_, group := discoveryGroups[command]
					if ok && !strings.HasSuffix(command, " help") && !commands[command] && !group {
						t.Errorf("help has no dispatch registration: %s", command)
					}
				}
			}
			return true
		})
	}
	// Metadata must cover every registration and cannot advertise nonexistent
	// children. Group-only nodes are explicitly distinct from runnable leaves.
	listed := map[string]bool{"help": true, "version": true, "context": true, "doctor": true}
	roots := dispatchRootCatalogue(functions)
	for root := range roots {
		if _, group := discoveryGroups[root]; !group && !listed[root] {
			t.Errorf("unclassified root dispatch command: %s", root)
		}
	}
	for group, children := range discoveryGroups {
		if !strings.Contains(group, " ") && !roots[group] {
			t.Errorf("catalogue advertises unrecognized root group: %s", group)
		}
		for _, child := range strings.Fields(children) {
			path := group + " " + child
			listed[path] = true
			if _, subgroup := discoveryGroups[path]; !subgroup && !commands[path] {
				t.Errorf("catalogue advertises unregistered command: %s", path)
			}
		}
	}
	for command := range commands {
		if !listed[command] {
			t.Errorf("unclassified dispatch command: %s", command)
		}
	}
	t.Logf("audited %d registered command/group paths and %d discovery groups", len(commands), len(discoveryGroups))
}

// Existing operation parsers below validate an already registered operation;
// they cannot register another public spelling. Distinguish their internal
// labels from the registration sources, and fail closed on new routing helpers.
var internalCatalogueParsers = map[string]string{
	"parseMutation":                 "isMutation owns registration",
	"parseReviewControl":            "isMutation owns review-control registration",
	"parsePrimaryJob":               "isMutation owns primary selection registration",
	"parseWorkerPause":              "isMutation owns pause/resume registration",
	"parseJobProgress":              "isMutation owns update/checklist registration",
	"parseReviewBatch":              "isMutation owns batch registration",
	"parseReviewPublication":        "isMutation owns publication registration",
	"parseReviewImport":             "parseInvocation and wrapper parsers own public import spellings",
	"parseReviewTransfer":           "parseInvocation owns transfer spellings; import-* is internal reuse",
	"parseSubagentReview":           "parseInvocation owns the three subagent spellings",
	"parseReviewFollowing":          "parseInvocation owns follow/unfollow/following",
	"parseReviewQueueChange":        "parseInvocation owns queue-add/move/remove",
	"parseReviewRun":                "parseReview owns start/run-read",
	"parseOwnerManagement":          "ownerManagementOperation owns registration",
	"parseAdoptionObligationsAdopt": "adopt-preview/adopt-apply are internal; args[0] is registered separately",
}

func checkCatalogueDispatchSources(t *testing.T, functions map[string]*ast.FuncDecl) {
	t.Helper()
	reachable := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if reachable[name] || functions[name] == nil {
			return
		}
		reachable[name] = true
		ast.Inspect(functions[name].Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok {
					visit(name.Name)
				}
			}
			return true
		})
	}
	visit("parseInvocation")
	for name := range commandDispatchSelectors {
		if !reachable[name] {
			t.Errorf("catalogue source is no longer reachable from parsing: %s", name)
		}
	}
	isSelector := func(selector string) bool {
		return selector == "command" || selector == "operation" || selector == "call.command" || selector == "call.subcommand" || selector == "in.operation" || selector == "in.action" || selector == "args[0]" || selector == "operationArgs[0]" || selector == `command + " " + operation` || selector == `call.command + " " + call.subcommand`
	}
	for name, fn := range functions {
		if !reachable[name] || !strings.HasPrefix(name, "parse") {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			selector := ""
			switch n := n.(type) {
			case *ast.SwitchStmt:
				selector = catalogueExpr(n.Tag)
			case *ast.BinaryExpr:
				if n.Op == token.EQL || n.Op == token.NEQ {
					if _, ok := catalogueLiteral(n.Y); ok {
						selector = catalogueExpr(n.X)
					}
				}
			}
			if !isSelector(selector) {
				return true
			}
			if _, ok := commandDispatchSelectors[name][selector]; !ok && internalCatalogueParsers[name] == "" {
				// parseInvocation's paired command/subcommand conditions and
				// parseTracking/ProjectMap validation are handled separately.
				if name != "parseInvocation" && name != "parseTracking" && name != "parseProjectMap" {
					t.Errorf("unclassified dispatch selector %s in %s", selector, name)
				}
			}
			return true
		})
	}
}

func TestCommandCatalogueSupplementalSyntaxReachesParsers(t *testing.T) {
	// Instantiate the advertised required syntax only. Parsing cannot contact a
	// service, load profiles or prepare/import files. This catches stale flag
	// names and required arguments rather than merely testing help against itself.
	replacements := strings.NewReplacer(
		"(--body TEXT | --body-file PATH)", "--body TEXT",
		"(--workstream ENTRY_ID | --clear)", "--clear",
		"(--pr NUMBER | --unassigned)", "--unassigned",
		"ABSOLUTE_PRIVATE_JSON_PATH", "/not-read/input.json",
		"ABSOLUTE_JSON_PATH", "/not-read/input.json",
		"NEW_ABSOLUTE_PATH", "/not-read/new-output",
		"ABSOLUTE_PATH", "/not-read/profiles.json",
		"COLLECTION_ID", "history_collection_"+strings.Repeat("a", 32),
		"PAGE_ID", "history_page_"+strings.Repeat("a", 32),
		"OBLIGATION_ID", "adoption_obligation_"+strings.Repeat("a", 32),
		"JOB_ID", "job_"+strings.Repeat("a", 32),
		"ENTRY_ID", "map_"+strings.Repeat("a", 32),
		"EVENT_CURSOR", "fixture-cursor",
		"SOURCE_KEY", "fixture-source",
		"REVISION", "1", "NUMBER", "42", "N,N", "42,43",
		"HASH", strings.Repeat("a", 64),
		"URL", "https://localhost:8443", "PATH", "/not-read/input",
		"NAME", "fixture", "VALUE", "fixture", "TEXT", "fixture", "REF", "fixture", "KEY", "fixture", "ID", "fixture", "N", "1",
	)
	for command := range dispatchCommandCatalogue(t, cliCatalogueSources(t)) {
		if _, group := discoveryGroups[command]; group {
			continue
		}
		help, ok := discoveryOperationHelp(command)
		if !ok {
			continue
		}
		syntax, _, _ := strings.Cut(strings.TrimPrefix(help.Usage, "<bound launcher> "), "[")
		// Replace only argument placeholders, not characters in the command.
		suffix := strings.TrimPrefix(syntax, command)
		args := append(strings.Fields(command), strings.Fields(replacements.Replace(suffix))...)
		call, ok := parseInvocation(args)
		if !ok || call.command == "help" {
			t.Errorf("advertised syntax does not parse: %s (%v)", help.Usage, args)
		}
	}
}

func TestCommandCatalogueDetectsRegistrationDrift(t *testing.T) {
	functions := cliCatalogueSources(t)
	// Add a nested command and an existing-word/new-parent combination. Neither
	// can hide behind matching individual words in another command family.
	file, err := parser.ParseFile(token.NewFileSet(), "injected.go", `package cli
func parseReviewRounds() { switch in.operation { case "future": } }
func isMutation() { if command == "jobs" && operation == "send" { return true }; switch command + " " + operation { case "jobs future": return true }; return false }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			functions[fn.Name.Name] = fn
		}
	}
	commands := dispatchCommandCatalogue(t, functions)
	for _, path := range []string{"reviews rounds future", "jobs send", "jobs future"} {
		if !commands[path] {
			t.Error("new registration escaped audit", path)
		}
		if _, ok := operationHelp(path); ok {
			t.Error("unknown registration unexpectedly helped", path)
		}
	}
	// Remove an actual registration but leave its later argument-validation
	// comparisons in place. Those comparisons must not masquerade as admission.
	functions = cliCatalogueSources(t)
	ast.Inspect(functions["parseInvocation"].Body, func(n ast.Node) bool {
		if clause, ok := n.(*ast.CaseClause); ok {
			values := clause.List[:0]
			for _, value := range clause.List {
				if s, _ := catalogueLiteral(value); s != "jobs read" {
					values = append(values, value)
				}
			}
			clause.List = values
		}
		return true
	})
	if commands, _ := collectDispatchCommandCatalogue(functions); commands["jobs read"] {
		t.Fatal("removed registration escaped reverse audit")
	}
	if _, ok := operationHelp("jobs read"); !ok {
		t.Fatal("fixture must retain help for removed registration")
	}
}

func TestCommandCatalogueRejectsLaterAdmissionSites(t *testing.T) {
	for _, extra := range []string{
		`switch command + " " + operation { case "jobs future": return true }`,
		`if command == "jobs" && operation == "future" { return true }`,
		`admitted := command == "jobs" && operation == "future"; if admitted { return true }`,
	} {
		functions := cliCatalogueSources(t)
		file, err := parser.ParseFile(token.NewFileSet(), "injected.go", `package cli
func isMutation() bool {
 switch command + " " + operation { case "jobs create": return true }
 `+extra+`
 return false
}`, 0)
		if err != nil {
			t.Fatal(err)
		}
		functions["isMutation"] = file.Decls[0].(*ast.FuncDecl)
		_, problems := collectDispatchCommandCatalogue(functions)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, "in isMutation:") }) {
			t.Errorf("second reachable admission site escaped audit: %s", extra)
		}
	}
}

func TestCommandCatalogueRootDrift(t *testing.T) {
	functions := cliCatalogueSources(t)
	if roots := dispatchRootCatalogue(functions); !roots["items"] || !roots["milestones"] {
		t.Fatal("tracking root predicate was not audited", roots)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "injected.go", `package cli
func trackingCommand(command string) bool { return command == "items" || command == "future" }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	functions["trackingCommand"] = file.Decls[0].(*ast.FuncDecl)
	roots := dispatchRootCatalogue(functions)
	if roots["milestones"] || !roots["future"] {
		t.Fatal("root addition/removal escaped parity inputs", roots)
	}
}

func TestCommandCatalogueTrackingAdvertisedSyntax(t *testing.T) {
	replace := strings.NewReplacer("ABSOLUTE_JSON_PATH", "/not-read/batch.json", "YYYY-MM-DD", "2027-01-31", "TYPE:REF", "pr:42", "REVISION", "1", "CURSOR", "fixture", "STATUS", "planned", "LABEL", "fixture", "TEXT", "fixture", "KEY", "fixture", "N", "1", "[", "", "]", "")
	instantiate := func(s string) []string {
		s, _, _ = strings.Cut(s, " (")
		args := strings.Fields(replace.Replace(s))
		for i := range args {
			args[i], _, _ = strings.Cut(args[i], "|")
		}
		return args
	}
	for command := range dispatchCommandCatalogue(t, cliCatalogueSources(t)) {
		if !strings.HasPrefix(command, "items ") && !strings.HasPrefix(command, "milestones ") {
			continue
		}
		t.Run(command, func(t *testing.T) {
			// Use the effective help, including the older selected reference and
			// supplemental read help. Never prepare files or execute the command.
			help, ok := operationHelp(command)
			if !ok {
				t.Fatal("missing tracking help")
			}
			parse := func(args []string) {
				t.Helper()
				call, ok := parseInvocation(args)
				if !ok || call.tracking == nil || call.tracking.command+" "+call.tracking.operation != command {
					t.Fatalf("advertised tracking syntax does not select %s: %v", command, args)
				}
			}
			syntax, _, _ := strings.Cut(strings.TrimPrefix(help.Usage, "<bound launcher> "), "[")
			args := instantiate(syntax)
			if strings.HasSuffix(command, " set") {
				if command == "items set" {
					args = append(args, "--status", "done")
				} else {
					args = append(args, "--state", "closed")
				}
				args = append(args, "--expect-revision", "1")
			}
			parse(args)
			parse(instantiate(strings.TrimPrefix(help.Example, "<bound launcher> ")))
			for _, optional := range help.Optional {
				flag := instantiate(optional)
				// Replace an already supplied flag (notably --title on create
				// and the set guard), rather than testing duplicate rejection.
				withFlag := append([]string{}, args...)
				if i := slices.Index(withFlag, flag[0]); i >= 0 {
					withFlag = append(withFlag[:i], withFlag[i+2:]...)
				}
				parse(append(withFlag, flag...))
			}
			for _, required := range help.Required {
				if required == "at least one change" {
					continue // Supplied above; a set without a change is invalid.
				}
				for _, token := range instantiate(required) {
					if !slices.Contains(args, token) {
						t.Errorf("required field %q missing from usage %q", required, help.Usage)
					}
				}
			}
		})
	}
}

func TestCommandCatalogueHelpIsOffline(t *testing.T) {
	commands := dispatchCommandCatalogue(t, cliCatalogueSources(t))
	for group := range discoveryGroups {
		commands[group] = true
	}
	paths := make([]string, 0, len(commands))
	for path := range commands {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if path == "help" {
			continue
		} // Root uses its existing orientation schema.
		t.Run(path, func(t *testing.T) {
			spellings := [][]string{append(strings.Fields(path), "--help"), append([]string{"help"}, strings.Fields(path)...)}
			if _, group := discoveryGroups[path]; group {
				spellings = append(spellings, append(strings.Fields(path), "help"))
			}
			for _, args := range spellings {
				args = append([]string{"--profiles-file", "/not-read/discovery-profiles.json", "--profile", "discovery-fixture"}, args...)
				var stdout, stderr bytes.Buffer
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				code := RunContext(ctx, args, nil, &stdout, &stderr)
				var help operationHelpResponse
				if code != ExitSuccess || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &help) != nil || !strings.Contains(help.Usage, path) || len(help.Semantics) == 0 || help.Documentation == "" || stdout.Len() > 4096 {
					t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, &stdout, &stderr)
				}
			}
		})
	}
}

func TestCommandCatalogueAliasesAndInternalLabels(t *testing.T) {
	// Root help aliases retain the root schema and identical content.
	var expected bytes.Buffer
	if code := Run([]string{"help"}, nil, &expected, &bytes.Buffer{}); code != ExitSuccess {
		t.Fatal(code)
	}
	for _, alias := range []string{"--help", "-h"} {
		var out, errOut bytes.Buffer
		if code := Run([]string{alias}, nil, &out, &errOut); code != ExitSuccess || out.String() != expected.String() || errOut.Len() != 0 {
			t.Fatal(alias, code, &out, &errOut)
		}
	}
	// The bare scheduling operation is a supported alias for scheduling read.
	bare, ok := parseInvocation([]string{"reviews", "scheduling"})
	read, readOK := parseInvocation([]string{"reviews", "scheduling", "read"})
	if !ok || !readOK || !reflect.DeepEqual(bare, read) {
		t.Fatal("scheduling read alias changed")
	}
	for _, operation := range []string{"grant", "close", "taper"} {
		args := []string{"reviews", "rounds", operation, "--pr", "42", "--channel", "cli", "--expect-revision", "1", "--key", "catalogue-alias"}
		if operation == "grant" {
			args = append(args, "--max", "1")
		}
		if operation == "taper" {
			args = append(args, "--taper", "2")
		}
		first, ok := parseInvocation(args)
		args[7] = "--expected-revision"
		second, secondOK := parseInvocation(args)
		if !ok || !secondOK || !reflect.DeepEqual(first, second) {
			t.Fatal("round revision flag alias changed", operation, args)
		}
	}
	// These are internal invocation labels/default branches, not additional
	// public command spellings. Wrapper imports must not leak internal names.
	for _, path := range []string{"reviews obligations adopt-preview", "reviews obligations adopt-apply", "reviews attempt", "history export"} {
		if _, ok := operationHelp(path); ok {
			t.Fatal("internal/unsupported spelling advertised", path)
		}
		if _, ok := parseInvocation(strings.Fields(path)); ok {
			t.Fatal("internal/unsupported spelling registered", path)
		}
	}
}
