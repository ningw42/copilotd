package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type productionFile struct {
	rel    string
	syntax *ast.File
}

type componentSink struct {
	function  string
	consumer  string
	component string
}

type componentObservation struct {
	sink     componentSink
	argument int
}

type componentConsumer struct {
	function string
	consumer string
}

type baseParameter struct {
	name  string
	index int
}

// expectedLogKeys is #151's two-way inventory from the design. Rule 4 checks
// provenance against the registry parsed from keys.go, not against this table.
var expectedLogKeys = map[string]string{
	"ServiceKey":                "service",
	"VersionKey":                "version",
	"ComponentKey":              "component",
	"RequestIDKey":              "request_id",
	"SurfaceKey":                "surface",
	"InboundKey":                "inbound",
	"WSKey":                     "ws",
	"UpstreamRequestIDKey":      "upstream_request_id",
	"MethodKey":                 "method",
	"StatusKey":                 "status",
	"BytesKey":                  "bytes",
	"DurationKey":               "duration",
	"HandshakeDurationKey":      "handshake_duration",
	"OutcomeKey":                "outcome",
	"FramesKey":                 "frames",
	"FallbacksKey":              "fallbacks",
	"CatalogShapeKey":           "catalog_shape",
	"TerminalReasonKey":         "terminal_reason",
	"CloseCodeKey":              "close_code",
	"MsgsC2UKey":                "msgs_c2u",
	"MsgsU2CKey":                "msgs_u2c",
	"BytesC2UKey":               "bytes_c2u",
	"BytesU2CKey":               "bytes_u2c",
	"PanicKey":                  "panic",
	"TimeoutKey":                "timeout",
	"AddrKey":                   "addr",
	"ErrorKey":                  "error",
	"StageKey":                  "stage",
	"CachedValueKey":            "cached_value",
	"CachedValueVersionKey":     "cached_value_version",
	"CachedValueSourceKey":      "cached_value_source",
	"TriggerKey":                "trigger",
	"FailureClassKey":           "failure_class",
	"ExpiresAtKey":              "expires_at",
	"RefreshInKey":              "refresh_in",
	"AttemptKey":                "attempt",
	"AttemptsKey":               "attempts",
	"IntervalKey":               "interval",
	"VerificationURIKey":        "verification_uri",
	"ExpiresInKey":              "expires_in",
	"LoginKey":                  "login",
	"PathKey":                   "path",
	"ModelKey":                  "model",
	"MetadataSourceKey":         "metadata_source",
	"SkipReasonKey":             "skip_reason",
	"ReviewerKey":               "reviewer",
	"BuildKey":                  "build",
	"ConfigKey":                 "config",
	"EnabledShimsKey":           "enabled_shims",
	"ShimKey":                   "shim",
	"HookKey":                   "hook",
	"HookStateKey":              "hook_state",
	"ThresholdKey":              "threshold",
	"HookOverrunsKey":           "hook_overruns",
	"QueueFullDropsKey":         "queue_full_drops",
	"RuntimeWriteLossesKey":     "runtime_write_losses",
	"LateAfterCutoffDropsKey":   "late_after_cutoff_drops",
	"FinalFlushLossesKey":       "final_flush_losses",
	"DriverCleanupCompletedKey": "driver_cleanup_completed",
}

