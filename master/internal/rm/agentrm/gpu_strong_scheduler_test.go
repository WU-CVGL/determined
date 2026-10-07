package agentrm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// strongPool runs the scheduling passes of a pool with the priority scheduler on agents of
// fixtures as schedulerTick runs them, without the database: every planned request is reserved
// on the agents with the pass's GPU policy and marked allocated, then the check after the pass
// runs on the agents as the pass left them.
type strongPool struct {
	rp     *resourcePool
	live   map[aproto.ID]*agentState
	policy gpuPolicy
}

// strongTask is a task of a strongPool test: its slots, priority, and whether it asks for
// prefer_gpu_topology "strong".
type strongTask struct {
	id       string
	slots    int
	priority int
	strong   bool
	// running places the task on these GPUs of agent "a".
	running []int
	// nonPreemptible keeps a running task out of the preemption search.
	nonPreemptible bool
}

func newStrongPool(
	t *testing.T, preemption bool, policy gpuPolicy, agents map[aproto.ID]topologyFixture, tasks ...strongTask,
) *strongPool {
	t.Helper()
	conf := &config.ResourcePoolConfig{PoolName: "gpus", Scheduler: &config.SchedulerConfig{
		Priority: &config.PrioritySchedulerConfig{
			DefaultPriority: ptrs.Ptr(42), Preemption: preemption,
		},
		FittingPolicy: best,
	}}
	var mockTasks []*MockTask
	var groups []*MockGroup
	start := time.Now()
	for i, task := range tasks {
		group := &MockGroup{ID: "job-" + task.id, Priority: ptrs.Ptr(task.priority)}
		groups = append(groups, group)
		mockTasks = append(mockTasks, &MockTask{
			ID: model.AllocationID(task.id), SlotsNeeded: task.slots, Group: group,
			NonPreemptible:    task.nonPreemptible,
			JobSubmissionTime: start.Add(time.Duration(i) * time.Second),
		})
	}
	rp := setupResourcePool(t, nil, conf, mockTasks, groups, nil)
	t.Cleanup(rp.stop)
	s := &strongPool{rp: rp, live: map[aproto.ID]*agentState{}, policy: policy}
	for id, f := range agents {
		state := topologyAgentState(t, f)
		state.id = id
		s.live[id] = state
	}
	for _, task := range tasks {
		req, ok := rp.taskList.TaskByID(model.AllocationID(task.id))
		require.True(t, ok)
		if task.strong {
			req.FittingRequirements.GPUTopology = expconf.GPUTopologyStrong
		}
		req.FittingRequirements.SingleAgent = true
		if task.running != nil {
			s.run(t, req, s.live["a"], gpuDeviceList(task.running...))
		}
	}
	return s
}

// run records req as running on devices of state.
func (s *strongPool) run(
	t *testing.T, req *sproto.AllocateRequest, state *agentState, devices []device.Device,
) {
	t.Helper()
	cid := cproto.NewID()
	for _, d := range devices {
		require.Nil(t, state.Devices[d], "device %d", d.ID)
		state.Devices[d] = &cid
	}
	state.containerState[cid] = &cproto.Container{ID: cid, Devices: devices}
	s.allocated(req, state, cid, devices)
}

// allocated marks req allocated with the container cid of state.
func (s *strongPool) allocated(
	req *sproto.AllocateRequest, state *agentState, cid cproto.ID, devices []device.Device,
) {
	s.rp.taskList.AddAllocation(req.AllocationID, &sproto.ResourcesAllocated{
		ID: req.AllocationID, JobSubmissionTime: req.JobSubmissionTime,
		Resources: map[sproto.ResourcesID]sproto.Resources{
			sproto.ResourcesID(cid): &containerResources{
				req: req, agent: state, containerID: cid, devices: devices,
			},
		},
	})
}

