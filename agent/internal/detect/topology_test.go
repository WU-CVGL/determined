package detect

import (
	"sync"
	"testing"
	"time"

	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func cudaDevices(uuids ...string) []device.Device {
	var devices []device.Device
	for i, u := range uuids {
		devices = append(devices, device.Device{ID: device.ID(i), Brand: "test", UUID: u, Type: device.CUDA})
	}
	return devices
}

// sessionCalls records the inventories a fake NVML session was given. The session runs on its
// own goroutine, so it reports failures with t.Errorf.
type sessionCalls struct {
	mu          sync.Mutex
	inventories [][]aproto.GPUInfo
}

func (s *sessionCalls) session(result func([]aproto.GPUInfo) GPUCollection) nvmlSession {
	return func(inv []aproto.GPUInfo) GPUCollection {
		s.mu.Lock()
		s.inventories = append(s.inventories, inv)
		s.mu.Unlock()
		return result(inv)
	}
}

func (s *sessionCalls) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inventories)
}

func mustNotRun(t *testing.T) nvmlSession {
	return func([]aproto.GPUInfo) GPUCollection {
		t.Errorf("NVML must not be loaded")
		return GPUCollection{}
	}
}

// waitSessionDone waits until no NVML session runs.
func waitSessionDone() {
	nvmlRunning <- struct{}{}
	<-nvmlRunning
}

var libraryNotFound = GPUCollection{NVMLInit: "ERROR_LIBRARY_NOT_FOUND", NVMLInitCode: 12}

func TestCollectGPUsWithoutGPUs(t *testing.T) {
	// The agent loads NVML only when it has a GPU to report.
	cpu := []device.Device{{ID: 0, Brand: "cpu", UUID: "cpu", Type: device.CPU}}
	rocm := []device.Device{{ID: 0, Brand: "amd", UUID: "0x7e", Type: device.ROCM}}
	for _, devices := range [][]device.Device{nil, cpu, rocm} {
		require.Equal(t, GPUCollection{}, collectGPUs(devices, nil, false, mustNotRun(t), time.Minute))
	}

	// The subcommand loads it anyway, so that it can tell whether the library loads.
	var calls sessionCalls
	c := collectGPUs(cpu, nil, true, calls.session(func(inv []aproto.GPUInfo) GPUCollection {
		assert.Empty(t, inv)
		return libraryNotFound
	}), time.Minute)
	require.Equal(t, libraryNotFound, c)
	require.Equal(t, 1, calls.count())

	// An agent whose GPUs are all excluded still reports them.
	c = collectGPUs(nil, cudaDevices("GPU-a"), false, calls.session(func(inv []aproto.GPUInfo) GPUCollection {
		return GPUCollection{NVMLInit: "SUCCESS", Topology: &aproto.GPUTopology{GPUs: inv}}
	}), time.Minute)
	require.Equal(t, []aproto.GPUInfo{{UUID: "GPU-a", Excluded: true}}, c.Topology.GPUs)
}

func TestCollectGPUsMIG(t *testing.T) {
	devices := cudaDevices("MIG-1111", "MIG-2222")
	want := &aproto.GPUTopology{
		UnknownReason: "MIG instances: GPU topology not collected",
		GPUs:          []aproto.GPUInfo{{UUID: "MIG-1111"}, {UUID: "MIG-2222"}},
	}
	require.Equal(t, GPUCollection{Topology: want},
		collectGPUs(devices, nil, false, mustNotRun(t), time.Minute))

	// The subcommand still initializes NVML, but measures no MIG instance.
	var calls sessionCalls
	c := collectGPUs(devices, nil, true, calls.session(func(inv []aproto.GPUInfo) GPUCollection {
		assert.Empty(t, inv)
		return GPUCollection{NVMLInit: "SUCCESS", DriverVersion: "610.57.04"}
	}), time.Minute)
	require.Equal(t, GPUCollection{NVMLInit: "SUCCESS", DriverVersion: "610.57.04", Topology: want}, c)
}