var expectedComponentSinks = map[componentSink]struct{}{
	{function: "runServe", consumer: "local", component: "cmd/copilotd"}:                                 {},
	{function: "runServe", consumer: "sqlitestore.Open", component: "internal/usage/sqlitestore"}:        {},
	{function: "runBoundServe", consumer: "local", component: "cmd/copilotd"}:                            {},
	{function: "runBoundServe", consumer: "runServeStartup", component: "cmd/copilotd"}:                  {},
	{function: "runBoundServe", consumer: "upstream.New", component: "internal/upstream"}:                {},
	{function: "runBoundServe", consumer: "forward.New arg 7", component: "internal/sse"}:                {},
	{function: "runBoundServe", consumer: "forward.New arg 8", component: "internal/shim"}:               {},
	{function: "runBoundServe", consumer: "wsforward.New arg 6", component: "internal/wsforward"}:        {},
	{function: "runBoundServe", consumer: "wsforward.New arg 7", component: "internal/shim"}:             {},
	{function: "runBoundServe", consumer: "server.New arg 1", component: "internal/server"}:              {},
	{function: "runBoundServe", consumer: "server.New arg 2", component: "internal/catalog"}:             {},
	{function: "buildServeProvider", consumer: "identity.NewManager", component: "internal/identity"}:    {},
	{function: "buildServeProvider", consumer: "impersonation.New", component: "internal/cache"}:         {},
	{function: "configuredCodexModels", consumer: "catalog.NewModelsCache", component: "internal/cache"}: {},
	{function: "runLogin", consumer: "local", component: "cmd/copilotd"}:                                 {},
	{function: "runLogin", consumer: "identity.Login", component: "internal/identity"}:                   {},
}

var expectedBaseParameters = map[string]baseParameter{
	"runBoundServe":         {name: "base", index: 2},
	"buildServeProvider":    {name: "base", index: 1},
	"configuredCodexModels": {name: "base", index: 3},
}

var attributeConstructors = map[string]struct{}{
	"Any": {}, "Bool": {}, "Duration": {}, "Float64": {}, "Group": {},
	"GroupAttrs": {}, "Int": {}, "Int64": {}, "String": {}, "Time": {}, "Uint64": {},
}

var packageEmissionFunctions = map[string]struct{}{
	"Debug": {}, "DebugContext": {}, "Info": {}, "InfoContext": {},
	"Warn": {}, "WarnContext": {}, "Error": {}, "ErrorContext": {},
	"Log": {}, "LogAttrs": {},
}

