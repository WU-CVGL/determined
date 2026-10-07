package agentrm

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"golang.org/x/exp/maps"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
)

const defaultResourcePoolName = "default"

type slotEnabled struct {
	deviceAdded  bool
	agentEnabled bool
	userEnabled  bool
	draining     bool
}

func (s slotEnabled) enabled() bool {
	return s.agentEnabled && s.userEnabled
}

// allocatable reports whether the slot takes new work: it is enabled and not draining.
func (s slotEnabled) allocatable() bool {
	return s.enabled() && !s.draining
}

type slot struct {
	device      device.Device
	enabled     slotEnabled
	containerID *cproto.ID
}

// agentState holds the scheduler state for an agent. The implementation of agent-related operations
// (e.g., socket I/O) is deferred to the actor.
type agentState struct {
	syslog *log.Entry

	id               aproto.ID
	handler          *agent
	Devices          map[device.Device]*cproto.ID
	resourcePoolName string
	enabled          bool
	draining         bool
	uuid             uuid.UUID

	maxZeroSlotContainers int

	slotStates          map[device.ID]*slot
	containerAllocation map[cproto.ID]model.AllocationID
	containerState      map[cproto.ID]*cproto.Container

	// gpuTopology is what the agent reported at its last start. It is nil after a restore from the
	// snapshot until the agent's AgentStarted arrives. It is never mutated, only replaced
	// (setGPUTopology), so copies share it (deepCopy); it stays out of device.Device and the
	// snapshot.
	gpuTopology *gpuTopology
}

// newAgentState returns a new agent empty agent state backed by the handler.
// TODO(DET-9977): It is error-prone that we can new up an agentState is invalid / would cause panics.
func newAgentState(id aproto.ID, maxZeroSlotContainers int) *agentState {
	return &agentState{
		syslog:                log.WithField("component", "agent-state-state").WithField("id", id),
		id:                    id,
		Devices:               make(map[device.Device]*cproto.ID),
		maxZeroSlotContainers: maxZeroSlotContainers,
		enabled:               true,
		slotStates:            make(map[device.ID]*slot),
		containerAllocation:   make(map[cproto.ID]model.AllocationID),
		containerState:        make(map[cproto.ID]*cproto.Container),
		uuid:                  uuid.New(),
	}
}

func (a *agentState) string() string {
	return string(a.id)
}

func (a *agentState) agentID() aproto.ID {
	return a.id
}

// numSlots returns the total number of slots available.
func (a *agentState) numSlots() int {
	switch {
	case a.draining:
		return a.numUsedSlots()
	case !a.enabled:
		return 0
	default:
		return len(a.Devices)
	}
}

// numEmptySlots returns the number of slots that have not been allocated to containers.
func (a *agentState) numEmptySlots() (slots int) {
	switch {
	case a.draining, !a.enabled:
		return 0
	default:
		return a.numSlots() - a.numUsedSlots()
	}
}

// numUsedSlots returns the number of slots that have been allocated to containers.
func (a *agentState) numUsedSlots() (slots int) {
	for _, id := range a.Devices {
		if id != nil {
			slots++
		}
	}
	return slots
}

// numUsedZeroSlots returns the number of allocated zero-slot units.
func (a *agentState) numUsedZeroSlots() int {
	result := 0
	for _, container := range a.containerState {
		if len(container.Devices) == 0 {
			result++
		}
	}

	return result
}

// numZeroSlots returns the total number of zero-slot units.
func (a *agentState) numZeroSlots() int {
	switch {
	case a.draining:
		return a.numUsedZeroSlots()
	case !a.enabled:
		return 0
	default:
		return a.maxZeroSlotContainers
	}
}

// numEmptyZeroSlots returns the number of unallocated zero-slot units.
func (a *agentState) numEmptyZeroSlots() int {
	switch {
	case a.draining || !a.enabled:
		return 0
	default:
		return a.numZeroSlots() - a.numUsedZeroSlots()
	}
}

// idle signals if the agent is idle.
func (a *agentState) idle() bool {
	return a.numUsedZeroSlots() == 0 && a.numUsedSlots() == 0
}

