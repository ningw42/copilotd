// Command verify-usage retains native Usage release evidence. It is verification
// tooling, not a copilotd runtime dependency. Run from the repository root.
package main

import (
	"bufio"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type commandResult struct {
	Argv    []string `json:"argv"`
	Exit    int      `json:"exit"`
	Elapsed string   `json:"elapsed"`
	Output  string   `json:"output_file"`
	Error   string   `json:"error,omitempty"`
}

type verification struct {
	dir      string
	env      []string
	commands []commandResult
}

func main() {
	target := flag.String("target", "", "required native GOOS/GOARCH")
	runner := flag.String("runner", "local", "actual runner label")
	ci := flag.Bool("native-ci", false, "assert hosted-runner identity and exercise controlled disposable-VM system timezone")
	flag.Parse()
	dir, err := filepath.Abs(filepath.Join(".scratch", "native-usage"))
	if err == nil {
		err = os.MkdirAll(dir, 0700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	v := &verification{dir: dir, env: append(os.Environ(), "CGO_ENABLED=0")}
	err = v.verify(*target, *runner, *ci)
	status := map[string]any{"target": *target, "runner": *runner, "passed": err == nil, "error": fmt.Sprint(err), "native_ci": *ci}
	writeErr := v.json("result.json", status)
	fmt.Printf("Usage verification: %v; evidence: %s\n", status, dir)
	if err != nil || writeErr != nil {
		fmt.Fprintln(os.Stderr, errors.Join(err, writeErr))
		os.Exit(1)
	}
}

func (v *verification) verify(target, runner string, ci bool) error {
	actual := runtime.GOOS + "/" + runtime.GOARCH
	goPath, pathErr := exec.LookPath("go")
	metadata := map[string]any{"requested_target": target, "runner_label": runner, "verification_process": actual, "runtime_version": runtime.Version(), "started_utc": time.Now().UTC(), "go_executable": goPath, "go_path_error": fmt.Sprint(pathErr)}
	// Deliberately selected public metadata only: never dump the environment.
	for _, key := range []string{"GITHUB_SHA", "GITHUB_REF", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_JOB", "GITHUB_SERVER_URL", "GITHUB_REPOSITORY", "COPILOTD_WORKFLOW_SHA", "COPILOTD_WORKFLOW_REF", "RUNNER_OS", "RUNNER_ARCH", "ImageOS", "ImageVersion"} {
		metadata[key] = os.Getenv(key)
	}
	metadata["run_url"] = os.Getenv("GITHUB_SERVER_URL") + "/" + os.Getenv("GITHUB_REPOSITORY") + "/actions/runs/" + os.Getenv("GITHUB_RUN_ID") + "/attempts/" + os.Getenv("GITHUB_RUN_ATTEMPT")
	if err := v.json("metadata.json", metadata); err != nil {
		return err
	}
	if target != actual {
		return fmt.Errorf("verification process %s, not requested native %s", actual, target)
	}
	if ci {
		if os.Getenv("GITHUB_ACTIONS") != "true" {
			return errors.New("--native-ci is restricted to disposable GitHub Actions VMs")
		}
		osName := map[string]string{"linux": "Linux", "darwin": "macOS", "windows": "Windows"}[runtime.GOOS]
		arch := map[string]string{"amd64": "X64", "arm64": "ARM64"}[runtime.GOARCH]
		if os.Getenv("RUNNER_OS") != osName || os.Getenv("RUNNER_ARCH") != arch {
			return errors.New("runner OS/architecture mismatch")
		}
	}
	source, err := v.run("source", "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	status, err := v.run("source-status", "git", "status", "--short")
	if err != nil {
		return err
	}
	if ci && (strings.TrimSpace(source) != os.Getenv("GITHUB_SHA") || strings.TrimSpace(status) != "") {
		return errors.New("checked-out source is not the clean workflow source SHA")
	}
	if err := v.checkoutBytes(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		out, err := v.run("native-os", "pwsh", "-NoProfile", "-Command", "[System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant(); [System.Runtime.InteropServices.RuntimeInformation]::OSDescription")
		want := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.TrimSpace(out), want+"\n") && !strings.HasPrefix(strings.TrimSpace(out), want+"\r\n") {
			return fmt.Errorf("native Windows architecture mismatch: %q", out)
		}
	} else {
		out, err := v.run("native-architecture", "uname", "-m")
		if err != nil {
			return err
		}
		want := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
		if strings.TrimSpace(out) != want {
			return fmt.Errorf("native architecture mismatch: %q", out)
		}
		if _, err := v.run("native-os", "uname", "-a"); err != nil {
			return err
		}
		if runtime.GOOS == "linux" {
			data, err := os.ReadFile("/etc/os-release")
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(v.dir, "os-release.txt"), data, 0600); err != nil {
				return err
			}
		}
		if runtime.GOOS == "darwin" {
			if _, err := v.run("macos-version", "sw_vers"); err != nil {
				return err
			}
			out, err := v.run("native-apple-silicon", "sysctl", "-n", "hw.optional.arm64")
			if err != nil || strings.TrimSpace(out) != "1" {
				return fmt.Errorf("Apple Silicon preflight: %v %q", err, out)
			}
		}
	}
	out, err := v.run("go-environment", "go", "env", "-json", "GOHOSTOS", "GOHOSTARCH", "GOOS", "GOARCH", "GOVERSION", "CGO_ENABLED", "GOROOT", "GOTOOLCHAIN")
	if err != nil {
		return err
	}
	var goenv map[string]string
	if err := json.Unmarshal([]byte(out), &goenv); err != nil {
		return err
	}
	if goenv["GOHOSTOS"]+"/"+goenv["GOHOSTARCH"] != target || goenv["GOOS"]+"/"+goenv["GOARCH"] != target || goenv["CGO_ENABLED"] != "0" {
		return errors.New("Go host/target/CGO mismatch; do not mask this by setting GOOS/GOARCH")
	}
	if _, err := v.run("go-version", "go", "version"); err != nil {
		return err
	}
	if _, err := v.run("selected-modules", "go", "list", "-m", "all"); err != nil {
		return err
	}
	binary := filepath.Join(v.dir, "copilotd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if _, err := v.run("build", "go", "build", "-trimpath", "-o", binary, "./cmd/copilotd"); err != nil {
		return err
	}
	file, err := os.Open(binary)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	if err := v.json("executable.json", map[string]string{"path": binary, "sha256": hex.EncodeToString(hash.Sum(nil))}); err != nil {
		return err
	}
	if _, err := v.run("build-metadata", "go", "version", "-m", binary); err != nil {
		return err
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return err
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"]+"/"+settings["GOARCH"] != target || settings["CGO_ENABLED"] != "0" || info.GoVersion != goenv["GOVERSION"] {
		return errors.New("executable build metadata does not match native toolchain/CGO-disabled target")
	}
	v.env = append(v.env, "COPILOTD_TEST_GO_VERSION="+goenv["GOVERSION"], "COPILOTD_TEST_BINARY="+binary, "COPILOTD_TEST_EVIDENCE="+filepath.Join(v.dir, "cli"), "COPILOTD_TEST_NATIVE_TARGET="+target, "COPILOTD_TEST_SYSTEM_CONFIGURATION=untouched image")
	if runtime.GOOS == "linux" {
		if _, err := v.run("namespace-preflight", "unshare", "-Ur", "true"); err != nil {
			return fmt.Errorf("mandatory static-isolation preflight: %w", err)
		}
		v.env = append(v.env, "COPILOTD_TEST_STATIC_BINARY="+binary)
	}
	if _, err := v.run("runtime-preflight", "go", "test", "-json", "./cmd/copilotd", "-run", "^TestUsageNativeRuntime$", "-count=1"); err != nil {
		return err
	}
	_, testErr := v.run("tests", "go", "test", "-json", "./...", "-count=1")
	accountErr := v.account("tests.log", mandatoryTests(runtime.GOOS))
	// Preserve original-image results even when unsupported; independently prove
	// a supported system-link configuration, never weaken the resolver policy.
	var systemErr error
	if ci && runtime.GOOS != "windows" {
		systemErr = v.controlledSystemTimezone()
	}
	return errors.Join(testErr, accountErr, systemErr)
}

// Compare the real working-tree payloads with their committed Git blobs, without
// text conversion. A clean git status alone can hide core.autocrlf conversion.
func (v *verification) checkoutBytes() error {
	setting, err := v.run("checkout-autocrlf", "git", "config", "--default", "unspecified", "--get", "core.autocrlf")
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" && strings.TrimSpace(setting) != "false" {
		return errors.New("Windows verification requires core.autocrlf=false before checkout")
	}
	var evidence []map[string]any
	var mismatches []error
	for i, path := range []string{
		"internal/catalog/codexdata/models.json",
		"internal/shim/testdata/usage/openai-responses-sse.recorded.sse",
		"internal/shim/testdata/usage/anthropic-messages-sse-cumulative.synthetic.sse",
	} {
		blob, err := v.run(fmt.Sprintf("checkout-blob-%d", i), "git", "rev-parse", "HEAD:"+path)
		if err != nil {
			return err
		}
		actual, err := v.run(fmt.Sprintf("checkout-bytes-%d", i), "git", "hash-object", "--no-filters", path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		matches := strings.TrimSpace(blob) == strings.TrimSpace(actual)
		evidence = append(evidence, map[string]any{"path": path, "bytes": len(data), "lf": strings.Count(string(data), "\n"), "crlf": strings.Count(string(data), "\r\n"), "sha256": hex.EncodeToString(hash[:]), "git_blob": strings.TrimSpace(blob), "working_tree_blob": strings.TrimSpace(actual), "matches": matches})
		if !matches {
			mismatches = append(mismatches, fmt.Errorf("checkout changed Git blob bytes: %s", path))
		}
	}
	return errors.Join(append(mismatches, v.json("checkout-bytes.json", evidence))...)
}

func (v *verification) controlledSystemTimezone() (err error) {
	zone := "/usr/share/zoneinfo/Europe/Berlin"
	if runtime.GOOS == "darwin" {
		zone = "/var/db/timezone/zoneinfo/Europe/Berlin"
	}
	if _, err := os.Stat(zone); err != nil {
		return fmt.Errorf("controlled native zone absent: %w", err)
	}
	backup := filepath.Join(v.dir, "original-localtime")
	if _, err := v.run("timezone-backup", "sudo", "cp", "-P", "/etc/localtime", backup); err != nil {
		return err
	}
	defer func() {
		_, removeErr := v.run("timezone-remove-controlled", "sudo", "rm", "-f", "/etc/localtime")
		_, restoreErr := v.run("timezone-restore", "sudo", "cp", "-P", backup, "/etc/localtime")
		err = errors.Join(err, removeErr, restoreErr)
		_ = os.Remove(backup) // Do not upload a platform symlink or zone asset.
	}()
	if _, err := v.run("timezone-controlled-link", "sudo", "ln", "-sfn", zone, "/etc/localtime"); err != nil {
		return err
	}
	v.env = append(v.env, "COPILOTD_TEST_SYSTEM_CONFIGURATION=controlled disposable VM; restored after test", "COPILOTD_TEST_SYSTEM_ZONE=Europe/Berlin")
	_, testErr := v.run("controlled-system-tests", "go", "test", "-json", "./cmd/copilotd", "-run", "^TestUsageExecutableAcceptance$/^system_timezone$", "-count=1")
	return errors.Join(testErr, v.account("controlled-system-tests.log", []string{"cmd/copilotd:TestUsageExecutableAcceptance/system_timezone"}))
}

func (v *verification) run(name string, argv ...string) (string, error) {
	path := filepath.Join(v.dir, name+".log")
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Env = v.env
	command.Stdout, command.Stderr = file, file
	start := time.Now()
	runErr := command.Run()
	closeErr := file.Close()
	code := 0
	if runErr != nil {
		code = -1
		if exit, ok := runErr.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
	}
	v.commands = append(v.commands, commandResult{Argv: argv, Exit: code, Elapsed: time.Since(start).String(), Output: filepath.Base(path), Error: fmt.Sprint(runErr)})
	recordErr := v.json("commands.json", v.commands)
	fmt.Printf("%s: exit %d (%s)\n", name, code, time.Since(start))
	data, readErr := os.ReadFile(path)
	return string(data), errors.Join(runErr, closeErr, recordErr, readErr)
}

func (v *verification) json(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(v.dir, name), append(data, '\n'), 0600)
}

type testEvent struct {
	Action, Package, Test, Output string
	Elapsed                       float64
}

func (v *verification) account(log string, required []string) error {
	file, err := os.Open(filepath.Join(v.dir, log))
	if err != nil {
		return err
	}
	defer file.Close()
	states := map[string]string{}
	var skips []testEvent
	var failures []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		} // go build diagnostics remain in the original log.
		pkg := strings.TrimPrefix(event.Package, "github.com/ningw42/copilotd/")
		if event.Test == "" {
			continue
		}
		key := pkg + ":" + event.Test
		if event.Action == "pass" || event.Action == "skip" || event.Action == "fail" {
			states[key] = event.Action
		}
		if event.Action == "skip" {
			skips = append(skips, event)
			usageScope := strings.HasPrefix(pkg, "internal/usage") || ((pkg == "cmd/copilotd" || pkg == "internal/server") && (strings.Contains(event.Test, "Usage") || strings.Contains(event.Test, "Report")))
			if usageScope && !allowedUsageSkip(runtime.GOOS, key) {
				failures = append(failures, "unexpected mandatory-scope skip: "+key)
			}
		}
	}
	for _, key := range required {
		if states[key] != "pass" {
			failures = append(failures, "required "+key+": "+states[key])
		}
	}
	sort.Strings(failures)
	writeErr := v.json(log+"-accounting.json", map[string]any{"required": required, "states": states, "all_skips": skips, "not_applicable": nativeNotApplicable(runtime.GOOS), "failures": failures})
	if len(failures) != 0 {
		return errors.Join(errors.New(strings.Join(failures, "\n")), scanner.Err(), writeErr)
	}
	return errors.Join(scanner.Err(), writeErr)
}

