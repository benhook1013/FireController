package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/profile"
)

type profileSetupInvocation struct {
	server, instance, project, worker, credential string
	output, executable                            string
}

type profileSetupResult struct {
	State string `json:"state"`
	profile.Context
	Verified bool `json:"verified"`
}

func validSetupText(value string) bool {
	return value != "" && utf8.ValidString(value) && value == strings.TrimSpace(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func parseProfileSetup(command string, args []string) (*profileSetupInvocation, bool) {
	allowed := map[string]bool{}
	switch command {
	case "create":
		for _, flag := range []string{"--server", "--instance", "--project", "--worker", "--credential-file"} {
			allowed[flag] = true
		}
	case "launcher":
		allowed["--output"], allowed["--executable"] = true, true
	default:
		return nil, false
	}
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		flag, value, assigned := strings.Cut(args[i], "=")
		if !allowed[flag] || values[flag] != "" {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, false
			}
			value = args[i]
		}
		if !validSetupText(value) {
			return nil, false
		}
		values[flag] = value
	}
	for flag := range allowed {
		if flag != "--executable" && values[flag] == "" {
			return nil, false
		}
	}
	if command == "launcher" && (!filepath.IsAbs(values["--output"]) || values["--executable"] != "" && !filepath.IsAbs(values["--executable"])) {
		return nil, false
	}
	return &profileSetupInvocation{server: values["--server"], instance: values["--instance"], project: values["--project"], worker: values["--worker"], credential: values["--credential-file"], output: values["--output"], executable: values["--executable"]}, true
}

func setupCancelled(ctx context.Context, stderr io.Writer) bool {
	if ctx.Err() != nil {
		report(stderr, ExitCancelled, "cancelled", "The local setup command was cancelled before publication.", "Retry the exact same command when cancellation is no longer requested.")
		return true
	}
	return false
}

// Encode before publishing so an oversized binding can never create a file
// whose compact setup result cannot be returned.
func setupOutput(identity profile.Context, state string) ([]byte, bool) {
	body, err := json.Marshal(profileSetupResult{State: state, Context: identity, Verified: false})
	if err != nil || len(body)+1 > 4<<10 {
		return nil, false
	}
	return append(body, '\n'), true
}

func setupOutputTooLarge(stderr io.Writer) int {
	return report(stderr, ExitConfig, "setup_output_too_large", "The local binding exceeds the compact setup response budget.", "Use shorter profile and binding values before retrying.")
}

func reportSetupError(stderr io.Writer, err *profile.Error) int {
	exit := ExitConfig
	if err.Code == "profiles_conflict" || err.Code == "launcher_conflict" {
		exit = ExitConflict
	}
	return reportDetails(stderr, exit, diagnostic{Code: err.Code, Message: err.Message, Action: err.Action, OutcomeUnknown: err.Code == "profiles_write_uncertain" || err.Code == "launcher_write_uncertain"})
}

func emitSetup(stdout, stderr io.Writer, body []byte) int {
	n, err := stdout.Write(body)
	if err != nil || n != len(body) {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The local setup result could not be written.", Action: "Recover by retrying the exact same command and binding; do not replace or delete the possibly published file.", OutcomeUnknown: true})
	}
	return ExitSuccess
}

func createProfile(ctx context.Context, call invocation, stdout, stderr io.Writer) int {
	if !call.hasProfile || !call.hasProfilesFile || !filepath.IsAbs(call.profilesFile) {
		return report(stderr, ExitUsage, "invalid_arguments", "Profile creation requires an explicit profile and absolute profiles file.", "Provide --profile NAME and --profiles-file ABSOLUTE_PATH.")
	}
	if setupCancelled(ctx, stderr) {
		return ExitCancelled
	}
	identity := profile.Context{Profile: call.profile, ServerURL: call.setup.server, InstanceID: call.setup.instance, ProjectID: call.setup.project, WorkerID: call.setup.worker}
	body, ok := setupOutput(identity, "configured")
	if !ok {
		return setupOutputTooLarge(stderr)
	}
	if _, err := profile.Create(call.profilesFile, profile.Binding{Context: identity, CredentialFile: call.setup.credential}); err != nil {
		return reportSetupError(stderr, err)
	}
	return emitSetup(stdout, stderr, body)
}

func createProfileLauncher(ctx context.Context, call invocation, stdout, stderr io.Writer) int {
	if !validSetupText(call.profile) || !validSetupText(call.profilesFile) || !filepath.IsAbs(call.profilesFile) {
		return report(stderr, ExitUsage, "invalid_arguments", "The selected local profile is invalid.", "Use a valid profile selector and an absolute profiles file.")
	}
	if setupCancelled(ctx, stderr) {
		return ExitCancelled
	}
	identity, err := profile.Load(call.profilesFile, call.profile)
	if err != nil {
		return reportSetupError(stderr, err)
	}
	body, ok := setupOutput(identity, "launcher_created")
	if !ok {
		return setupOutputTooLarge(stderr)
	}
	executable := call.setup.executable
	if executable == "" {
		var executableErr error
		executable, executableErr = os.Executable()
		if executableErr != nil {
			return report(stderr, ExitConfig, "launcher_invalid_executable", "The current executable could not be resolved.", "Provide --executable with an absolute executable path.")
		}
	}
	if setupCancelled(ctx, stderr) {
		return ExitCancelled
	}
	published, publishErr := profile.CreateLauncher(call.setup.output, call.profilesFile, call.profile, executable)
	if publishErr != nil {
		return reportSetupError(stderr, publishErr)
	}
	if published != identity {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "profiles_changed", Message: "The selected profile changed during launcher setup.", Action: "Inspect the selected local binding, then retry the exact same command; preserve the possibly published launcher.", OutcomeUnknown: true})
	}
	return emitSetup(stdout, stderr, body)
}
