package internal

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

const genericTaskStateProtoPrefix = "GENERIC_TASK_STATE_"

// genericTaskListRow is a generic task with its owner and its latest snapshot.
type genericTaskListRow struct {
	bun.BaseModel `bun:"table:tasks,alias:t"`

	TaskID       model.TaskID           `bun:"task_id"`
	JobID        *model.JobID           `bun:"job_id"`
	State        *model.TaskState       `bun:"task_state"`
	StartTime    time.Time              `bun:"start_time"`
	EndTime      *time.Time             `bun:"end_time"`
	ParentID     *model.TaskID          `bun:"parent_id"`
	ForkedFrom   *string                `bun:"forked_from"`
	NoPause      *bool                  `bun:"no_pause"`
	OwnerID      *int32                 `bun:"owner_id"`
	Username     *string                `bun:"username"`
	DisplayName  *string                `bun:"display_name"`
	AllocationID *string                `bun:"allocation_id"`
	Spec         *tasks.GenericTaskSpec `bun:"generic_task_spec"`
}

// genericTaskSlotsExpr is the slot count a generic task asks for. Creating a task always
// stores it; a spec without one counts as 0 slots, as the task's slots field reports it.
const genericTaskSlotsExpr = "COALESCE((cs.generic_task_spec->'GenericTaskConfig'" +
	"->'resources'->>'slots')::int, 0)"

// genericTaskWorkspaceExpr is the workspace of a generic task, from its stored spec.
const genericTaskWorkspaceExpr = "(cs.generic_task_spec->>'WorkspaceID')::int"

// GetGenericTasks lists generic tasks, filtered by owner, workspace, project, state, parent, name
// or slot count, among those in workspaces the user may view, and sorted as asked: newest first
// by default.
func (a *apiServer) GetGenericTasks(
	ctx context.Context, req *apiv1.GetGenericTasksRequest,
) (*apiv1.GetGenericTasksResponse, error) {
	if err := grpcutil.ValidateRequest(grpcutil.ValidateLimit(req.Limit)); err != nil {
		return nil, err
	}
	less, err := genericTaskLess(req.SortBy, req.OrderBy)
	if err != nil {
		return nil, err
	}
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}
	if req.WorkspaceId != 0 {
		if _, err := a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false); err != nil {
			return nil, err
		}
	}
	if req.ProjectId != 0 {
		if _, err := a.GetProjectByID(ctx, req.ProjectId, *curUser); err != nil {
			return nil, err
		}
	}
	scopes, err := command.AuthZProvider.Get().AccessibleScopes(
		ctx, *curUser, model.AccessScopeID(req.WorkspaceId),
	)
	if err != nil {
		return nil, err
	}
	if req.WorkspaceId != 0 && len(scopes) == 0 {
		return nil, api.NotFoundErrs("workspace", strconv.Itoa(int(req.WorkspaceId)), true)
	}

	query := db.Bun().NewSelect().Model((*genericTaskListRow)(nil)).
		ColumnExpr("t.task_id, t.job_id, t.task_state, t.start_time, t.end_time").
		ColumnExpr("t.parent_id, t.forked_from, t.no_pause").
		ColumnExpr("j.owner_id, u.username, u.display_name").
		ColumnExpr("cs.allocation_id, cs.generic_task_spec").
		Join("JOIN jobs AS j ON j.job_id = t.job_id").
		Join("LEFT JOIN users AS u ON u.id = j.owner_id").
		Join("LEFT JOIN command_state AS cs ON cs.task_id = t.task_id").
		Where("t.task_type = ?", model.TaskTypeGeneric)
	if len(req.Users) > 0 {
		query = query.Where("u.username IN (?)", bun.In(req.Users))
	}
	if len(req.UserIds) > 0 {
		query = query.Where("j.owner_id IN (?)", bun.In(req.UserIds))
	}
	if len(req.States) > 0 {
		states := make([]model.TaskState, 0, len(req.States))
		for _, s := range req.States {
			name := s.String()
			if s == taskv1.GenericTaskState_GENERIC_TASK_STATE_UNSPECIFIED ||
				len(name) <= len(genericTaskStateProtoPrefix) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid generic task state %s", name)
			}
			states = append(states, model.TaskState(name[len(genericTaskStateProtoPrefix):]))
		}
		query = query.Where("t.task_state IN (?)", bun.In(states))
	}
	if req.ParentId != nil {
		query = query.Where("t.parent_id = ?", *req.ParentId)
	}
	if len(req.TaskIds) > 0 {
		query = query.Where("t.task_id IN (?)", bun.In(req.TaskIds))
	}
	if req.ProjectId != 0 {
		// The spec has no JSON tags, so its keys are the Go field names.
		query = query.Where("(cs.generic_task_spec->>'ProjectID')::int = ?", req.ProjectId)
	}
	if len(req.WorkspaceIds) > 0 {
		// Workspaces that are gone match no task; those the user cannot view fail the scope check.
		query = query.Where(genericTaskWorkspaceExpr+" IN (?)", bun.In(req.WorkspaceIds))
	}
	query = applySlotsFilter(query, genericTaskSlotsExpr, req.Slots, req.SlotsAbove)

	var rows []genericTaskListRow
	if err := query.Scan(ctx, &rows); err != nil {
		return nil, err
	}

	search := strings.ToLower(req.Search)
	resp := &apiv1.GetGenericTasksResponse{Tasks: make([]*taskv1.GenericTask, 0, len(rows))}
	for _, row := range rows {
		if row.Spec == nil || !scopes[model.AccessScopeID(row.Spec.WorkspaceID)] {
			continue
		}
		if req.WorkspaceId != 0 && int32(row.Spec.WorkspaceID) != req.WorkspaceId {
			continue
		}
		task := row.toProto()
		// The name shown for a task without one is made from its ID, so search what clients see.
		if search != "" && !strings.Contains(strings.ToLower(task.Name), search) &&
			!strings.Contains(strings.ToLower(task.TaskId), search) {
			continue
		}
		resp.Tasks = append(resp.Tasks, task)
	}
	sort.SliceStable(resp.Tasks, func(i, j int) bool { return less(resp.Tasks[i], resp.Tasks[j]) })
	return resp, api.Paginate(&resp.Pagination, &resp.Tasks, req.Offset, req.Limit)
}