// pass runs one scheduling pass. It returns the devices of the requests it allocated, the
// requests it asks to release, and whether the check after the pass asks for another pass.
func (s *strongPool) pass(t *testing.T) (map[string][]int, []model.AllocationID, bool) {
	t.Helper()
	s.rp.mu.Lock()
	defer s.rp.mu.Unlock()
	s.rp.agentStatesCache = s.live
	s.rp.gpuPolicy = s.policy
	toAllocate, toRelease := s.rp.scheduler.Schedule(s.rp)
	allocated := map[string][]int{}
	reserved := false
	for _, req := range toAllocate {
		fits := findFits(req, s.live, s.rp.fittingMethod, false, s.policy.packNUMA)
		if len(fits) == 0 {
			continue
		}
		require.Len(t, fits, 1)
		reserved = true
		cid := cproto.NewID()
		res, err := fits[0].Agent.allocateFreeDevices(fits[0].Slots, cid, s.policy.selection(req, fits))
		if err != nil {
			continue
		}
		s.allocated(req, fits[0].Agent, cid, res.devices)
		allocated[string(req.AllocationID)] = deviceIDs(res.devices)
	}
	return allocated, toRelease, s.rp.checkStrongRequests(reserved)
}

// addStrong adds a pending task with prefer_gpu_topology "strong" at the default priority and
// subscribes to its events.
func (s *strongPool) addStrong(t *testing.T, id string, slots int) *sproto.ResourcesSubscription {
	t.Helper()
	sub := rmevents.Subscribe(model.AllocationID(id))
	t.Cleanup(sub.Close)
	s.rp.mu.Lock()
	defer s.rp.mu.Unlock()
	s.rp.groups[model.JobID(id)] = &tasklist.Group{JobID: model.JobID(id), Weight: 1, Priority: ptrs.Ptr(42)}
	s.rp.taskList.AddTask(&sproto.AllocateRequest{
		AllocationID: model.AllocationID(id), JobID: model.JobID(id), SlotsNeeded: slots,
		JobSubmissionTime: time.Now(),
		FittingRequirements: sproto.FittingRequirements{
			SingleAgent: true, GPUTopology: expconf.GPUTopologyStrong,
		},
	})
	return sub
}

// exit ends a running task: its devices are free and it leaves the task list.
func (s *strongPool) exit(t *testing.T, id string) {
	t.Helper()
	s.rp.mu.Lock()
	defer s.rp.mu.Unlock()
	allocated := s.rp.taskList.Allocation(model.AllocationID(id))
	require.NotNil(t, allocated, id)
	removeTaskFromAgents(s.live, allocated)
	s.rp.taskList.RemoveTaskByID(model.AllocationID(id))
}

func TestStrongStartsInOnePassUnderPacking(t *testing.T) {
	// GPUs 5-7 are busy: a 1-GPU task and then a strong 4-GPU task start in the same pass, the
	// first on GPU 4, packed, and the second on NUMA node 0.
	s := newStrongPool(t, false, gpuPolicy{packNUMA: true}, map[aproto.ID]topologyFixture{"a": node02},
		strongTask{id: "busy", slots: 3, priority: 42, running: []int{5, 6, 7}},
		strongTask{id: "one", slots: 1, priority: 42},
		strongTask{id: "strong", slots: 4, priority: 42, strong: true},
	)
	allocated, _, again := s.pass(t)
	require.Equal(t, map[string][]int{"one": {4}, "strong": {0, 1, 2, 3}}, allocated)
	require.False(t, again)
}