func allowedUsageSkip(goos, key string) bool {
	if key == "internal/usage/sqlitestore:TestStoreProcessHelper" {
		return true
	}
	if goos != "windows" {
		return false
	}
	switch key {
	case "internal/usage/sqlitestore:TestStoreCreatesPrivateArtifactsAndRejectsUnsafeDestinations",
		"internal/usage/reportcli:TestCommandUnixTZAllowsOneLeadingColon",
		"internal/usage/reportcli:TestCommandEmptyUnixTZSelectsConfiguredUTC",
		"internal/usage/reportcli:TestCommandDiscoversNamedUnixTimezoneThroughHTTP":
		return true
	}
	return false
}

func nativeNotApplicable(goos string) map[string]string {
	notes := map[string]string{}
	if goos == "windows" {
		for _, test := range []string{"TestUsageExecutableInformationalCommandsRetainSIGPIPE", "TestUsageExecutableCancellationUsesCLIErrorPath", "TestUsageExecutableMalformedFlagsWithClosedStderrPipe", "TestUsageExecutableMalformedHelpWithClosedStderrPipe"} {
			notes["cmd/copilotd:"+test] = "Unix-only signal behavior; excluded by unix build tag, not a Windows pass"
		}
	}
	return notes
}

func mandatoryTests(goos string) []string {
	var required []string
	groups := map[string][]string{
		"internal/usage/report": {
			"TestPinnedDriverFirstReadOnlyWALConnections", "TestPinnedDriverReadOnlyGuardAndInterruptCleanup", "TestQueryCapsNativeLockWaitingByRemainingBudget",
			"TestQueryBothNativeSectionsShareOneCommittedSnapshot", "TestQueryReadsCommittedHistoryWithoutFlushingOrRetainingWriter", "TestQueryRejectsReportWhenNativeCleanupFails",
			"TestQueryExactFilterExcludesOversizedUnrelatedIdentity", "TestQueryEnforcesWholeReportResourceLimits", "TestQueryIndexedStreamingNativeEvidence",
			"TestRealSQLiteFailuresUseGenericHTTPResponsesAndReleaseAdmission", "TestInterruptedRealSQLiteScanUsesHTTPDeadlinePrecedenceAndReleasesAdmission",
		},
		"internal/server": {
			"TestUsageIncompleteRequestBodiesReleaseReportSlotsWithinWriteBudget/content_length", "TestUsageIncompleteRequestBodiesReleaseReportSlotsWithinWriteBudget/chunked",
			"TestReportIncompleteBodiesBoundFinalFlushAndRecovery/small_success", "TestReportIncompleteBodiesBoundFinalFlushAndRecovery/head",
			"TestReportIncompleteBodiesBoundFinalFlushAndRecovery/method", "TestReportIncompleteBodiesBoundFinalFlushAndRecovery/disabled",
			"TestReportIncompleteBodiesBoundFinalFlushAndRecovery/syntax", "TestReportIncompleteBodiesBoundFinalFlushAndRecovery/semantic", "TestReportIncompleteBodiesBoundFinalFlushAndRecovery/panic",
		},
		"internal/usage/reporthttp":  {"TestHandlerBodylessWorkTimeoutSurvivesDeadlineScheduling"},
		"internal/usage/sqlitestore": {"TestStoreRecoveredWriteFailureDoesNotPoisonLaterOrFinalLevels"},
		"internal/wsforward":         {"TestProxyWriteTimeoutTearsDownSlowReaderSession"},
		"cmd/copilotd": {
			"TestUsageReportDeadlinesDoNotLeakIntoReusedInferenceConnections",
			"TestUsageExecutableAcceptance/closed_stdout_pipe/compact", "TestUsageExecutableAcceptance/closed_stdout_pipe/--details", "TestUsageExecutableAcceptance/closed_stdout_pipe/--json",
			"TestUsageNativeRuntime", "TestUsageExecutableAcceptance", "TestUsageExecutableAcceptance/process_timezone", "TestUsageExecutableAcceptance/system_timezone", "TestUsageExecutableAcceptance/observed_inference_to_executable",
			"TestUsageReportsOverlapNativeInferenceAndAnotherCommittedWriter", "TestUsageSlowTCPReportsHoldSlotsReleaseSQLiteAndDoNotDeadlineSSE", "TestUsageForcedDrainCancelsRealSQLiteReadAndInference",
			"TestUsageGracefulDrainFinishesReportAndInferenceBeforeWriterCutoff", "TestUsageStorageFailuresDoNotChangeInferenceReadinessOrWriterAdmission", "TestUsageEncodedLimitRejectsWholeRealReportAndReleasesSlots",
			"TestUsageReportClientTeardownClosesAnUnusedDialBeforeGracefulStop", "TestDailyOpenAIUsageCommandThroughProductionListener",
			"TestRunBoundServeForcedWebSocketDrainAndFreshUsageFinalizationAreBounded", "TestRunBoundServeStopsUsageAdmissionBeforeReportingForcedDrainError",
		},
	}
	if goos == "linux" {
		groups["cmd/copilotd"] = append(groups["cmd/copilotd"], "TestUsageExecutableEmbeddedTimezoneWithoutHostData", "TestUsageExecutableDiscoversContainerSystemTimezone")
	}
	if goos == "windows" {
		groups["internal/usage/sqlitestore"] = append(groups["internal/usage/sqlitestore"], "TestStoreWindowsPermissionsAreExplicitlyBestEffort")
	} else {
		groups["cmd/copilotd"] = append(groups["cmd/copilotd"], "TestUsageExecutableInformationalCommandsRetainSIGPIPE", "TestUsageExecutableCancellationUsesCLIErrorPath", "TestUsageExecutableMalformedFlagsWithClosedStderrPipe", "TestUsageExecutableMalformedHelpWithClosedStderrPipe")
	}
	for pkg, tests := range groups {
		for _, test := range tests {
			required = append(required, pkg+":"+test)
		}
	}
	sort.Strings(required)
	return required
}