func (row genericTaskListRow) toProto() *taskv1.GenericTask {
	spec := *row.Spec
	if spec.Base.TaskID == "" {
		spec.Base.TaskID = string(row.TaskID)
	}
	res := spec.GenericTaskConfig.Resources
	out := &taskv1.GenericTask{
		TaskId:       string(row.TaskID),
		Name:         spec.DisplayName(),
		Description:  spec.GenericTaskConfig.Description,
		WorkspaceId:  int32(spec.WorkspaceID),
		ProjectId:    int32(spec.ProjectID),
		StartTime:    timestamppb.New(row.StartTime),
		NoPause:      genericTaskNoPause(row.NoPause),
		ForkedFrom:   row.ForkedFrom,
		AllocationId: row.AllocationID,
	}
	if row.JobID != nil {
		out.JobId = row.JobID.String()
	}
	if row.State != nil {
		out.State = taskv1.GenericTaskState(
			taskv1.GenericTaskState_value[genericTaskStateProtoPrefix+string(*row.State)])
	}
	if row.EndTime != nil {
		out.EndTime = timestamppb.New(*row.EndTime)
	}
	if row.ParentID != nil {
		out.ParentId = (*string)(row.ParentID)
	}
	if row.OwnerID != nil {
		out.UserId = *row.OwnerID
	}
	if row.Username != nil {
		out.Username = *row.Username
	}
	if row.DisplayName != nil {
		out.DisplayName = *row.DisplayName
	}
	// Raw fields: a list must not fail on a spec that was persisted without defaults.
	if res.RawResourcePool != nil {
		out.ResourcePool = *res.RawResourcePool
	}
	if res.RawSlots != nil {
		out.Slots = int32(*res.RawSlots)
	}
	return out
}