func TestCollectGPUsTimeout(t *testing.T) {
	devices := cudaDevices("GPU-a", "GPU-b")
	excluded := []device.Device{{ID: 2, UUID: "GPU-c", Type: device.CUDA}}
	wantGPUs := []aproto.GPUInfo{{UUID: "GPU-a"}, {UUID: "GPU-b"}, {UUID: "GPU-c", Excluded: true}}
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		waitSessionDone()
	})

	var calls sessionCalls
	started := make(chan struct{})
	blocking := calls.session(func(inv []aproto.GPUInfo) GPUCollection {
		close(started)
		<-release // a cgo call that never returns
		inv[0].PCIBusID = "written after the timeout"
		return GPUCollection{NVMLInit: "SUCCESS", Topology: &aproto.GPUTopology{GPUs: inv}}
	})
	c := collectGPUs(devices, excluded, true, blocking, 50*time.Millisecond)
	<-started
	require.Equal(t, GPUCollection{
		NVMLInit:     "TIMEOUT",
		NVMLInitCode: -2,
		Topology:     &aproto.GPUTopology{UnknownReason: "NVML did not finish within 0.05s", GPUs: wantGPUs},
	}, c)
	require.NotSame(t, &calls.inventories[0][0], &c.Topology.GPUs[0], "the session works on its own copy")

	// While the first session may still be blocked, no second session starts. The short deadline
	// makes a second session fail fast: it would block and report the deadline instead.
	c = collectGPUs(devices, excluded, true, blocking, 50*time.Millisecond)
	require.Equal(t, "TIMEOUT", c.NVMLInit)
	require.Equal(t, "an earlier NVML session has not finished", c.Topology.UnknownReason)
	require.Equal(t, wantGPUs, c.Topology.GPUs)
	require.Equal(t, 1, calls.count())

	require.Equal(t, "NVML did not finish within 60s", timeoutReason(nvmlTimeout))
}

func TestCollectGPUsBackToBack(t *testing.T) {
	// A collection right after a finished one finds the guard free: the session releases it
	// before it hands back its result. The window was narrow; this many collections take about a
	// second with -race and caught the release after the send in every run.
	devices := cudaDevices("GPU-a")
	instant := func(inv []aproto.GPUInfo) GPUCollection {
		return GPUCollection{NVMLInit: "SUCCESS", Topology: &aproto.GPUTopology{GPUs: inv}}
	}
	for i := 0; i < 200000; i++ {
		c := collectGPUs(devices, nil, false, instant, time.Minute)
		require.Equal(t, "SUCCESS", c.NVMLInit, "collection %d", i)
	}
}

func TestLogGPUTopology(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)
	messages := func() []string {
		var out []string
		for _, e := range hook.AllEntries() {
			out = append(out, e.Level.String()+" "+e.Message)
		}
		hook.Reset()
		return out
	}

	topo := &aproto.GPUTopology{
		DriverVersion: "610.57.04",
		GPUs: []aproto.GPUInfo{
			{UUID: "GPU-a"}, {UUID: "GPU-b"}, {UUID: "GPU-c", Excluded: true},
		},
	}
	logGPUTopology(topo)
	require.Equal(t, []string{
		"info GPU topology collected: slots=2 excluded=1 nvml_errors=0 driver=610.57.04",
	}, messages())

	// nvml_errors counts failed calls, one per line that follows.
	topo.GPUs[0].NVMLError = "GetPciInfo: ERROR_NOT_READY (27)"
	topo.GPUs[2].NVMLError = "GetCurrPcieLinkWidth: ERROR_GPU_IS_LOST (15); " +
		"GetMaxPcieLinkWidth: ERROR_UNKNOWN (999)"
	logGPUTopology(topo)
	require.Equal(t, []string{
		"warning GPU topology collected: slots=2 excluded=1 nvml_errors=3 driver=610.57.04",
		"warning GPU NVML error: uuid=GPU-a excluded=false call=GetPciInfo return=ERROR_NOT_READY (27)",
		"warning GPU NVML error: uuid=GPU-c excluded=true call=GetCurrPcieLinkWidth " +
			"return=ERROR_GPU_IS_LOST (15)",
		"warning GPU NVML error: uuid=GPU-c excluded=true call=GetMaxPcieLinkWidth " +
			"return=ERROR_UNKNOWN (999)",
	}, messages())

	logGPUTopology(unmeasured(topo.GPUs, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)"))
	require.Equal(t, []string{
		`warning GPU topology unknown: slots=2 excluded=1 reason="NVML init: ERROR_LIBRARY_NOT_FOUND (12)"`,
	}, messages())

	logGPUTopology(nil)
	require.Empty(t, messages())
}