func TestVariadicKeysOnFieldSelectedLoggerAreChecked(t *testing.T) {
	fset := token.NewFileSet()
	syntax, err := parser.ParseFile(fset, "field_receiver.go", `package sample
func emit(c *consumer) {
	c.logger.DebugContext(nil, "event", "sneaky", 1)
}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	var emission *ast.CallExpr
	ast.Inspect(syntax, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			if selected, ok := call.Fun.(*ast.SelectorExpr); ok && selected.Sel.Name == "DebugContext" {
				emission = call
			}
		}
		return true
	})
	if emission == nil {
		t.Fatal("field-selected logger emission was not parsed")
	}
	problems := emissionArgumentProblems(emission, false, expectedLogKeys)
	if len(problems) != 1 || problems[0].missingValue {
		t.Fatalf("field-selected unregistered key problems = %#v, want one provenance failure", problems)
	}
	literal, ok := problems[0].key.(*ast.BasicLit)
	if !ok || literal.Value != `"sneaky"` {
		t.Fatalf("reported key = %#v, want sneaky string literal", problems[0].key)
	}
}

func TestProductionLogStructure(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	fset := token.NewFileSet()
	files := parseProductionFiles(t, fset, root)
	if len(files) == 0 {
		t.Fatal("parsed zero production Go files under cmd/ and internal/")
	}

	registry := readKeyRegistry(t, files)
	checkRegistry(t, registry)
	checkProductionCallsAndKeys(t, fset, files, registry)
	checkWebSocketKeySource(t, fset, files)
	checkComponentInventory(t, fset, files)
	checkShimThresholdAssembly(t, fset, files)
	checkBaseUses(t, fset, files)
}

func parseProductionFiles(t *testing.T, fset *token.FileSet, root string) []productionFile {
	t.Helper()
	var files []productionFile
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			syntax, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, productionFile{rel: filepath.ToSlash(rel), syntax: syntax})
			return nil
		})
		if err != nil {
			t.Fatalf("parse production Go files under %s: %v", dir, err)
		}
	}
	return files
}

func readKeyRegistry(t *testing.T, files []productionFile) map[string]string {
	t.Helper()
	registry := make(map[string]string)
	foundFile := false
	for _, file := range files {
		if file.rel != "internal/logging/keys.go" {
			continue
		}
		foundFile = true
		for _, decl := range file.syntax.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if len(values.Names) != len(values.Values) {
					t.Errorf("%s: registry declaration must spell one value per name", file.rel)
					continue
				}
				for i, name := range values.Names {
					literal, ok := values.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Errorf("%s: registry constant %s is not a string literal", file.rel, name.Name)
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Errorf("%s: registry constant %s: %v", file.rel, name.Name, err)
						continue
					}
					if _, duplicate := registry[name.Name]; duplicate {
						t.Errorf("%s: duplicate registry constant %s", file.rel, name.Name)
					}
					registry[name.Name] = value
				}
			}
		}
	}
	if !foundFile {
		t.Fatal("internal/logging/keys.go was not parsed")
	}
	return registry
}

func checkRegistry(t *testing.T, registry map[string]string) {
	t.Helper()
	for name, want := range expectedLogKeys {
		if got, ok := registry[name]; !ok {
			t.Errorf("logging registry is missing %s", name)
		} else if got != want {
			t.Errorf("logging registry %s = %q, want %q", name, got, want)
		}
	}
	for name, value := range registry {
		if _, ok := expectedLogKeys[name]; !ok {
			t.Errorf("logging registry has unexpected constant %s=%q", name, value)
		}
	}
}

func checkProductionCallsAndKeys(t *testing.T, fset *token.FileSet, files []productionFile, registry map[string]string) {
	t.Helper()
	for _, file := range files {
		checkGovernedImportNames(t, fset, file)
		// internal/config's nested setting names come from ADR-0012's descriptor
		// table inside LogValue. Rules 1 and 3 still prove it emits no records.
		configExcluded := strings.HasPrefix(file.rel, "internal/config/")
		loggingPackage := strings.HasPrefix(file.rel, "internal/logging/")
		ast.Inspect(file.syntax, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if isSelector(node.Fun, "slog", "Default") {
					t.Errorf("%s: production must not call slog.Default()", position(fset, node))
				}
				if pkg, name, ok := selector(node.Fun); ok && pkg == "slog" {
					if _, emission := packageEmissionFunctions[name]; emission {
						t.Errorf("%s: package-level slog.%s emission is forbidden", position(fset, node), name)
					}
					if _, constructor := attributeConstructors[name]; constructor && !configExcluded {
						if len(node.Args) == 0 || !isRegistryKey(node.Args[0], loggingPackage, registry) {
							t.Errorf("%s: slog.%s key must resolve directly to the logging registry", position(fset, node), name)
						}
					}
				}
				if !configExcluded {
					checkEmissionArguments(t, fset, node, loggingPackage, registry)
				}
			case *ast.CompositeLit:
				if !configExcluded && isSlogAttrType(node.Type) {
					checkAttrComposite(t, fset, node, loggingPackage, registry)
				}
			}
			return true
		})
	}
}

// checkGovernedImportNames preserves the syntactic soundness of rules 1, 3,
// and 4: log/slog and the key registry must retain the names those rules inspect.
func checkGovernedImportNames(t *testing.T, fset *token.FileSet, file productionFile) {
	t.Helper()
	for _, imported := range file.syntax.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil || imported.Name == nil {
			continue
		}
		var required string
		switch path {
		case "log/slog":
			required = "slog"
		case "github.com/ningw42/copilotd/internal/logging":
			required = "logging"
		default:
			continue
		}
		if imported.Name.Name != required {
			t.Errorf("%s: %s must be imported as %s so slog structure is checkable syntactically", position(fset, imported), path, required)
		}
	}
}

type emissionArgumentProblem struct {
	key          ast.Expr
	missingValue bool
}

func checkEmissionArguments(t *testing.T, fset *token.FileSet, call *ast.CallExpr, loggingPackage bool, registry map[string]string) {
	t.Helper()
	for _, problem := range emissionArgumentProblems(call, loggingPackage, registry) {
		if problem.missingValue {
			t.Errorf("%s: variadic slog key has no value", position(fset, problem.key))
			continue
		}
		t.Errorf("%s: variadic slog key must resolve directly to the logging registry", position(fset, problem.key))
	}
}

func emissionArgumentProblems(call *ast.CallExpr, loggingPackage bool, registry map[string]string) []emissionArgumentProblem {
	selected, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	start := -1
	switch selected.Sel.Name {
	case "Debug", "Info", "Warn", "Error":
		start = 1
	case "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
		start = 2
	case "Log":
		start = 3
	case "LogAttrs":
		return nil
	default:
		return nil
	}
	if len(call.Args) <= start {
		return nil
	}
	var problems []emissionArgumentProblem
	for i := start; i < len(call.Args); {
		arg := call.Args[i]
		if call.Ellipsis.IsValid() && i == len(call.Args)-1 {
			return problems
		}
		if isAttrExpression(arg) {
			i++
			continue
		}
		if !isRegistryKey(arg, loggingPackage, registry) {
			problems = append(problems, emissionArgumentProblem{key: arg})
			if i+1 >= len(call.Args) {
				return problems
			}
			i += 2
			continue
		}
		if i+1 >= len(call.Args) {
			problems = append(problems, emissionArgumentProblem{key: arg, missingValue: true})
			return problems
		}
		i += 2
	}
	return problems
}

func checkAttrComposite(t *testing.T, fset *token.FileSet, literal *ast.CompositeLit, loggingPackage bool, registry map[string]string) {
	t.Helper()
	if len(literal.Elts) == 0 {
		t.Errorf("%s: slog.Attr composite has no registry-backed Key", position(fset, literal))
		return
	}
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			if element == literal.Elts[0] && !isRegistryKey(element.(ast.Expr), loggingPackage, registry) {
				t.Errorf("%s: slog.Attr key must resolve directly to the logging registry", position(fset, element))
			}
			continue
		}
		field, ok := keyValue.Key.(*ast.Ident)
		if ok && field.Name == "Key" && !isRegistryKey(keyValue.Value, loggingPackage, registry) {
			t.Errorf("%s: slog.Attr Key must resolve directly to the logging registry", position(fset, keyValue.Value))
		}
	}
}

func checkWebSocketKeySource(t *testing.T, fset *token.FileSet, files []productionFile) {
	t.Helper()
	for _, file := range files {
		inspectWithParents(file.syntax, func(node ast.Node, parents []ast.Node) {
			identifier, ok := node.(*ast.Ident)
			if !ok || identifier.Name != "WSKey" || file.rel == "internal/logging/keys.go" {
				return
			}
			if !insideWebSocketRegistration(parents) {
				t.Errorf("%s: logging.WSKey is allowed only in a registration function literal with an endpoint.WSForward parameter", position(fset, identifier))
			}
		})
	}
}

func insideWebSocketRegistration(parents []ast.Node) bool {
	for i := len(parents) - 1; i >= 0; i-- {
		literal, ok := parents[i].(*ast.FuncLit)
		if ok && hasSelectorParameter(literal.Type.Params, "endpoint", "WSForward") {
			return true
		}
	}
	return false
}

func checkComponentInventory(t *testing.T, fset *token.FileSet, files []productionFile) {
	t.Helper()
	var observations []componentObservation
	for _, file := range files {
		inspectWithParents(file.syntax, func(node ast.Node, parents []ast.Node) {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSelector(call.Fun, "logging", "ForComponent") {
				return
			}
			if file.rel != "cmd/copilotd/main.go" {
				t.Errorf("%s: logging.ForComponent must be called only in cmd/copilotd", position(fset, call))
			}
			function := enclosingFunction(parents)
			consumer, argument := enclosingConsumer(call, parents)
			component := "<non-literal>"
			if len(call.Args) != 2 {
				t.Errorf("%s: logging.ForComponent requires base and one component literal", position(fset, call))
			} else if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				if unquoted, err := strconv.Unquote(literal.Value); err == nil {
					component = unquoted
				}
			} else {
				t.Errorf("%s: logging.ForComponent component must be a string literal", position(fset, call.Args[1]))
			}
			observations = append(observations, componentObservation{
				sink:     componentSink{function: function, consumer: consumer, component: component},
				argument: argument,
			})
		})
	}

	multiplicity := make(map[componentConsumer]int)
	for _, observation := range observations {
		key := componentConsumer{function: observation.sink.function, consumer: observation.sink.consumer}
		multiplicity[key]++
	}
	actual := make(map[componentSink]int)
	for _, observation := range observations {
		sink := observation.sink
		key := componentConsumer{function: sink.function, consumer: sink.consumer}
		// Positions disambiguate only consumers that receive multiple Component
		// children; single-child constructors remain insensitive to parameter moves.
		if multiplicity[key] > 1 {
			sink.consumer += " arg " + strconv.Itoa(observation.argument)
		}
		actual[sink]++
	}

	found := 0
	for sink, count := range actual {
		found += count
		if _, ok := expectedComponentSinks[sink]; !ok {
			t.Errorf("unexpected component sink: function=%s consumer=%s component=%s", sink.function, sink.consumer, sink.component)
		} else if count != 1 {
			t.Errorf("component sink appears %d times: function=%s consumer=%s component=%s", count, sink.function, sink.consumer, sink.component)
		}
	}
	if found == 0 {
		t.Fatal("matched zero logging.ForComponent inventory rows")
	}
	for sink := range expectedComponentSinks {
		if actual[sink] != 1 {
			t.Errorf("missing component sink: function=%s consumer=%s component=%s", sink.function, sink.consumer, sink.component)
		}
	}
}

func checkShimThresholdAssembly(t *testing.T, fset *token.FileSet, files []productionFile) {
	t.Helper()
	mainFile := findProductionFile(t, files, "cmd/copilotd/main.go")
	expected := map[string]int{
		"forward.New":   9,
		"wsforward.New": 8,
	}
	found := make(map[string]int)
	ast.Inspect(mainFile.syntax, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		consumer := callName(call.Fun)
		argument, governed := expected[consumer]
		if !governed {
			return true
		}
		found[consumer]++
		if len(call.Args) <= argument {
			t.Errorf("%s: %s has no Hook overrun threshold argument %d", position(fset, call), consumer, argument)
			return true
		}
		selection, ok := call.Args[argument].(*ast.SelectorExpr)
		if !ok {
			t.Errorf("%s: %s Hook overrun threshold must be cfg.ShimHookOverrunThreshold", position(fset, call.Args[argument]), consumer)
			return true
		}
		cfg, cfgOK := selection.X.(*ast.Ident)
		if !cfgOK || cfg.Name != "cfg" || selection.Sel.Name != "ShimHookOverrunThreshold" {
			t.Errorf("%s: %s Hook overrun threshold must be cfg.ShimHookOverrunThreshold", position(fset, call.Args[argument]), consumer)
		}
		return true
	})
	for consumer := range expected {
		if found[consumer] != 1 {
			t.Errorf("cmd/copilotd assembly has %d %s calls, want exactly one", found[consumer], consumer)
		}
	}
}

func checkBaseUses(t *testing.T, fset *token.FileSet, files []productionFile) {
	t.Helper()
	expectedOrigins := map[string]struct{}{"runServe": {}, "runLogin": {}}
	constructorCalls := 0
	for _, file := range files {
		inspectWithParents(file.syntax, func(node ast.Node, parents []ast.Node) {
			call, ok := node.(*ast.CallExpr)
			if !ok || !(isSelector(call.Fun, "logging", "New") || isSelector(call.Fun, "logging", "NewWithWriter")) {
				return
			}
			constructorCalls++
			function := enclosingFunction(parents)
			if file.rel != "cmd/copilotd/main.go" {
				t.Errorf("%s: logging base constructor is outside cmd/copilotd", position(fset, call))
			} else if _, ok := expectedOrigins[function]; !ok {
				t.Errorf("%s: unexpected logging base origin in %s", position(fset, call), function)
			}
		})
	}

	mainFile := findProductionFile(t, files, "cmd/copilotd/main.go")
	functions := make(map[string]*ast.FuncDecl)
	for _, decl := range mainFile.syntax.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok {
			functions[function.Name.Name] = function
		}
	}

	for name, expected := range expectedBaseParameters {
		function := functions[name]
		if function == nil {
			t.Errorf("cmd/copilotd is missing base-carrying function %s", name)
			continue
		}
		parameters := flattenedParameters(function.Type.Params)
		// The closed base-carrying table identifies each syntactically tracked
		// parameter by its declared position and name; changing either is a table edit.
		if expected.index >= len(parameters) || parameters[expected.index].Name != expected.name {
			t.Errorf("%s base parameter = %#v, want %s at index %d", name, identifierNames(parameters), expected.name, expected.index)
			continue
		}
		checkBaseIdentifierUses(t, fset, function, parameters[expected.index])
	}

	actualOrigins := make(map[string]int)
	for name, function := range functions {
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
				return true
			}
			call, ok := assignment.Rhs[0].(*ast.CallExpr)
			if !ok || !(isSelector(call.Fun, "logging", "New") || isSelector(call.Fun, "logging", "NewWithWriter")) {
				return true
			}
			base, ok := assignment.Lhs[0].(*ast.Ident)
			if !ok {
				t.Errorf("%s: logging base must be assigned to an identifier", position(fset, assignment.Lhs[0]))
				return true
			}
			actualOrigins[name]++
			if _, expected := expectedOrigins[name]; !expected {
				t.Errorf("%s: unexpected logging base origin in %s", position(fset, call), name)
			}
			// Keeping the constructor result named base makes the restricted identifier
			// visible at the composition root and matches #152's closed base inventory.
			if base.Name != "base" {
				t.Errorf("%s: logging base identifier = %s, want base for closed-inventory tracking", position(fset, base), base.Name)
			}
			checkBaseIdentifierUses(t, fset, function, base)
			return true
		})
	}
	assignedOrigins := 0
	for name := range expectedOrigins {
		assignedOrigins += actualOrigins[name]
		if actualOrigins[name] != 1 {
			t.Errorf("base origins in %s = %d, want 1", name, actualOrigins[name])
		}
	}
	if constructorCalls != assignedOrigins {
		t.Errorf("logging base constructor calls = %d, directly assigned approved origins = %d", constructorCalls, assignedOrigins)
	}
}

func checkBaseIdentifierUses(t *testing.T, fset *token.FileSet, function *ast.FuncDecl, declaration *ast.Ident) {
	t.Helper()
	inspectWithParents(function.Body, func(node ast.Node, parents []ast.Node) {
		identifier, ok := node.(*ast.Ident)
		if !ok || !sameObject(identifier, declaration) || identifier == declaration {
			return
		}
		call, argumentIndex, ok := directCallArgument(identifier, parents)
		if !ok {
			t.Errorf("%s: unsupported use of component-free base %s in %s", position(fset, identifier), declaration.Name, function.Name.Name)
			return
		}
		pkg, name, selectorCall := selector(call.Fun)
		if selectorCall && argumentIndex == 0 {
			if pkg == "slog" && name == "SetDefault" {
				return
			}
			if pkg == "logging" && (name == "ForComponent" || name == "DependencyErrorLog") {
				return
			}
		}
		if !selectorCall {
			callee, ok := call.Fun.(*ast.Ident)
			if ok {
				if expected, listed := expectedBaseParameters[callee.Name]; listed && argumentIndex == expected.index {
					return
				}
			}
		}
		t.Errorf("%s: component-free base passed to unlisted use %s argument %d", position(fset, identifier), callName(call.Fun), argumentIndex)
	})
}

func findProductionFile(t *testing.T, files []productionFile, rel string) productionFile {
	t.Helper()
	for _, file := range files {
		if file.rel == rel {
			return file
		}
	}
	t.Fatalf("production file %s was not parsed", rel)
	return productionFile{}
}

func inspectWithParents(root ast.Node, visit func(ast.Node, []ast.Node)) {
	var parents []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			parents = parents[:len(parents)-1]
			return false
		}
		visit(node, parents)
		parents = append(parents, node)
		return true
	})
}

func selector(expr ast.Expr) (string, string, bool) {
	selection, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	pkg, ok := selection.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	return pkg.Name, selection.Sel.Name, true
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	gotPkg, gotName, ok := selector(expr)
	return ok && gotPkg == pkg && gotName == name
}

func isRegistryKey(expr ast.Expr, loggingPackage bool, registry map[string]string) bool {
	for {
		parenthesized, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = parenthesized.X
	}
	if selection, ok := expr.(*ast.SelectorExpr); ok {
		pkg, ok := selection.X.(*ast.Ident)
		if !ok || pkg.Name != "logging" {
			return false
		}
		_, ok = registry[selection.Sel.Name]
		return ok
	}
	// internal/logging may reference the constants declared in its own registry
	// directly; every importing package must use a logging.<Name>Key selector.
	identifier, ok := expr.(*ast.Ident)
	if !ok || !loggingPackage {
		return false
	}
	_, ok = registry[identifier.Name]
	return ok
}

func isAttrExpression(expr ast.Expr) bool {
	if call, ok := expr.(*ast.CallExpr); ok {
		pkg, name, ok := selector(call.Fun)
		if ok && pkg == "slog" {
			_, ok = attributeConstructors[name]
			return ok
		}
	}
	if literal, ok := expr.(*ast.CompositeLit); ok {
		return isSlogAttrType(literal.Type)
	}
	return false
}

func isSlogAttrType(expr ast.Expr) bool {
	selection, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selection.X.(*ast.Ident)
	return ok && pkg.Name == "slog" && selection.Sel.Name == "Attr"
}

func hasSelectorParameter(fields *ast.FieldList, pkg, name string) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		if isSelector(field.Type, pkg, name) {
			return true
		}
	}
	return false
}

func enclosingFunction(parents []ast.Node) string {
	for i := len(parents) - 1; i >= 0; i-- {
		if function, ok := parents[i].(*ast.FuncDecl); ok {
			return function.Name.Name
		}
	}
	return "<package>"
}

func enclosingConsumer(target *ast.CallExpr, parents []ast.Node) (string, int) {
	for i := len(parents) - 1; i >= 0; i-- {
		call, ok := parents[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		for argumentIndex, argument := range call.Args {
			if argument == target {
				return callName(call.Fun), argumentIndex
			}
		}
	}
	return "local", -1
}

func callName(expr ast.Expr) string {
	if pkg, name, ok := selector(expr); ok {
		return pkg + "." + name
	}
	if identifier, ok := expr.(*ast.Ident); ok {
		return identifier.Name
	}
	return "<expression>"
}

func flattenedParameters(fields *ast.FieldList) []*ast.Ident {
	if fields == nil {
		return nil
	}
	var parameters []*ast.Ident
	for _, field := range fields.List {
		parameters = append(parameters, field.Names...)
	}
	return parameters
}

func identifierNames(identifiers []*ast.Ident) []string {
	names := make([]string, len(identifiers))
	for i, identifier := range identifiers {
		names[i] = identifier.Name
	}
	return names
}

func sameObject(identifier, declaration *ast.Ident) bool {
	if declaration.Obj != nil {
		return identifier.Obj == declaration.Obj
	}
	return identifier.Name == declaration.Name
}

func directCallArgument(identifier *ast.Ident, parents []ast.Node) (*ast.CallExpr, int, bool) {
	for i := len(parents) - 1; i >= 0; i-- {
		call, ok := parents[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		for argumentIndex, argument := range call.Args {
			if argument == identifier {
				return call, argumentIndex, true
			}
		}
	}
	return nil, 0, false
}

func position(fset *token.FileSet, node ast.Node) string {
	return fset.Position(node.Pos()).String()
}
