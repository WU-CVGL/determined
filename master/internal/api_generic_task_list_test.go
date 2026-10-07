package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// jobsSortFixturePath is the order of the Jobs page's sorts, which the WebUI's tests read too.
const jobsSortFixturePath = "../../webui/react/src/fixtures/jobsSortOrder.json"

// jobsSortFixtureRow is a run in the fixture. Rows are listed in ID tie-break order.
type jobsSortFixtureRow struct {
	ID string `json:"id"`
	// GenericTaskOnly rows are left out of the experiments.
	GenericTaskOnly bool   `json:"genericTaskOnly"`
	Name            string `json:"name"`
	// Owner is the owner's display name; nil for a generic task without an owner.
	Owner *string `json:"owner"`
	Pool  *string `json:"pool"`
	Slots int     `json:"slots"`
	// SlotsDefault leaves resources.slots_per_trial out of an experiment's config.
	SlotsDefault bool `json:"slotsDefault"`
	// State is nil for a generic task without a state.
	State *string    `json:"state"`
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end"`
}

type jobsSortFixtureOrder struct {
	Ascend  []string `json:"ascend"`
	Descend []string `json:"descend"`
}

type jobsSortFixture struct {
	Rows   []jobsSortFixtureRow            `json:"rows"`
	Orders map[string]jobsSortFixtureOrder `json:"orders"`
}

// experimentOrder is the order without the rows that only generic tasks have.
func (f jobsSortFixture) experimentOrder(order []string) []string {
	genericTaskOnly := map[string]bool{}
	for _, row := range f.Rows {
		genericTaskOnly[row.ID] = row.GenericTaskOnly
	}
	out := []string{}
	for _, id := range order {
		if !genericTaskOnly[id] {
			out = append(out, id)
		}
	}
	return out
}

// jobsSortFixtureGenericKeys are the generic task sorts of the fixture's keys.
var jobsSortFixtureGenericKeys = map[string]apiv1.GetGenericTasksRequest_SortBy{
	"name":       apiv1.GetGenericTasksRequest_SORT_BY_NAME,
	"owner":      apiv1.GetGenericTasksRequest_SORT_BY_USER,
	"pool":       apiv1.GetGenericTasksRequest_SORT_BY_RESOURCE_POOL,
	"slots":      apiv1.GetGenericTasksRequest_SORT_BY_SLOTS,
	"stateGroup": apiv1.GetGenericTasksRequest_SORT_BY_STATE_GROUP,
	"start":      apiv1.GetGenericTasksRequest_SORT_BY_START_TIME,
	"end":        apiv1.GetGenericTasksRequest_SORT_BY_END_TIME,
}

func loadJobsSortFixture(t *testing.T) jobsSortFixture {
	t.Helper()
	data, err := os.ReadFile(jobsSortFixturePath)
	require.NoError(t, err)
	var fixture jobsSortFixture
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.NotEmpty(t, fixture.Rows)
	keys := make([]string, 0, len(fixture.Orders))
	for key, order := range fixture.Orders {
		keys = append(keys, key)
		require.Len(t, order.Ascend, len(fixture.Rows), key)
		require.Len(t, order.Descend, len(fixture.Rows), key)
	}
	expected := make([]string, 0, len(jobsSortFixtureGenericKeys))
	for key := range jobsSortFixtureGenericKeys {
		expected = append(expected, key)
	}
	require.ElementsMatch(t, expected, keys, "every sort of the Jobs page is in the fixture")
	return fixture
}

// jobsSortFixtureGenericTasks are the fixture's rows as generic tasks, with task IDs that sort in
// the rows' order.
func jobsSortFixtureGenericTasks(fixture jobsSortFixture) []*taskv1.GenericTask {
	out := make([]*taskv1.GenericTask, 0, len(fixture.Rows))
	for i, row := range fixture.Rows {
		task := &taskv1.GenericTask{
			TaskId:    fmt.Sprintf("task-%02d", i),
			Name:      row.Name,
			Slots:     int32(row.Slots),
			StartTime: timestamppb.New(row.Start),
		}
		if row.Owner != nil {
			task.DisplayName = *row.Owner
			task.Username = fmt.Sprintf("user-%02d", i)
		}
		if row.State != nil {
			task.State = taskv1.GenericTaskState(
				taskv1.GenericTaskState_value[genericTaskStateProtoPrefix+*row.State])
		}
		if row.Pool != nil {
			task.ResourcePool = *row.Pool
		}
		if row.End != nil {
			task.EndTime = timestamppb.New(*row.End)
		}
		out = append(out, task)
	}
	return out
}