// deviceReservation is what allocateFreeDevices reserved, and how it chose the devices.
type deviceReservation struct {
	devices []device.Device
	// choice is the selection's result; its devices are the reserved ones, or nil for map order.
	choice gpuChoice
	// failure is why a selection was dropped for map order: its set was invalid, it gave neither a
	// set nor a reason, or it panicked.
	failure string
}

// allocateFreeDevices reserves slots devices for the container cid. Its one change of state comes
// last: chooseFreeDevices picks a full set and validates it whole without changing anything, then
// every device of the set is reserved at once. With fewer than slots free devices, or for
// prefer_gpu_topology "strong" without a set on one NUMA node, it returns an error and changes
// nothing. A zero-slot container takes no devices, and the topology is never
// read. The scheduler's copies run this too, so it must not log: a copy has no syslog.
func (a *agentState) allocateFreeDevices(
	slots int, cid cproto.ID, sel deviceSelection,
) (deviceReservation, error) {
	// TODO(ilia): Rename to AllocateContainer.
	if slots == 0 {
		a.containerState[cid] = &cproto.Container{ID: cid}
		return deviceReservation{}, nil
	}
	res, err := a.chooseFreeDevices(slots, sel, selectFreeDevices)
	if err != nil {
		return deviceReservation{}, err
	}
	for _, d := range res.devices {
		a.Devices[d] = &cid
	}
	a.containerState[cid] = &cproto.Container{ID: cid, Devices: res.devices}
	return res, nil
}

// chooseFreeDevices returns a full set of slots free devices, validated whole (checkFreeDevices),
// and how it was chosen; it changes nothing. When sel ranks, the selection (selector:
// selectFreeDevices, or a fake in tests) runs first and its set is taken if it is valid. Otherwise
// the set is the free devices in map order, as before GPU selection existed, and goes through the
// same validation:
//   - for the zero selection, which never runs the selection;
//   - when the selection chooses no devices and says why (gpuChoice.mapOrder);
//   - when the selection fails: its set is invalid, it gives neither a set nor a reason, or it
//     panics. The reservation reports the failure, so it succeeds exactly when one in map order
//     would.
//
// Only the selection runs under recover: it has no side effects. prefer_gpu_topology "strong"
// never takes map order (chooseOnOneNUMANode).
func (a *agentState) chooseFreeDevices(
	slots int, sel deviceSelection, selector func(gpuSelectionInput, int, deviceSelection) gpuChoice,
) (deviceReservation, error) {
	if a.numFreeDevices() < slots {
		return deviceReservation{}, errors.New("not enough devices")
	}
	if sel.strong {
		return a.chooseOnOneNUMANode(slots, sel, selector)
	}
	var res deviceReservation
	if sel.ranks() {
		res.choice, res.failure = a.selectRankedDevices(slots, sel, selector)
		switch {
		case res.failure != "":
			// It panicked.
		case res.choice.devices != nil:
			err := a.checkFreeDevices(res.choice.devices, slots)
			if err == nil {
				res.devices = res.choice.devices
				return res, nil
			}
			res.failure = err.Error()
		case res.choice.mapOrder == "":
			res.failure = "no devices and no reason"
		}
		if res.failure != "" {
			res.choice = gpuChoice{}
		}
	}

	// The one fallback to map order, for the zero selection and for a plain or "soft" selection
	// that chooses no set or fails; prefer_gpu_topology "strong" never gets here
	// (chooseOnOneNUMANode). Every path selects a full set and validates it whole;
	// allocateFreeDevices changes state once, after the validation.
	devices := a.mapOrderDevices(slots)
	if err := a.checkFreeDevices(devices, slots); err != nil {
		return deviceReservation{}, err
	}
	res.devices = devices
	return res, nil
}