func TestStrongPlansWithThePassXIDs(t *testing.T) {
	// GPU 7 is busy and GPU 0 has a recent critical XID in the pass's policy. With it, the first
	// strong 2-GPU task takes healthy GPUs of NUMA node 0 (fewer allocatable healthy slots), the
	// strong 3-GPU task takes node 1, and the second 2-GPU task takes the rest of node 0, GPU 0
	// included: all three start in one pass. A plan without the pass's XIDs puts the first on
	// node 1 and leaves no node for the third.
	xids := map[string]bool{gpuDevice(0).UUID: true}
	s := newStrongPool(t, false, gpuPolicy{packNUMA: true, xids: xids}, map[aproto.ID]topologyFixture{"a": node02},
		strongTask{id: "busy", slots: 1, priority: 42, running: []int{7}},
		strongTask{id: "first", slots: 2, priority: 42, strong: true},
		strongTask{id: "three", slots: 3, priority: 42, strong: true},
		strongTask{id: "last", slots: 2, priority: 42, strong: true},
	)
	allocated, _, again := s.pass(t)
	require.Equal(t, map[string][]int{"first": {1, 2}, "three": {4, 5, 6}, "last": {0, 3}}, allocated)
	require.False(t, again)
}

func TestStrongStartsWithinTwoPassesWithoutPacking(t *testing.T) {
	// The same in map order (worst, or numa_packing false): the plan and the reservations take
	// random GPUs for the 1-GPU task. The strong task starts, in the first pass or in the one the
	// check after it asks for, exactly when the 1-GPU task's reservation left NUMA node 0 free;
	// otherwise it waits. A pass that reserves nothing asks for no other.
	const waits = "waits"
	outcomes := map[string]int{}
	for trial := 0; trial < 1000; trial++ {
		s := newStrongPool(t, false, gpuPolicy{}, map[aproto.ID]topologyFixture{"a": node02},
			strongTask{id: "busy", slots: 3, priority: 42, running: []int{5, 6, 7}},
			strongTask{id: "one", slots: 1, priority: 42},
			strongTask{id: "strong", slots: 4, priority: 42, strong: true},
		)
		allocated, _, again := s.pass(t)
		one := allocated["one"]
		require.Len(t, one, 1)
		outcome := waits
		switch {
		case allocated["strong"] != nil:
			outcome = "first pass"
			require.False(t, again)
		case again:
			outcome = "second pass"
			allocated, _, again = s.pass(t)
			require.False(t, again)
		}
		if outcome != waits {
			require.Equal(t, []int{4}, one, "trial %d", trial)
			require.Equal(t, []int{0, 1, 2, 3}, allocated["strong"], "trial %d", trial)
		} else {
			require.NotEqual(t, []int{4}, one, "trial %d", trial)
		}
		allocated, _, again = s.pass(t)
		require.Empty(t, allocated)
		require.False(t, again)
		outcomes[outcome]++
	}
	t.Logf("%v", outcomes)
}

func TestStrongMissBlocksLowerPrioritiesOnly(t *testing.T) {
	// 6 GPUs are free, 3 on each NUMA node. A strong 4-GPU task at priority 10 waits; a 1-GPU task
	// at its priority starts, one at a lower priority does not, as for any task that waits. A
	// plain 4-GPU task at priority 10 starts, and so does the lower-priority one.
	for _, strong := range []bool{true, false} {
		s := newStrongPool(t, false, gpuPolicy{packNUMA: true}, map[aproto.ID]topologyFixture{"a": node02},
			strongTask{id: "busy", slots: 2, priority: 50, running: []int{3, 7}},
			strongTask{id: "four", slots: 4, priority: 10, strong: strong},
			strongTask{id: "same", slots: 1, priority: 10},
			strongTask{id: "lower", slots: 1, priority: 42},
		)
		allocated, _, again := s.pass(t)
		started := map[string]bool{}
		for id := range allocated {
			started[id] = true
		}
		if strong {
			require.Equal(t, map[string]bool{"same": true}, started)
		} else {
			require.Equal(t, map[string]bool{"four": true, "same": true, "lower": true}, started)
		}
		require.False(t, again)
	}
}