func TestCompareJobsText(t *testing.T) {
	ascending := []string{
		"",
		" lead",
		"128c",
		"48c",
		"_x", // '_' is before a-z once A-Z is folded, though after A-Z as is.
		"a-b",
		"a_b",
		"ab",
		"ALPHA",
		"Alpha",
		"alpha",
		"Beta",
		"beta",
		"Zed",
		"Éclair", // Only A-Z fold: É and é stay apart, after z.
		"éclair",
		"\u212a",     // KELVIN SIGN does not fold to k.
		"\uff71",     // HALFWIDTH KATAKANA LETTER A, before
		"\U0001F600", // an emoji, by code point (not by UTF-16 code unit).
	}
	for i := range ascending {
		for j := range ascending {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			require.Equal(t, want, compareJobsText(ascending[i], ascending[j]),
				"%q vs %q", ascending[i], ascending[j])
		}
	}
}

func TestGenericTaskLessMatchesJobsSortFixture(t *testing.T) {
	fixture := loadJobsSortFixture(t)
	labels := map[string]string{}
	tasks := jobsSortFixtureGenericTasks(fixture)
	for i, task := range tasks {
		labels[task.TaskId] = fixture.Rows[i].ID
	}
	slices.Reverse(tasks)

	for key, sortBy := range jobsSortFixtureGenericKeys {
		for _, desc := range []bool{false, true} {
			orderBy, want := apiv1.OrderBy_ORDER_BY_ASC, fixture.Orders[key].Ascend
			if desc {
				orderBy, want = apiv1.OrderBy_ORDER_BY_DESC, fixture.Orders[key].Descend
			}
			less, err := genericTaskLess(sortBy, orderBy)
			require.NoError(t, err)
			sorted := slices.Clone(tasks)
			sort.SliceStable(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
			got := make([]string, 0, len(sorted))
			for _, task := range sorted {
				got = append(got, labels[task.TaskId])
			}
			require.Equal(t, want, got, "%s %s", key, orderBy)
		}
	}
}

func TestGenericTaskLess(t *testing.T) {
	at := func(sec int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC))
	}
	sortIDs := func(
		sortBy apiv1.GetGenericTasksRequest_SortBy, orderBy apiv1.OrderBy, tasks ...*taskv1.GenericTask,
	) []string {
		t.Helper()
		less, err := genericTaskLess(sortBy, orderBy)
		require.NoError(t, err)
		sorted := slices.Clone(tasks)
		sort.SliceStable(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })
		ids := []string{}
		for _, task := range sorted {
			ids = append(ids, task.TaskId)
		}
		return ids
	}
	asc, desc := apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC

	// Without a sort: newest first, then by task ID. A key without a direction is ascending.
	older := &taskv1.GenericTask{TaskId: "c", StartTime: at(1)}
	newerB := &taskv1.GenericTask{TaskId: "b", StartTime: at(2)}
	newerA := &taskv1.GenericTask{TaskId: "a", StartTime: at(2)}
	unspecified := apiv1.OrderBy_ORDER_BY_UNSPECIFIED
	require.Equal(t, []string{"a", "b", "c"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_UNSPECIFIED, unspecified, older, newerB, newerA))
	require.Equal(t, []string{"c", "a", "b"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_UNSPECIFIED, asc, older, newerB, newerA))
	require.Equal(t, []string{"c", "a", "b"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_START_TIME, unspecified, older, newerB, newerA))

	// A task without a state or an owner comes last in either direction. The owner is the
	// display name, or the username without one.
	paused := &taskv1.GenericTask{
		TaskId: "paused", State: taskv1.GenericTaskState_GENERIC_TASK_STATE_PAUSED,
		Username: "zed", StartTime: at(1),
	}
	stopping := &taskv1.GenericTask{
		TaskId: "stopping", State: taskv1.GenericTaskState_GENERIC_TASK_STATE_STOPPING_PAUSED,
		Username: "bob", DisplayName: "Yan", StartTime: at(1),
	}
	noState := &taskv1.GenericTask{TaskId: "no-state", StartTime: at(3)}
	ended := &taskv1.GenericTask{
		TaskId: "ended", State: taskv1.GenericTaskState_GENERIC_TASK_STATE_ERROR,
		Username: "alice", StartTime: at(1),
	}
	for _, orderBy := range []apiv1.OrderBy{asc, desc} {
		ids := sortIDs(apiv1.GetGenericTasksRequest_SORT_BY_STATE_GROUP, orderBy,
			noState, paused, ended, stopping)
		require.Equal(t, "no-state", ids[3], orderBy)
	}
	require.Equal(t, []string{"stopping", "paused", "ended", "no-state"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_STATE_GROUP, asc, noState, paused, ended, stopping))
	require.Equal(t, []string{"ended", "stopping", "paused", "no-state"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_USER, asc, noState, paused, ended, stopping))
	require.Equal(t, []string{"paused", "stopping", "ended", "no-state"}, sortIDs(
		apiv1.GetGenericTasksRequest_SORT_BY_USER, desc, noState, paused, ended, stopping))

	_, err := genericTaskLess(99, asc)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	_, err = genericTaskLess(apiv1.GetGenericTasksRequest_SORT_BY_NAME, 99)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
}