// chooseOnOneNUMANode is chooseFreeDevices for prefer_gpu_topology "strong": the selection's
// (selector's) set, validated whole and on one known NUMA node, or an error that changes nothing.
// It never takes map order: the selection's reason for choosing nothing, its failure and an invalid
// set are errors. Its fit admits only agents where the selection chooses a set
// (holdsOnOneNUMANode), so an error means the agent changed after the fit; the scheduler's
// simulation counts it as a miss (addTaskToAgents).
func (a *agentState) chooseOnOneNUMANode(
	slots int, sel deviceSelection, selector func(gpuSelectionInput, int, deviceSelection) gpuChoice,
) (deviceReservation, error) {
	choice, failure := a.selectRankedDevices(slots, sel, selector)
	switch {
	case failure != "":
		return deviceReservation{}, fmt.Errorf("GPU topology preference strong: selection failed: %s", failure)
	case choice.devices == nil && choice.mapOrder == "":
		return deviceReservation{}, errors.New("GPU topology preference strong: no devices and no reason")
	case choice.devices == nil:
		return deviceReservation{}, fmt.Errorf("GPU topology preference strong: %s", choice.mapOrder)
	}
	if err := a.checkFreeDevices(choice.devices, slots); err != nil {
		return deviceReservation{}, fmt.Errorf("GPU topology preference strong: %w", err)
	}
	if err := a.checkOneNUMANode(choice.devices); err != nil {
		return deviceReservation{}, fmt.Errorf("GPU topology preference strong: %w", err)
	}
	return deviceReservation{devices: choice.devices, choice: choice}, nil
}

// checkOneNUMANode validates the set of prefer_gpu_topology "strong": every device on one known
// NUMA node.
func (a *agentState) checkOneNUMANode(devices []device.Device) error {
	node := numaNodeOf(a.gpuTopology, devices[0])
	for _, d := range devices {
		switch n := numaNodeOf(a.gpuTopology, d); {
		case n < 0:
			return fmt.Errorf("selected device %d has no known NUMA node", d.ID)
		case n != node:
			return fmt.Errorf("selected devices on NUMA nodes %d and %d", node, n)
		}
	}
	return nil
}

// slotsByNUMA counts the agent's slots with a known NUMA node, by node, whether they take new work
// or not; a device without a slot state counts too.
func (a *agentState) slotsByNUMA() map[int]int {
	out := map[int]int{}
	count := func(d device.Device) {
		if node := numaNodeOf(a.gpuTopology, d); node >= 0 {
			out[node]++
		}
	}
	for _, s := range a.slotStates {
		count(s.device)
	}
	for d := range a.Devices {
		if _, ok := a.slotStates[d.ID]; !ok {
			count(d)
		}
	}
	return out
}

// mapOrderDevices returns up to slots free devices in the order of the Devices map.
func (a *agentState) mapOrderDevices(slots int) []device.Device {
	devices := make([]device.Device, 0, slots)
	for d, cid := range a.Devices {
		if len(devices) == slots {
			break
		}
		if cid == nil {
			devices = append(devices, d)
		}
	}
	return devices
}

// numFreeDevices returns the number of devices without a container.
func (a *agentState) numFreeDevices() int {
	free := 0
	for _, cid := range a.Devices {
		if cid == nil {
			free++
		}
	}
	return free
}

