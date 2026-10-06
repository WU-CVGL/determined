package prom

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/determined-ai/determined/master/pkg/schemas/expconf"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/model"

	"github.com/determined-ai/determined/master/pkg/device"
)

// mapping is a gauge vector of label mappings in which each label set counts its associations.
// The gauge of a label set holds that count, as before, so the label set can be associated more
// than once: an allocation with one association per container, or an experiment with one per
// running trial. The label set is deleted with its last association instead of staying exported
// as a 0-valued series until the master restarts.
//
// Deleting at zero needs the count under a lock either way: reading the gauge back after Dec
// would race a concurrent Inc of the same label set. Keeping the counts here also means that a
// removal without a matching association changes nothing instead of exporting a negative value.
type mapping struct {
	vec    *prometheus.GaugeVec
	counts map[string]*mappingCount
}

type mappingCount struct {
	values []string
	n      int
}

// mappingsMu guards the counts of all mappings. The exporter functions are called concurrently
// by allocations, trials and API handlers.
var mappingsMu sync.Mutex

func newMapping(opts prometheus.GaugeOpts, labels []string) *mapping {
	return &mapping{vec: promauto.NewGaugeVec(opts, labels), counts: map[string]*mappingCount{}}
}

// mappingKey quotes each value, so that values containing separators, such as user-supplied
// experiment labels, cannot collide.
func mappingKey(values []string) string {
	return fmt.Sprintf("%q", values)
}

// inc adds an association of the label set. The caller holds mappingsMu.
func (m *mapping) inc(values ...string) {
	key := mappingKey(values)
	c := m.counts[key]
	if c == nil {
		c = &mappingCount{values: values}
		m.counts[key] = c
	}
	c.n++
	m.vec.WithLabelValues(values...).Set(float64(c.n))
}

// dec removes an association of the label set and deletes the label set with its last one. The
// caller holds mappingsMu.
func (m *mapping) dec(values ...string) {
	key := mappingKey(values)
	c := m.counts[key]
	if c == nil {
		return
	}
	c.n--
	if c.n > 0 {
		m.vec.WithLabelValues(values...).Set(float64(c.n))
		return
	}
	delete(m.counts, key)
	m.vec.DeleteLabelValues(values...)
}

// set sets the count of the label set to n > 0. The caller holds mappingsMu.
func (m *mapping) set(n int, values ...string) {
	m.counts[mappingKey(values)] = &mappingCount{values: values, n: n}
	m.vec.WithLabelValues(values...).Set(float64(n))
}

// has reports whether the label set has an association. The caller holds mappingsMu.
func (m *mapping) has(values ...string) bool {
	return m.counts[mappingKey(values)] != nil
}

// countWhere returns the associations of the label sets whose value at index i is value. The
// caller holds mappingsMu.
func (m *mapping) countWhere(i int, value string) int {
	n := 0
	for _, c := range m.counts {
		if c.values[i] == value {
			n += c.n
		}
	}
	return n
}

// deleteWhere deletes every label set whose value at index i is value. The caller holds
// mappingsMu.
func (m *mapping) deleteWhere(i int, value string) {
	for key, c := range m.counts {
		if c.values[i] == value {
			delete(m.counts, key)
			m.vec.DeleteLabelValues(c.values...)
		}
	}
}

var (
	containerIDToAllocationID = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "container_id_allocation_id",
		Help: `
Exposes mapping of allocation ID to container ID`,
	}, []string{"container_id", "allocation_id"})

	allocationIDToTask = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "allocation_id_task_id_task_actor",
		Help: `
Exposes mapping of allocation ID to task ID and actor`,
	}, []string{"allocation_id", "task_id", "task_actor", "job_id"})

	containerIDToRuntimeID = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "container_id_runtime_container_id",
		Help:      "a mapping of the container ID to the container ID given be the runtime",
	}, []string{"container_runtime_id", "container_id"})

	jobIDToExperimentID = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "job_id_experiment_id",
		Help:      "a mapping of the job ID to the experiment ID",
	}, []string{"job_id", "experiment_id"})

	experimentIDToLabels = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "experiment_id_label",
		Help:      "a mapping of the experiment ID to the labels",
	}, []string{"experiment_id", "label"})

	gpuUUIDToContainerID = newMapping(prometheus.GaugeOpts{
		Subsystem: "det",
		Name:      "gpu_uuid_container_id",
		Help: `
Exposes mapping of Determined's container ID to the GPU UUID/device ID as given by nvidia-smi
`,
	}, []string{"gpu_uuid", "container_id"})

	// DetStateMetrics is a prometheus registry containing all exported user-facing metrics.
	DetStateMetrics = prometheus.NewRegistry()
)

const (
	// CAdvisorPort is the default port for cAdvisor.
	CAdvisorPort = ":8080"

	// DcgmPort is the default port for DCGM.
	DcgmPort = ":9400"

	// DetAgentIDLabel is the internal ID for the Determined agent.
	DetAgentIDLabel = "det_agent_id"

	// DetResourcePoolLabel is the resource pool name.
	DetResourcePoolLabel = "det_resource_pool"
)

// TargetSDConfig is the format for specifying targets for prometheus service discovery.
type TargetSDConfig struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

