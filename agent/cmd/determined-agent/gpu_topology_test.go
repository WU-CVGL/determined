package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/agent/internal/detect"
	"github.com/determined-ai/determined/agent/internal/options"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

func gpus(n int) []device.Device {
	devices := []device.Device{}
	for i := 0; i < n; i++ {
		devices = append(devices, device.Device{
			ID: device.ID(i), Brand: "test", UUID: "GPU-" + string(rune('a'+i)), Type: device.CUDA,
		})
	}
	return devices
}

// recordingDeps records the order of the subcommand's steps. Its collection has the inventory it
// is given, unmeasured, with the init status c.
func recordingDeps(calls *[]string, c detect.GPUCollection, detected []device.Device) gpuTopologyDeps {
	return gpuTopologyDeps{
		detect: func(slotType, visibleGPUs string) ([]device.Device, error) {
			*calls = append(*calls, "detect "+slotType+" "+visibleGPUs)
			return detected, nil
		},
		collect: func(devices, excluded []device.Device) detect.GPUCollection {
			*calls = append(*calls, fmt.Sprintf("collect %d+%d", len(devices), len(excluded)))
			if len(devices)+len(excluded) == 0 {
				return c
			}
			c.Topology = &aproto.GPUTopology{UnknownReason: "NVML init: ERROR_LIBRARY_NOT_FOUND (12)"}
			for _, d := range devices {
				c.Topology.GPUs = append(c.Topology.GPUs, aproto.GPUInfo{UUID: d.UUID})
			}
			for _, d := range excluded {
				c.Topology.GPUs = append(c.Topology.GPUs, aproto.GPUInfo{UUID: d.UUID, Excluded: true})
			}
			return c
		},
	}
}

var libraryNotFound = detect.GPUCollection{NVMLInit: "ERROR_LIBRARY_NOT_FOUND", NVMLInitCode: 12}

func encodeOutput(t *testing.T, out gpuTopologyOutput) map[string]any {
	var buf bytes.Buffer
	require.NoError(t, writeGPUTopology(&buf, out))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	return decoded
}

func TestGPUTopologySubcommandOneCollection(t *testing.T) {
	var calls []string
	deps := recordingDeps(&calls, libraryNotFound, gpus(2))
	out := runGPUTopology(deps, "auto", "0,1", "")

	require.Equal(t, []string{"detect auto 0,1", "collect 2+0"}, calls)
	decoded := encodeOutput(t, out)
	require.Equal(t, "ERROR_LIBRARY_NOT_FOUND", decoded["nvml_init"])
	require.InDelta(t, 12, decoded["nvml_init_code"], 0)
	require.NotContains(t, decoded, "driver_version")
	require.Len(t, decoded["devices"], 2)
	require.Equal(t, []any{}, decoded["excluded"])
	require.NotContains(t, decoded, "exclude_error")
	topo := decoded["topology"].(map[string]any)
	require.Equal(t, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", topo["unknown_reason"])
	require.NotContains(t, topo, "collected_at", "an unknown collection time is left out")

	// SUCCESS adds the driver version.
	calls = nil
	deps = recordingDeps(&calls,
		detect.GPUCollection{NVMLInit: "SUCCESS", DriverVersion: "610.57.04"}, gpus(1))
	decoded = encodeOutput(t, runGPUTopology(deps, "auto", "", ""))
	require.Equal(t, "SUCCESS", decoded["nvml_init"])
	require.InDelta(t, 0, decoded["nvml_init_code"], 0)
	require.Equal(t, "610.57.04", decoded["driver_version"])

	// The statuses without an NVML return reach the output unchanged.
	for _, c := range []detect.GPUCollection{
		{NVMLInit: "NOT_BUILT", NVMLInitCode: -1},
		{NVMLInit: "TIMEOUT", NVMLInitCode: -2},
	} {
		calls = nil
		decoded = encodeOutput(t, runGPUTopology(recordingDeps(&calls, c, gpus(1)), "auto", "", ""))
		require.Equal(t, c.NVMLInit, decoded["nvml_init"])
		require.InDelta(t, c.NVMLInitCode, decoded["nvml_init_code"], 0)
	}

	// Without GPUs, NVML is still loaded: the release check reads nvml_init in a GPU-less image.
	calls = nil
	decoded = encodeOutput(t, runGPUTopology(recordingDeps(&calls, libraryNotFound, nil), "auto", "", ""))
	require.Equal(t, []string{"detect auto ", "collect 0+0"}, calls)
	require.Equal(t, "ERROR_LIBRARY_NOT_FOUND", decoded["nvml_init"])
	require.Nil(t, decoded["topology"])

	// A detection failure is reported, not fatal, and NVML is still loaded.
	calls = nil
	deps = recordingDeps(&calls, libraryNotFound, nil)
	deps.detect = func(string, string) ([]device.Device, error) {
		return nil, errors.New("error parsing output of nvidia-smi")
	}
	decoded = encodeOutput(t, runGPUTopology(deps, "auto", "", "GPU-a"))
	require.Equal(t, []string{"collect 0+0"}, calls)
	require.Equal(t, "error parsing output of nvidia-smi", decoded["detect_error"])
	require.NotContains(t, decoded, "exclude_error")
	require.Equal(t, "ERROR_LIBRARY_NOT_FOUND", decoded["nvml_init"])
	require.Nil(t, decoded["topology"])
}

func TestGPUTopologySubcommandExcludeGPUs(t *testing.T) {
	var calls []string
	deps := recordingDeps(&calls, detect.GPUCollection{NVMLInit: "SUCCESS"}, gpus(4))

	out := runGPUTopology(deps, "auto", "", "GPU-c")
	require.Empty(t, out.ExcludeError)
	require.Equal(t, []device.Device{gpus(4)[0], gpus(4)[1], gpus(4)[3]}, out.Devices)
	require.Equal(t, []device.Device{gpus(4)[2]}, out.Excluded)
	require.Equal(t, aproto.GPUInfo{UUID: "GPU-c", Excluded: true}, out.Topology.GPUs[3])

	// An entry that matches no GPU is printed instead of stopping the command: the detected
	// devices are unchanged and nothing is excluded.
	out = runGPUTopology(deps, "auto", "", "GPU-c,GPU-typo")
	require.Equal(t, `exclude_gpus: no detected CUDA GPU has the UUID "GPU-typo"`, out.ExcludeError)
	require.Equal(t, gpus(4), out.Devices)
	require.Equal(t, []device.Device{}, out.Excluded)
	decoded := encodeOutput(t, out)
	require.Equal(t, []any{}, decoded["excluded"])
	require.Len(t, out.Topology.GPUs, 4)
	for _, g := range out.Topology.GPUs {
		require.False(t, g.Excluded)
	}
}

func TestGPUTopologySubcommandLinks(t *testing.T) {
	ok := aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK, Write: aproto.GPUP2PStatusOK}
	deps := gpuTopologyDeps{
		detect: func(string, string) ([]device.Device, error) { return gpus(2), nil },
		collect: func(devices, excluded []device.Device) detect.GPUCollection {
			return detect.GPUCollection{NVMLInit: "SUCCESS", Topology: &aproto.GPUTopology{
				GPUs: []aproto.GPUInfo{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
				Links: []aproto.GPULink{{
					UUIDA: "GPU-a", UUIDB: "GPU-b", Level: aproto.GPULinkLevelNode,
					P2PAToB: ok, P2PBToA: aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK},
				}},
			}}
		},
	}
	decoded := encodeOutput(t, runGPUTopology(deps, "auto", "", ""))
	links := decoded["topology"].(map[string]any)["links"].([]any)
	require.Len(t, links, 1)
	link := links[0].(map[string]any)
	require.Equal(t, "", link["p2p"], "a failed WRITE query leaves the pair unknown")
	require.Equal(t, map[string]any{"read": "OK", "write": "OK"}, link["p2p_a_to_b"])
	require.Equal(t, map[string]any{"read": "OK"}, link["p2p_b_to_a"])
	require.Equal(t, "NODE", link["level"])
}