// selectRankedDevices runs the selection, recovering a panic as a failure.
func (a *agentState) selectRankedDevices(
	slots int, sel deviceSelection, selector func(gpuSelectionInput, int, deviceSelection) gpuChoice,
) (c gpuChoice, failure string) {
	defer func() {
		if r := recover(); r != nil {
			c, failure = gpuChoice{}, fmt.Sprintf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return selector(a.gpuSelectionInput(), slots, sel), ""
}

// gpuSelectionInput returns what a GPU selection reads of the agent.
func (a *agentState) gpuSelectionInput() gpuSelectionInput {
	in := gpuSelectionInput{topology: a.gpuTopology}
	for d, cid := range a.Devices {
		if cid == nil {
			in.free = append(in.free, d)
		}
		if s, ok := a.slotStates[d.ID]; !ok || s.enabled.allocatable() {
			in.allocatable = append(in.allocatable, d)
		}
	}
	sort.Slice(in.free, func(i, j int) bool { return in.free[i].ID < in.free[j].ID })
	sort.Slice(in.allocatable, func(i, j int) bool { return in.allocatable[i].ID < in.allocatable[j].ID })
	return in
}

// checkFreeDevices validates a selection: exactly slots devices, no duplicates, each a free device
// of the agent.
func (a *agentState) checkFreeDevices(devices []device.Device, slots int) error {
	if len(devices) != slots {
		return fmt.Errorf("selected %d devices for %d slots", len(devices), slots)
	}
	seen := map[device.Device]bool{}
	for _, d := range devices {
		cid, ok := a.Devices[d]
		switch {
		case seen[d]:
			return fmt.Errorf("selected device %d twice", d.ID)
		case !ok:
			return fmt.Errorf("selected device %d is not a device of the agent", d.ID)
		case cid != nil:
			return fmt.Errorf("selected device %d is in use", d.ID)
		}
		seen[d] = true
	}
	return nil
}

// deallocateContainer deallocates containers.
func (a *agentState) deallocateContainer(id cproto.ID) {
	delete(a.containerState, id)
	for d, cid := range a.Devices {
		if cid != nil && *cid == id {
			a.freeDevice(d)
		}
	}
}

// freeDevice marks d free. If its slot no longer takes new work (it was drained while d was in
// use), d leaves Devices, as the device of a disabled slot does at once. This also runs on the
// scheduler's copies, so it must not log: a copy has no syslog.
func (a *agentState) freeDevice(d device.Device) {
	a.Devices[d] = nil
	if s, ok := a.slotStates[d.ID]; ok && !s.enabled.allocatable() {
		s.enabled.deviceAdded = false
		delete(a.Devices, d)
	}
}

// deepCopy returns a copy of agentState for scheduler internals. Each copy gets its own slot
// states, since the scheduler's simulation frees devices on it. It shares gpuTopology, which is
// never mutated, only replaced, so the simulation selects GPUs on the copies from the inputs the
// live reservation reads on the agent. A ranked selection (NUMA packing, "soft" when it ranks,
// "strong") is deterministic: with the pass's policy and the same placements in the same order on
// unchanged agents (no preemption, every reservation succeeding), the two choose the same devices
// while every earlier placement on the agent in the pass ranked too, as under NUMA packing. Map
// order gives no such guarantee: a map-order placement can make the agent's later choices differ,
// ranked ones included. Fits use counts only, so a difference never changes which tasks fit, except
// for prefer_gpu_topology "strong", whose fit counts the free GPUs of each NUMA node; the check
// after the pass then asks for one more pass (checkStrongRequests).
func (a *agentState) deepCopy() *agentState {
	copiedAgent := &agentState{
		id:                    a.id,
		handler:               a.handler,
		Devices:               maps.Clone(a.Devices),
		maxZeroSlotContainers: a.maxZeroSlotContainers,
		enabled:               a.enabled,
		draining:              a.draining,
		containerState:        maps.Clone(a.containerState),
		// The scheduler's simulation frees devices on its copies (freeDevice), which updates
		// slot states, so a copy needs its own.
		slotStates:       make(map[device.ID]*slot, len(a.slotStates)),
		resourcePoolName: a.resourcePoolName,
		gpuTopology:      a.gpuTopology,
	}
	for id, s := range a.slotStates {
		copied := *s
		copiedAgent.slotStates[id] = &copied
	}

	return copiedAgent
}

// enable enables the agent.
func (a *agentState) enable() {
	a.syslog.Infof("enabling agent: %s", a.string())
	a.enabled = true
	a.draining = false
}

// disable disables or drains the agent.
func (a *agentState) disable(drain bool) {
	drainStr := "disabling"
	if drain {
		drainStr = "draining"
	}
	a.syslog.Infof("%s agent: %s", drainStr, a.string())
	a.draining = drain
	a.enabled = false
}

func (a *agentState) addDevice(device device.Device, containerID *cproto.ID) {
	a.syslog.Infof("adding device: %s on %s", device.String(), a.string())
	a.Devices[device] = containerID
}

func (a *agentState) removeDevice(device device.Device) {
	a.syslog.Infof("removing device: %s (%s)", device.String(), a.string())
	delete(a.Devices, device)
}

// agentStarted initializes slots from AgentStarted.Devices.
func (a *agentState) agentStarted(agentStarted *aproto.AgentStarted) {
	msg := agentStarted
	for _, d := range msg.Devices {
		enabled := slotEnabled{
			agentEnabled: true,
			userEnabled:  true,
		}
		a.slotStates[d.ID] = &slot{enabled: enabled, device: d}
		a.updateSlotDeviceView(d.ID)
	}

	if err := a.persist(); err != nil {
		a.syslog.Warnf("agentStarted persist failure")
	}
}

// setGPUTopology replaces the agent's GPU topology with the one from its latest AgentStarted.
func (a *agentState) setGPUTopology(g *gpuTopology) {
	a.gpuTopology = g
}

func (a *agentState) checkAgentStartedDevicesMatch(
	agentStarted *aproto.AgentStarted,
) error {
	ourDevices := map[device.ID]device.Device{}
	for did, slot := range a.slotStates {
		ourDevices[did] = slot.device
	}

	theirDevices := map[device.ID]device.Device{}
	for _, d := range agentStarted.Devices {
		theirDevices[d.ID] = d
	}

	if len(ourDevices) != len(theirDevices) {
		return fmt.Errorf("device count has changed: %d -> %d", len(ourDevices), len(theirDevices))
	}

	if !maps.Equal(ourDevices, theirDevices) {
		for k := range ourDevices {
			if ourDevices[k] != theirDevices[k] {
				return fmt.Errorf(
					"device properties have changed: %v -> %v",
					ourDevices[k],
					theirDevices[k],
				)
			}
		}
		return fmt.Errorf("devices has changed") // This should not happen!
	}

	return nil
}

// We need to compare the resource pool configurations between the agent and the master.
// Ideally, the master doesn't request new resource pool information if the agent reconnects
// within the designated reconnection period, while the agent should read from its updated configuration.
// If there's a mismatch, an error will be thrown, causing the agent to stop and require a restart.
func (a *agentState) checkAgentResourcePoolMatch(
	agentStarted *aproto.AgentStarted,
) error {
	if a.resourcePoolName != agentStarted.ResourcePoolName {
		return fmt.Errorf("resource pool has changed: %s -> %s", a.resourcePoolName, agentStarted.ResourcePoolName)
	}

	return nil
}

func (a *agentState) containerStateChanged(msg aproto.ContainerStateChanged) {
	for _, d := range msg.Container.Devices {
		s, ok := a.slotStates[d.ID]
		if !ok {
			a.syslog.Warnf("bad containerStateChanged on device: %d (%s)", d.ID, a.string())
			continue
		}

		s.containerID = &msg.Container.ID

		if msg.Container.State == cproto.Terminated {
			s.containerID = nil
		}
	}

	// containerState holds the containers the pool has not released: allocateFreeDevices,
	// startContainer and the restore from a snapshot add them, and deallocateContainer and the
	// Terminated report remove them. A report updates a container held there but does not add one.
	// The pool releases a container without waiting for it to terminate, so the agent can still
	// report it, for example Running, after the release. Holding it again would bring its device
	// back in use when its slot is enabled, and nothing would free the device after that.
	// One exception remains: a StartTaskContainer buffered while the agent was disconnected is
	// replayed on reconnect through startContainer, which adds its container again even if the pool
	// released it in the meantime. That container then stays here until its Terminated report.
	if msg.Container.State == cproto.Terminated {
		delete(a.containerState, msg.Container.ID)
	} else if _, ok := a.containerState[msg.Container.ID]; ok {
		a.containerState[msg.Container.ID] = &msg.Container
	}

	if err := a.persist(); err != nil {
		a.syslog.WithError(err).Warnf("containerStateChanged persist failure")
	}

	if err := updateContainerState(&msg.Container); err != nil {
		a.syslog.WithError(err).Warnf("containerStateChanged failed to update container state")
	}
}

func (a *agentState) startContainer(msg sproto.StartTaskContainer) error {
	inner := func(deviceId device.ID) error {
		s, ok := a.slotStates[deviceId]
		if !ok {
			return errors.New("can't find slot")
		}

		// TODO(ilia): Potential race condition if slot is disabled in-between scheduling?
		if !s.enabled.enabled() {
			return errors.New("container allocated but slot is not enabled")
		}
		if s.containerID != nil {
			return errors.New("container already allocated to slot")
		}

		s.containerID = &msg.StartContainer.Container.ID
		a.containerState[msg.StartContainer.Container.ID] = &msg.StartContainer.Container

		return nil
	}

	for _, d := range msg.StartContainer.Container.Devices {
		if err := inner(d.ID); err != nil {
			return errors.Wrapf(err, "bad startContainer on device: %d (%s)", d.ID, a.string())
		}
	}

	a.containerAllocation[msg.Container.ID] = msg.AllocationID

	if err := a.persist(); err != nil {
		a.syslog.WithError(err).Warnf("startContainer persist failure")
	}

	if err := updateContainerState(&msg.StartContainer.Container); err != nil {
		a.syslog.WithError(err).Warnf("startContainer failed to update container state")
	}

	return nil
}

func (a *agentState) getSlotsSummary(baseAddress string) model.SlotsSummary {
	summary := make(model.SlotsSummary, len(a.slotStates))
	for deviceID := range a.slotStates {
		summary[fmt.Sprintf("%s/slots/%d", baseAddress, deviceID)] = a.getSlotSummary(
			deviceID,
		)
	}

	return summary
}

func (a *agentState) getSlotSummary(deviceID device.ID) model.SlotSummary {
	s := a.slotStates[deviceID]
	cid := s.containerID
	var container *cproto.Container
	if cid != nil {
		container = a.containerState[*cid]
	}

	return model.SlotSummary{
		ID:        strconv.Itoa(int(s.device.ID)),
		Device:    s.device,
		Enabled:   s.enabled.enabled(),
		Container: container,
		Draining:  s.enabled.draining,
	}
}

func (a *agentState) updateSlotDeviceView(deviceID device.ID) {
	s, ok := a.slotStates[deviceID]
	if !ok {
		a.syslog.
			Warnf("bad updateSlotDeviceView on device: %d (%s): not found", deviceID, a.string())
		return
	}

	// TODO(ilia): Don't materialize `Devices` view on slots.
	// Devices holds the slots that take new work and the devices still in use. A draining slot
	// keeps its device while a container holds it, and the device leaves once it is free, here or
	// in freeDevice. So the free entries of Devices, which every allocation path counts and takes
	// from, are exactly the enabled, not draining, free slots.
	if s.enabled.allocatable() && !s.enabled.deviceAdded {
		s.enabled.deviceAdded = true

		// The device comes back in use only while the pool still holds its container. Once the
		// pool has released it (deallocateContainer) or it terminated, the device is free:
		// slot.containerID can outlive the release until the agent reports the container
		// terminated, and nothing would free the device after that. A report that arrives after
		// the release does not put the container back in containerState (containerStateChanged,
		// which also notes the one path that still does).
		cid := s.containerID
		if cid != nil {
			if _, ok := a.containerState[*cid]; !ok {
				cid = nil
			}
		}
		a.addDevice(s.device, cid)
	} else if !s.enabled.allocatable() {
		if s.enabled.deviceAdded && (!s.enabled.draining || a.Devices[s.device] == nil) {
			s.enabled.deviceAdded = false
			a.removeDevice(s.device)
		}

		// On `PostStop`, draining will be already set to false, and we'll kill the container
		// whether we have the device or not.
		if !s.enabled.draining && s.containerID != nil {
			rmevents.Publish(a.containerAllocation[*s.containerID], &sproto.ReleaseResources{
				Reason:    "slot disabled",
				ForceKill: true,
			})
		}
	}
}

func (a *agentState) patchSlotStateInner(
	msg patchSlotState, slotState *slot,
) model.SlotSummary {
	if msg.enabled != nil {
		slotState.enabled.userEnabled = *msg.enabled
		if *msg.enabled {
			// Enabling a slot ends its drain.
			slotState.enabled.draining = false
		}
	}
	if msg.drain != nil {
		slotState.enabled.draining = *msg.drain
	}
	a.updateSlotDeviceView(slotState.device.ID)

	return a.getSlotSummary(slotState.device.ID)
}

func (a *agentState) patchAllSlotsState(
	msg patchAllSlotsState,
) model.SlotsSummary {
	result := model.SlotsSummary{}
	for _, slotState := range a.slotStates {
		summary := a.patchSlotStateInner(patchSlotState{
			id:      slotState.device.ID, // Note: this is effectively unused.
			enabled: msg.enabled,
			drain:   msg.drain,
		}, slotState)
		result[summary.ID] = summary
	}
	return result
}

func (a *agentState) patchSlotState(
	msg patchSlotState,
) (model.SlotSummary, error) {
	s, ok := a.slotStates[msg.id]
	if !ok {
		return model.SlotSummary{}, fmt.Errorf(
			"bad updateSlotDeviceView on device: %d (%s): not found",
			msg.id,
			a.string(),
		)
	}
	return a.patchSlotStateInner(msg, s), nil
}

func (a *agentState) snapshot() *agentSnapshot {
	slots := make([]slotData, 0, len(a.slotStates))
	for _, slotState := range a.slotStates {
		slots = append(slots, slotData{
			Device:      slotState.device,
			UserEnabled: slotState.enabled.userEnabled,
			ContainerID: slotState.containerID,
		})
	}

	containerIds := maps.Keys(a.containerState)

	s := agentSnapshot{
		AgentID:          a.agentID(),
		UUID:             a.uuid.String(),
		ResourcePoolName: a.resourcePoolName,
		// TODO(ilia): we need to disambiguate user setting (which needs to be saved)
		// vs current state.
		UserEnabled:           a.enabled,
		UserDraining:          a.draining,
		MaxZeroSlotContainers: a.maxZeroSlotContainers,
		Slots:                 slots,
		Containers:            containerIds,
	}

	return &s
}

func (a *agentState) persist() error {
	snapshot := a.snapshot()
	_, err := db.Bun().NewInsert().Model(snapshot).
		On("CONFLICT (uuid) DO UPDATE").
		On("CONFLICT (agent_id) DO UPDATE").
		Exec(context.TODO())
	return err
}

func (a *agentState) delete() error {
	_, err := db.Bun().NewDelete().Model((*agentSnapshot)(nil)).
		Where("agent_id = ?", a.id).
		Exec(context.TODO())
	return err
}

func (a *agentState) clearUnlessRecovered(
	recovered map[cproto.ID]aproto.ContainerReattachAck,
) error {
	updated := false
	for d := range a.Devices {
		if cID := a.Devices[d]; cID != nil {
			_, ok := recovered[*cID]
			if !ok {
				a.slotStates[d.ID].containerID = nil
				a.freeDevice(d)
				updated = true
			}
		}
	}

	for _, slot := range a.slotStates {
		if slot.containerID != nil {
			_, ok := recovered[*slot.containerID]
			if !ok {
				slot.containerID = nil
				updated = true
			}
		}
	}

	for cid := range a.containerState {
		_, ok := recovered[cid]
		if !ok {
			delete(a.containerState, cid)
			updated = true
		}
	}

	for cid := range a.containerAllocation {
		_, ok := recovered[cid]
		if !ok {
			delete(a.containerAllocation, cid)
			updated = true
		}
	}

	if updated {
		return a.persist()
	}

	return nil
}

// retrieveAgentStates reconstructs AgentStates from the database for all resource pools that
// have agent_container_reattachment enabled.
func retrieveAgentStates() (map[aproto.ID]agentState, error) {
	var snapshots []agentSnapshot
	if err := db.Bun().NewSelect().Model(&snapshots).Scan(context.TODO()); err != nil {
		return nil, fmt.Errorf("selecting agent snapshost: %w", err)
	}

	result := make(map[aproto.ID]agentState, len(snapshots))
	for _, s := range snapshots {
		state, err := newAgentStateFromSnapshot(s)
		if err != nil {
			return nil, fmt.Errorf("failed to recreate agent state %s: %w", s.AgentID, err)
		}
		result[s.AgentID] = *state
	}
	return result, nil
}

func newAgentStateFromSnapshot(as agentSnapshot) (*agentState, error) {
	parsedUUID, err := uuid.Parse(as.UUID)
	if err != nil {
		return nil, err
	}

	slotStates := make(map[device.ID]*slot)
	devices := make(map[device.Device]*cproto.ID)

	for _, sd := range as.Slots {
		slotStates[sd.Device.ID] = &slot{
			device:      sd.Device,
			containerID: sd.ContainerID,
			enabled: slotEnabled{
				deviceAdded:  true,
				agentEnabled: as.UserEnabled,
				userEnabled:  as.UserEnabled,
				draining:     as.UserDraining,
			},
		}
		if sd.ContainerID != nil {
			devices[sd.Device] = sd.ContainerID
		} else {
			devices[sd.Device] = nil
		}
	}

	containerState := make(map[cproto.ID]*cproto.Container)

	if len(as.Containers) > 0 {
		containerSnapshots := make([]containerSnapshot, 0, len(as.Containers))
		err := db.Bun().NewSelect().Model(&containerSnapshots).
			Where("container_id IN (?)", bun.In(as.Containers)).
			Scan(context.TODO())
		if err != nil {
			return nil, err
		}

		for _, containerSnapshot := range containerSnapshots {
			container := containerSnapshot.ToContainer()
			containerState[container.ID] = &container
		}
	}

	result := agentState{
		id:                    as.AgentID,
		syslog:                log.WithField("component", "agent-state").WithField("id", as.AgentID),
		maxZeroSlotContainers: as.MaxZeroSlotContainers,
		resourcePoolName:      as.ResourcePoolName,
		uuid:                  parsedUUID,
		enabled:               as.UserEnabled,
		draining:              as.UserDraining,
		slotStates:            slotStates,
		Devices:               devices,
		containerAllocation:   make(map[cproto.ID]model.AllocationID),
		containerState:        containerState,
	}

	return &result, nil
}

func (a *agentState) restoreContainersField() error {
	containerIDs := maps.Keys(a.containerState)

	res, err := loadContainersToAllocationIds(containerIDs)
	if err != nil {
		return err
	}
	a.containerAllocation = res

	a.syslog.Debugf("restored %d/%d container to allocation mappings", len(res), len(containerIDs))
	return nil
}

func clearAgentStates(agentIds []aproto.ID) error {
	if _, err := db.Bun().NewDelete().Model((*agentSnapshot)(nil)).
		Where("agent_id in (?)", bun.In(agentIds)).
		Exec(context.TODO()); err != nil {
		return fmt.Errorf("clearing agent states: %w", err)
	}

	return nil
}

func updateContainerState(c *cproto.Container) error {
	snapshot := newContainerSnapshot(c)
	_, err := db.Bun().NewUpdate().
		Model(&snapshot).
		Where("container_id = ?", snapshot.ID).
		Column("state", "devices").
		Exec(context.TODO())

	return err
}

func loadContainersToAllocationIds(
	containerIDs []cproto.ID,
) (map[cproto.ID]model.AllocationID, error) {
	cs := []containerSnapshot{}
	result := []map[string]interface{}{}
	rr := map[cproto.ID]model.AllocationID{}

	if len(containerIDs) == 0 {
		return rr, nil
	}

	err := db.Bun().NewSelect().Model(&cs).
		Join("JOIN allocation_resources al_res ON al_res.resource_id = rmac.resource_id").
		Where("container_id IN (?)", bun.In(containerIDs)).
		Column("container_id", "allocation_id").
		Scan(context.TODO(), &result)
	if err != nil {
		return nil, err
	}

	for _, row := range result {
		rr[cproto.ID(row["container_id"].(string))] = model.AllocationID(
			row["allocation_id"].(string),
		)
	}

	return rr, nil
}