func TestStrongPreemptsWithTheExistingSearch(t *testing.T) {
	// Preemption on. Tasks at priority 50 hold GPU 0 (not preemptible), 4 and 5. A strong 4-GPU
	// task at priority 10 preempts the two on NUMA node 1: the copies hold the real devices. The
	// freed node is not held: after the first exit, the next pass preempts the other again, and
	// once both exited, the strong task starts there.
	s := newStrongPool(t, true, gpuPolicy{packNUMA: true}, map[aproto.ID]topologyFixture{"a": node02},
		strongTask{id: "r0", slots: 1, priority: 50, running: []int{0}, nonPreemptible: true},
		strongTask{id: "r1", slots: 1, priority: 50, running: []int{4}},
		strongTask{id: "r2", slots: 1, priority: 50, running: []int{5}},
		strongTask{id: "strong", slots: 4, priority: 10, strong: true},
	)
	allocated, released, _ := s.pass(t)
	require.Empty(t, allocated)
	require.ElementsMatch(t, []model.AllocationID{"r1", "r2"}, released)

	s.exit(t, "r2")
	allocated, released, _ = s.pass(t)
	require.Empty(t, allocated)
	require.Equal(t, []model.AllocationID{"r1"}, released)

	s.exit(t, "r1")
	allocated, released, again := s.pass(t)
	require.Equal(t, map[string][]int{"strong": {4, 5, 6, 7}}, allocated)
	require.Empty(t, released)
	require.False(t, again)
}

func TestStrongPlanMatchesTheReservations(t *testing.T) {
	// Under best with packing on and no preemption, the live reservations of the requests that a
	// pass plans, in the order the pass returns them, choose the GPUs that the simulation
	// (addTaskToAgents) chooses on copies of the agents with the pass's policy, for strong, soft
	// and plain tasks.
	agents := map[aproto.ID]topologyFixture{"a": node01Widths, "b": node07}
	var tasks []strongTask
	modes := []expconf.GPUTopologyPreference{"", "strong", "", "strong", "", "soft", "", "strong"}
	for i, n := range []int{1, 2, 1, 3, 4, 2, 1, 2} {
		tasks = append(tasks, strongTask{
			id: fmt.Sprintf("task-%d", i), slots: n, priority: 10 + 32*(i%2), strong: modes[i] == "strong",
		})
	}
	s := newStrongPool(t, false, gpuPolicy{packNUMA: true, xids: map[string]bool{gpuDevice(1).UUID: true}},
		agents, tasks...)
	soft, ok := s.rp.taskList.TaskByID("task-5")
	require.True(t, ok)
	soft.FittingRequirements.GPUTopology = expconf.GPUTopologySoft

	s.rp.mu.Lock()
	s.rp.agentStatesCache = s.live
	s.rp.gpuPolicy = s.policy
	toAllocate, toRelease := s.rp.scheduler.Schedule(s.rp)
	s.rp.mu.Unlock()
	require.Empty(t, toRelease)
	require.NotEmpty(t, toAllocate)

	// The simulation (addTaskToAgents) on copies of the agents with the pass's policy plans each
	// request's GPUs; the live reservation must choose the same.
	copies := deepCopyAgents(s.live)
	simulation := priorityScheduler{gpus: s.policy}
	strongPlanned := 0
	for _, req := range toAllocate {
		if strongTopology(req) {
			strongPlanned++
		}
		fits := findFits(req, s.live, s.rp.fittingMethod, false, s.policy.packNUMA)
		require.Len(t, fits, 1)
		copyFits := findFits(req, copies, s.rp.fittingMethod, false, s.policy.packNUMA)
		require.Len(t, copyFits, 1)
		require.Equal(t, fits[0].Agent.id, copyFits[0].Agent.id)
		before := freeDeviceIDs(copyFits[0].Agent)
		require.True(t, simulation.addTaskToAgents(req, copyFits), "request %s", req.AllocationID)
		planned := idsMinus(before, freeDeviceIDs(copyFits[0].Agent))

		res, err := fits[0].Agent.allocateFreeDevices(fits[0].Slots, cproto.NewID(), s.policy.selection(req, fits))
		require.NoError(t, err)
		require.Equal(t, planned, deviceIDs(res.devices), "request %s", req.AllocationID)
		if strongTopology(req) {
			require.NoError(t, fits[0].Agent.checkOneNUMANode(res.devices))
		}
	}
	require.Greater(t, strongPlanned, 1)
}