// genericTaskLess is the order of the generic task list for the sort and the direction: the key,
// with a missing value last in either direction, then the newest start, then the task ID. Text
// compares as compareJobsText does, as the experiment list sorts it in SQL.
func genericTaskLess(
	sortBy apiv1.GetGenericTasksRequest_SortBy, orderBy apiv1.OrderBy,
) (func(a, b *taskv1.GenericTask) bool, error) {
	var desc bool
	switch orderBy {
	case apiv1.OrderBy_ORDER_BY_UNSPECIFIED:
		desc = sortBy == apiv1.GetGenericTasksRequest_SORT_BY_UNSPECIFIED
	case apiv1.OrderBy_ORDER_BY_ASC:
	case apiv1.OrderBy_ORDER_BY_DESC:
		desc = true
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid order by %s", orderBy)
	}

	// compareKey compares two tasks' keys in ascending order; ok is false for a missing key.
	var compareKey func(a, b *taskv1.GenericTask) (cmp int, aOK, bOK bool)
	switch sortBy {
	case apiv1.GetGenericTasksRequest_SORT_BY_UNSPECIFIED,
		apiv1.GetGenericTasksRequest_SORT_BY_START_TIME:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			return compareTimestamps(a.StartTime, b.StartTime), true, true
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_END_TIME:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			return compareTimestamps(a.EndTime, b.EndTime), a.EndTime != nil, b.EndTime != nil
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_NAME:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			return compareJobsText(a.Name, b.Name), true, true
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_STATE_GROUP:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			ga, aOK := genericTaskStateGroup(a.State)
			gb, bOK := genericTaskStateGroup(b.State)
			return ga - gb, aOK, bOK
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_USER:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			oa, ob := genericTaskOwnerName(a), genericTaskOwnerName(b)
			return compareJobsText(oa, ob), oa != "", ob != ""
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_RESOURCE_POOL:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			return compareJobsText(a.ResourcePool, b.ResourcePool),
				a.ResourcePool != "", b.ResourcePool != ""
		}
	case apiv1.GetGenericTasksRequest_SORT_BY_SLOTS:
		compareKey = func(a, b *taskv1.GenericTask) (int, bool, bool) {
			return compareInts(a.Slots, b.Slots), true, true
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "invalid sort by %s", sortBy)
	}

	return func(a, b *taskv1.GenericTask) bool {
		cmp, aOK, bOK := compareKey(a, b)
		switch {
		case aOK != bOK:
			return aOK
		case !aOK:
			cmp = 0
		case desc:
			cmp = -cmp
		}
		if cmp == 0 {
			cmp = -compareTimestamps(a.StartTime, b.StartTime)
		}
		if cmp == 0 {
			cmp = strings.Compare(a.TaskId, b.TaskId)
		}
		return cmp < 0
	}, nil
}

// compareJobsText orders text as the Jobs page does: by code point with A-Z folded to a-z, then
// by code point as is. It is the order of Postgres's lower(x COLLATE "C"), x COLLATE "C": under
// the C collation lower() folds A-Z only.
func compareJobsText(a, b string) int {
	if cmp := strings.Compare(foldASCIIUpper(a), foldASCIIUpper(b)); cmp != 0 {
		return cmp
	}
	return strings.Compare(a, b)
}

// foldASCIIUpper maps A-Z to a-z and keeps every other byte. Go compares strings byte by byte,
// which for UTF-8 is the order of the code points.
func foldASCIIUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// compareTimestamps compares two timestamps; a nil one is the earliest.
func compareTimestamps(a, b *timestamppb.Timestamp) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.AsTime().Compare(b.AsTime())
}

func compareInts(a, b int32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// genericTaskStateGroup is a generic task's state group: 0 active, 1 paused, 2 ended, as
// experimentStateGroupExpr groups experiments. ok is false for a task without a state.
func genericTaskStateGroup(state taskv1.GenericTaskState) (group int, ok bool) {
	switch state {
	case taskv1.GenericTaskState_GENERIC_TASK_STATE_UNSPECIFIED:
		return 0, false
	case taskv1.GenericTaskState_GENERIC_TASK_STATE_PAUSED:
		return 1, true
	case taskv1.GenericTaskState_GENERIC_TASK_STATE_COMPLETED,
		taskv1.GenericTaskState_GENERIC_TASK_STATE_CANCELED,
		taskv1.GenericTaskState_GENERIC_TASK_STATE_ERROR:
		return 2, true
	default:
		return 0, true
	}
}

// genericTaskOwnerName is the owner's display name, or the username without one; empty without
// an owner.
func genericTaskOwnerName(task *taskv1.GenericTask) string {
	if task.DisplayName != "" {
		return task.DisplayName
	}
	return task.Username
}
