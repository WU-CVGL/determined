package internal

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// jobContentMessages are the messages that are a job or carry a job's title, logs or config. The
// log streams' responses are listed themselves, since they do not embed TaskLog. Task and
// Allocation carry free-text failure reasons, which follow the logs.
var jobContentMessages = map[protoreflect.FullName]bool{
	"determined.job.v1.Job":                     true,
	"determined.experiment.v1.Experiment":       true,
	"determined.run.v1.FlatRun":                 true,
	"determined.trial.v1.Trial":                 true,
	"determined.task.v1.GenericTask":            true,
	"determined.command.v1.Command":             true,
	"determined.shell.v1.Shell":                 true,
	"determined.task.v1.AllocationSummary":      true,
	"determined.task.v1.Task":                   true,
	"determined.task.v1.Allocation":             true,
	"determined.checkpoint.v1.Checkpoint":       true,
	"determined.model.v1.ModelVersion":          true,
	"determined.task.v1.TaskLog":                true,
	"determined.api.v1.TaskLogsResponse":        true,
	"determined.api.v1.TaskLogsFieldsResponse":  true,
	"determined.api.v1.TrialLogsResponse":       true,
	"determined.api.v1.TrialLogsFieldsResponse": true,
}

// jobPrivacyCheckedRPCs names, for each RPC whose response reaches job content, the test that
// reads it as a user who may not read that content.
var jobPrivacyCheckedRPCs = map[string]string{
	"GetExperiment":            "TestJobPrivacyExperimentReadPaths",
	"GetExperiments":           "TestJobPrivacyExperimentReadPaths",
	"SearchExperiments":        "TestJobPrivacyExperimentReadPaths",
	"SearchRuns":               "TestJobPrivacyExperimentReadPaths",
	"GetExperimentTrials":      "TestJobPrivacyExperimentReadPaths",
	"GetTrial":                 "TestJobPrivacyExperimentReadPaths",
	"PutTrial":                 "TestJobPrivacyTrialUpsert",
	"GetTask":                  "TestJobPrivacyFailureReasons",
	"GetAllocation":            "TestJobPrivacyFailureReasons",
	"GetJobs":                  "TestJobPrivacyTaskListsAndConfigs",
	"GetJobsV2":                "TestJobPrivacyTaskListsAndConfigs",
	"GetTasks":                 "TestJobPrivacyTaskListsAndConfigs",
	"GetCommands":              "TestJobPrivacyTaskListsAndConfigs",
	"GetCommand":               "TestJobPrivacyTaskListsAndConfigs",
	"GetShells":                "TestJobPrivacyTaskListsAndConfigs",
	"GetShell":                 "TestJobPrivacyTaskListsAndConfigs",
	"GetGenericTasks":          "TestJobPrivacyTaskListsAndConfigs",
	"GetCheckpoint":            "TestJobPrivacyTaskListsAndConfigs",
	"GetExperimentCheckpoints": "TestJobPrivacyTaskListsAndConfigs",
	"GetTrialCheckpoints":      "TestJobPrivacyTaskListsAndConfigs",
	"GetModelVersion":          "TestJobPrivacyTaskListsAndConfigs",
	"GetModelVersions":         "TestJobPrivacyTaskListsAndConfigs",
	"PostModelVersion":         "TestJobPrivacyTaskListsAndConfigs",
	"PatchModelVersion":        "TestJobPrivacyTaskListsAndConfigs",
	"PostCheckpointMetadata":   "TestJobPrivacyTaskListsAndConfigs",
	"TaskLogs":                 "TestJobPrivacyLogReadPaths",
	"TaskLogsFields":           "TestJobPrivacyLogReadPaths",
	"TrialLogs":                "TestJobPrivacyLogReadPaths",
	"TrialLogsFields":          "TestJobPrivacyLogReadPaths",
	"KillCommand":              "TestJobPrivacyFailureReasons",
	"SetCommandPriority":       "TestJobPrivacyFailureReasons",
	"KillShell":                "TestJobPrivacyFailureReasons",
	"SetShellPriority":         "TestJobPrivacyFailureReasons",
	"PatchExperiment":          "TestJobPrivacyControlResponses",
}

// jobPrivacyUncheckedRPCs gives, for each RPC whose response reaches job content and that no test
// reads as a user who may not read it, why no such user gets that content from it. "Editors" may
// be users other than the owner under RBAC.
var jobPrivacyUncheckedRPCs = map[string]string{
	"CreateExperiment": "always a new experiment, which the caller owns; " +
		"redactExperimentForViewer runs",
	"PutExperiment": "an upsert: a PUT of an existing external experiment returns it as " +
		"stored, possibly another user's; redactExperimentForViewer runs, as in GetExperiment",
	"ContinueExperiment": "editors; redactExperimentForViewer runs, as in GetExperiment",
	"LaunchCommand":      "always a new command, which the caller owns and which has not exited",
	"LaunchShell":        "always a new shell, which the caller owns and which has not exited",
	"CreateTrial": "editors; always a new trial (a new task ID, never an upsert), which has " +
		"no log policy match",
	"PatchTrial":           "editors; redactTrialsLogPolicy runs, as in GetTrial",
	"GetTrialByExternalID": "returns GetTrial's response, redaction included",
	"CompareTrials":        "get_trials_basic selects no title, config or log policy match",
}