func init() { //nolint: gochecknoinits
	DetStateMetrics.MustRegister(containerIDToAllocationID.vec)
	DetStateMetrics.MustRegister(containerIDToRuntimeID.vec)
	DetStateMetrics.MustRegister(gpuUUIDToContainerID.vec)
	DetStateMetrics.MustRegister(experimentIDToLabels.vec)
	DetStateMetrics.MustRegister(allocationIDToTask.vec)
	DetStateMetrics.MustRegister(jobIDToExperimentID.vec)
}

// AssociateAllocationContainer associates an allocation with its container ID.
func AssociateAllocationContainer(aID model.AllocationID, cID cproto.ID) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	containerIDToAllocationID.inc(cID.String(), aID.String())
}

// AssociateAllocationTask associates an allocation ID with its task/job info.
func AssociateAllocationTask(aID model.AllocationID,
	tID model.TaskID,
	name string,
	jID model.JobID,
) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	allocationIDToTask.inc(aID.String(), tID.String(), name, jID.String())
}

// AssociateJobExperiment associates a job ID with experiment info.
func AssociateJobExperiment(jID model.JobID, eID string, labels expconf.Labels) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	jobIDToExperimentID.inc(jID.String(), eID)
	for l := range labels {
		experimentIDToLabels.inc(eID, l)
	}
}

// DisassociateJobExperiment disassociates a job ID with experiment info. When the experiment has
// no association left, all its labels are removed, including those set by SetExperimentIDLabels
// when its labels were edited.
func DisassociateJobExperiment(jID model.JobID, eID string, labels expconf.Labels) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	jobIDToExperimentID.dec(jID.String(), eID)
	for l := range labels {
		experimentIDToLabels.dec(eID, l)
	}
	if !jobIDToExperimentID.has(jID.String(), eID) {
		experimentIDToLabels.deleteWhere(0, eID)
	}
}

// DisassociateAllocationTask disassociates an allocation ID with its task info.
func DisassociateAllocationTask(aID model.AllocationID, tID model.TaskID, name string,
	jID model.JobID,
) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	allocationIDToTask.dec(aID.String(), tID.String(), name, jID.String())
}

// AssociateContainerRuntimeID associates a Determined container ID with the runtime container ID.
func AssociateContainerRuntimeID(cID cproto.ID, dcID string) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	containerIDToRuntimeID.inc(dcID, cID.String())
}

// AddAllocationResources associates allocation and container and container and GPUs.
func AddAllocationResources(summary sproto.ResourcesSummary,
	containerStarted *sproto.ResourcesStarted,
) {
	if summary.ContainerID == nil {
		return
	}

	AssociateAllocationContainer(summary.AllocationID, *summary.ContainerID)
	AssociateContainerRuntimeID(*summary.ContainerID, containerStarted.NativeResourcesID)
	for _, ds := range summary.AgentDevices {
		for _, d := range ds {
			AssociateContainerGPU(*summary.ContainerID, d)
		}
	}
}

// RemoveAllocationResources disassociates allocation and container and container and its GPUs.
func RemoveAllocationResources(summary sproto.ResourcesSummary,
	started *sproto.ResourcesStarted,
) {
	if summary.ContainerID == nil {
		return
	}

	DisassociateAllocationContainer(summary.AllocationID, *summary.ContainerID)
	if started != nil {
		DisassociateContainerRuntimeID(*summary.ContainerID, started.NativeResourcesID)
	}
	for _, ds := range summary.AgentDevices {
		for _, d := range ds {
			DisassociateContainerGPU(*summary.ContainerID, d)
		}
	}
}

// DisassociateContainerRuntimeID removes the runtime identity when its
// corresponding container stops. Otherwise a terminated container remains a
// positive mapping after its allocation has ended.
func DisassociateContainerRuntimeID(cID cproto.ID, dcID string) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	containerIDToRuntimeID.dec(dcID, cID.String())
}

// DisassociateAllocationContainer disassociates allocation ID with its container ID.
func DisassociateAllocationContainer(aID model.AllocationID, cID cproto.ID) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	containerIDToAllocationID.dec(cID.String(), aID.String())
}

// SetExperimentIDLabels replaces the labels of an experiment when they are edited. Like
// AssociateJobExperiment, it exports them only while a trial of the experiment has an allocation:
// otherwise nothing would remove them. Each label counts the experiment's job associations, so
// that the first of several trial allocations to exit does not remove a label that the others
// still have; DisassociateJobExperiment removes the rest with the last one.
func SetExperimentIDLabels(eID string, labels []string) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	n := jobIDToExperimentID.countWhere(1, eID)
	if n == 0 {
		return
	}
	experimentIDToLabels.deleteWhere(0, eID)
	for _, l := range labels {
		experimentIDToLabels.set(n, eID, l)
	}
}

// DisassociateExperimentIDLabels disassociates experiment ID with a list of labels.
func DisassociateExperimentIDLabels(eID string, labels []string) {
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	for i := range labels {
		experimentIDToLabels.dec(eID, labels[i])
	}
}

// AssociateContainerGPU associates container ID with GPU device ID.
func AssociateContainerGPU(cID cproto.ID, d device.Device) {
	if d.Type != device.CUDA {
		return
	}
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	gpuUUIDToContainerID.inc(d.UUID, cID.String())
}

// DisassociateContainerGPU removes association between container ID and device ID.
func DisassociateContainerGPU(cID cproto.ID, d device.Device) {
	if d.Type != device.CUDA {
		return
	}
	mappingsMu.Lock()
	defer mappingsMu.Unlock()
	gpuUUIDToContainerID.dec(d.UUID, cID.String())
}