func TestGPUTopologySubcommandFlagErrors(t *testing.T) {
	cmd := newGPUTopologyCmd()
	cmd.SetArgs([]string{"--slot-type", "tpu"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.ErrorContains(t, cmd.Execute(), "invalid --slot-type")

	cmd = newGPUTopologyCmd()
	cmd.SetArgs([]string{"--no-such-flag"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	require.Error(t, cmd.Execute())
}

// optionsFromViper decodes the agent options from the global viper, as the run command does.
func optionsFromViper(t *testing.T) options.Options {
	bs, err := json.Marshal(v.AllSettings())
	require.NoError(t, err)
	var opts options.Options
	require.NoError(t, yaml.Unmarshal(bs, &opts, yaml.DisallowUnknownFields))
	return opts
}

// The config key and the flag set exclude_gpus; DET_EXCLUDE_GPUS does not, because an older agent
// would ignore the variable and run tasks on the GPU.
func TestExcludeGPUsOption(t *testing.T) {
	t.Setenv("DET_EXCLUDE_GPUS", "GPU-from-env")
	t.Setenv("DET_VISIBLE_GPUS", "0,1") // control: the environment works for other options

	opts := optionsFromViper(t)
	require.Equal(t, "0,1", opts.VisibleGPUs)
	require.Empty(t, opts.ExcludeGPUs, "DET_EXCLUDE_GPUS must not set the option")

	flag := runCmd.Flags().Lookup("exclude-gpus")
	require.NotNil(t, flag)
	require.NoError(t, runCmd.Flags().Set("exclude-gpus", "GPU-from-flag"))
	t.Cleanup(func() {
		require.NoError(t, flag.Value.Set(""))
		flag.Changed = false
	})
	require.Equal(t, "GPU-from-flag", optionsFromViper(t).ExcludeGPUs)
}

func TestExcludeGPUsConfigKey(t *testing.T) {
	initialViperConfig := v
	t.Cleanup(func() { v = initialViperConfig })
	v = createViperWithDefaults()

	opts, err := mergeConfigIntoViper([]byte("exclude_gpus: GPU-x,GPU-y\n"))
	require.NoError(t, err)
	require.Equal(t, "GPU-x,GPU-y", opts.ExcludeGPUs)
}