// jobPrivacyEchoRoutes names, for each Echo route that returns job content, its handler and the
// test that reads it as a user who may not read that content. The descriptor walk cannot see
// them.
var jobPrivacyEchoRoutes = map[string]struct{ handler, test string }{
	"GET /tasks": {"m.getTasks", "TestJobPrivacyTaskListsAndConfigs"},
	"GET /experiments/:experiment_id/preview_gc": {
		"m.getExperimentCheckpointsToGC", "TestJobPrivacyTaskListsAndConfigs",
	},
	"GET /experiments/:experiment_id/model_def": {
		"m.getExperimentModelDefinition", "TestJobPrivacyTaskListsAndConfigs",
	},
}

// reachesJobContent reports whether a message is, or transitively contains, job content.
func reachesJobContent(m protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool {
	if jobContentMessages[m.FullName()] {
		return true
	}
	if seen[m.FullName()] {
		return false
	}
	seen[m.FullName()] = true
	fields := m.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.IsMap() {
			f = f.MapValue()
		}
		if f.Message() != nil && reachesJobContent(f.Message(), seen) {
			return true
		}
	}
	return false
}

// packageTestNames returns the names of the test functions of this package.
func packageTestNames(t *testing.T) map[string]bool {
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	names := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f) //nolint:gosec // The package's own test files.
		require.NoError(t, err)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			names[m[1]] = true
		}
	}
	return names
}

// TestJobPrivacyRPCInventory checks that every RPC whose response reaches job content is checked
// by a job privacy test or says why it needs none, so a new one cannot skip the redaction
// unnoticed; and that every entry still names such an RPC and an existing test.
func TestJobPrivacyRPCInventory(t *testing.T) {
	tests := packageTestNames(t)
	service := apiv1.File_determined_api_v1_api_proto.Services().ByName("Determined")
	require.NotNil(t, service)
	reaching := map[string]bool{}
	methods := service.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		if reachesJobContent(m.Output(), map[protoreflect.FullName]bool{}) {
			reaching[string(m.Name())] = true
		}
	}

	var unlisted []string
	for name := range reaching {
		_, checked := jobPrivacyCheckedRPCs[name]
		_, unchecked := jobPrivacyUncheckedRPCs[name]
		if !checked && !unchecked {
			unlisted = append(unlisted, name)
		}
		require.False(t, checked && unchecked, "%s is listed twice", name)
	}
	sort.Strings(unlisted)
	require.Empty(t, unlisted,
		"these RPCs return job content: redact it for the viewer, add the RPC to a job privacy "+
			"test and list it in jobPrivacyCheckedRPCs")

	for name, test := range jobPrivacyCheckedRPCs {
		require.True(t, reaching[name], "%s no longer returns job content", name)
		require.True(t, tests[test], "%s names %s, which is not a test of this package", name, test)
	}
	for name := range jobPrivacyUncheckedRPCs {
		require.True(t, reaching[name], "%s no longer returns job content", name)
	}

	core, err := os.ReadFile("core.go")
	require.NoError(t, err)
	for route, r := range jobPrivacyEchoRoutes {
		require.Contains(t, string(core), r.handler, "%s is no longer routed", route)
		require.True(t, tests[r.test], "%s names %s, which is not a test of this package",
			route, r.test)
	}
}

// rawTitleSQL matches SQL that reads an experiment's name, description or labels from its config
// directly.
var rawTitleSQL = regexp.MustCompile(
	`config\s*->>?\s*'(name|description|labels)'|config\s*#>>?\s*'\{(name|description|labels)\}'`)

// rawTitleSQLAllowed counts, for each file allowed to read titles directly, its reads; -1 is any
// number. Paths are relative to master.
var rawTitleSQLAllowed = map[string]int{
	// The expressions that every read path uses.
	"internal/jobprivacy/sql.go": -1,
	// getExperimentTx, which loads the experiment for internal callers in full; handlers redact
	// what they return.
	"internal/api_experiment.go": 3,
	// db.ExperimentLabelUsage, which no handler calls.
	"static/srv/get_experiment_labels.sql": 1,
	// The usage aggregates by label, which keep every experiment's labels (documented).
	"static/srv/update_aggregated_allocation.sql": 2,
	// The raw usage rows, whose labels redactRawAllocationLabels omits for the viewer.
	"static/srv/get_raw_allocation.sql": 1,
}

// TestJobPrivacyRawTitleSQL checks that experiment titles are read from the config only through
// the job privacy expressions and the allowed paths.
func TestJobPrivacyRawTitleSQL(t *testing.T) {
	master, err := filepath.Abs("..")
	require.NoError(t, err)
	found := map[string]int{}
	scan := func(root string, keep func(path string) bool) {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !keep(path) {
				return err
			}
			src, err := os.ReadFile(path) //nolint:gosec // The repository's own sources.
			if err != nil {
				return err
			}
			if n := len(rawTitleSQL.FindAllString(string(src), -1)); n > 0 {
				rel, err := filepath.Rel(master, path)
				if err != nil {
					return err
				}
				found[filepath.ToSlash(rel)] = n
			}
			return nil
		})
		require.NoError(t, err)
	}
	scan(filepath.Join(master, "internal"), func(path string) bool {
		return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
	})
	scan(filepath.Join(master, "static", "srv"), func(path string) bool {
		return strings.HasSuffix(path, ".sql")
	})

	for path, n := range found {
		want, ok := rawTitleSQLAllowed[path]
		require.True(t, ok, "%s reads experiment titles from the config: use the jobprivacy "+
			"expressions", path)
		if want >= 0 {
			require.Equal(t, want, n, "%s reads experiment titles from the config", path)
		}
	}
	for path := range rawTitleSQLAllowed {
		require.Contains(t, found, path, "%s no longer reads titles: drop it from the list", path)
	}
}