// awaitEvent returns the next event of the subscription, or nil if none arrives in wait.
func awaitEvent(t *testing.T, sub *sproto.ResourcesSubscription, wait time.Duration) sproto.ResourcesEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	event, err := sub.GetWithContext(ctx)
	if err != nil {
		return nil
	}
	return event
}

func TestStrongCheckAfterThePass(t *testing.T) {
	// GPUs 0 and 4 are busy. A strong 4-GPU task waits, and the task log says so once. A strong
	// 5-GPU task can never fit: it fails once, with a cause that ends a trial without restarts.
	s := newStrongPool(t, false, gpuPolicy{packNUMA: true}, map[aproto.ID]topologyFixture{"a": node02},
		strongTask{id: "busy0", slots: 1, priority: 42, running: []int{0}},
		strongTask{id: "busy4", slots: 1, priority: 42, running: []int{4}},
	)
	four, five := s.addStrong(t, "four", 4), s.addStrong(t, "five", 5)

	allocated, _, again := s.pass(t)
	require.Empty(t, allocated)
	require.False(t, again)
	event := awaitEvent(t, four, time.Second)
	require.IsType(t, &sproto.ContainerLog{}, event)
	require.Equal(t, "GPU topology preference strong: waiting until one NUMA node of an agent in pool "+
		"gpus has 4 free GPUs", event.(*sproto.ContainerLog).Message())
	require.Equal(t, model.LogLevelInfo, *event.(*sproto.ContainerLog).Level)
	event = awaitEvent(t, five, time.Second)
	require.IsType(t, &sproto.InvalidResourcesRequestError{}, event)
	cause := event.(*sproto.InvalidResourcesRequestError).Cause
	require.True(t, sproto.IsUnrecoverableSystemError(cause), "%T", cause)
	require.EqualError(t, cause, "invalid resources request: no NUMA node in pool gpus has 5 slots; use soft")

	// Once per task: further passes say nothing more.
	for i := 0; i < 2; i++ {
		_, _, again = s.pass(t)
		require.False(t, again)
	}
	require.Nil(t, awaitEvent(t, four, 2*actionCoolDown))
	require.Nil(t, awaitEvent(t, five, 2*actionCoolDown))

	// The failed task leaves the task list, as its release does, and its notice goes.
	s.exit(t, "busy4")
	s.rp.mu.Lock()
	s.rp.taskList.RemoveTaskByID("five")
	s.rp.mu.Unlock()
	allocated, _, again = s.pass(t)
	require.Equal(t, map[string][]int{"four": {4, 5, 6, 7}}, allocated)
	require.False(t, again)
	require.Equal(t, map[model.AllocationID]strongNotice{"four": strongWaiting}, s.rp.strongNotices)

	// A strong task that fits after a pass that reserved asks for another; one after a pass that
	// reserved nothing does not.
	s.addStrong(t, "two", 2)
	s.rp.mu.Lock()
	s.rp.agentStatesCache = s.live
	require.True(t, s.rp.checkStrongRequests(true))
	require.False(t, s.rp.checkStrongRequests(false))
	s.rp.mu.Unlock()
}

func TestStrongWaitsForAgentsThatHaveNotReported(t *testing.T) {
	// After a master restart, an agent restored from its snapshot has no topology until its
	// AgentStarted: a strong task that no NUMA node of the other agents can hold waits.
	s := newStrongPool(t, false, gpuPolicy{packNUMA: true},
		map[aproto.ID]topologyFixture{"a": node02, "b": node02})
	s.live["b"].gpuTopology = nil
	sub := s.addStrong(t, "five", 5)
	s.pass(t)
	require.IsType(t, &sproto.ContainerLog{}, awaitEvent(t, sub, time.Second))

	s.live["b"].gpuTopology = node02.build()
	s.pass(t)
	require.IsType(t, &sproto.InvalidResourcesRequestError{}, awaitEvent(t, sub, time.Second))
}
