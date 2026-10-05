package main

import (
	"bytes"
	"encoding/json"
	"errors"
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

// recordingDeps records the order of the subcommand's steps.
func recordingDeps(calls *[]string, status detect.NVMLInitStatus, detected []device.Device) gpuTopologyDeps {
	return gpuTopologyDeps{
		probe: func() detect.NVMLInitStatus {
			*calls = append(*calls, "probe")
			return status
		},
		detect: func(slotType, visibleGPUs string) ([]device.Device, error) {
			*calls = append(*calls, "detect "+slotType+" "+visibleGPUs)
			return detected, nil
		},
		topology: func(devices, excluded []device.Device) *aproto.GPUTopology {
			*calls = append(*calls, "topology")
			topo := &aproto.GPUTopology{UnknownReason: "NVML init: ERROR_LIBRARY_NOT_FOUND (12)"}
			for _, d := range devices {
				topo.GPUs = append(topo.GPUs, aproto.GPUInfo{UUID: d.UUID})
			}
			for _, d := range excluded {
				topo.GPUs = append(topo.GPUs, aproto.GPUInfo{UUID: d.UUID, Excluded: true})
			}
			return topo
		},
	}
}

func encodeOutput(t *testing.T, out gpuTopologyOutput) map[string]any {
	var buf bytes.Buffer
	require.NoError(t, writeGPUTopology(&buf, out))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	return decoded
}

func TestGPUTopologySubcommandInitsFirst(t *testing.T) {
	var calls []string
	deps := recordingDeps(&calls, detect.NVMLInitStatus{Name: "ERROR_LIBRARY_NOT_FOUND", Code: 12}, gpus(2))
	out := runGPUTopology(deps, "auto", "0,1", "")

	require.Equal(t, []string{"probe", "detect auto 0,1", "topology"}, calls)
	decoded := encodeOutput(t, out)
	require.Equal(t, "ERROR_LIBRARY_NOT_FOUND", decoded["nvml_init"])
	require.InDelta(t, 12, decoded["nvml_init_code"], 0)
	require.NotContains(t, decoded, "driver_version")
	require.Len(t, decoded["devices"], 2)
	require.Equal(t, []any{}, decoded["excluded"])
	require.NotContains(t, decoded, "exclude_error")
	topo := decoded["topology"].(map[string]any)
	require.Equal(t, "NVML init: ERROR_LIBRARY_NOT_FOUND (12)", topo["unknown_reason"])

	// SUCCESS adds the driver version.
	calls = nil
	deps = recordingDeps(&calls,
		detect.NVMLInitStatus{Name: "SUCCESS", Code: 0, DriverVersion: "610.57.04"}, gpus(1))
	decoded = encodeOutput(t, runGPUTopology(deps, "auto", "", ""))
	require.Equal(t, "SUCCESS", decoded["nvml_init"])
	require.InDelta(t, 0, decoded["nvml_init_code"], 0)
	require.Equal(t, "610.57.04", decoded["driver_version"])

	// The stub's answer reaches the output unchanged.
	calls = nil
	deps = recordingDeps(&calls, detect.NVMLInitStatus{Name: "NOT_BUILT", Code: -1}, gpus(1))
	decoded = encodeOutput(t, runGPUTopology(deps, "auto", "", ""))
	require.Equal(t, "NOT_BUILT", decoded["nvml_init"])
	require.InDelta(t, -1, decoded["nvml_init_code"], 0)

	// A detection failure is reported, not fatal.
	calls = nil
	deps = recordingDeps(&calls, detect.NVMLInitStatus{Name: "SUCCESS"}, nil)
	deps.detect = func(string, string) ([]device.Device, error) {
		return nil, errors.New("error parsing output of nvidia-smi")
	}
	decoded = encodeOutput(t, runGPUTopology(deps, "auto", "", ""))
	require.Equal(t, "error parsing output of nvidia-smi", decoded["detect_error"])
	require.Nil(t, decoded["topology"])
}

func TestGPUTopologySubcommandExcludeGPUs(t *testing.T) {
	var calls []string
	deps := recordingDeps(&calls, detect.NVMLInitStatus{Name: "SUCCESS"}, gpus(4))

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
		probe:  func() detect.NVMLInitStatus { return detect.NVMLInitStatus{Name: "SUCCESS"} },
		detect: func(string, string) ([]device.Device, error) { return gpus(2), nil },
		topology: func(devices, excluded []device.Device) *aproto.GPUTopology {
			return &aproto.GPUTopology{
				GPUs: []aproto.GPUInfo{{UUID: "GPU-a"}, {UUID: "GPU-b"}},
				Links: []aproto.GPULink{{
					UUIDA: "GPU-a", UUIDB: "GPU-b", Level: aproto.GPULinkLevelNode,
					P2PAToB: ok, P2PBToA: aproto.GPUP2PCaps{Read: aproto.GPUP2PStatusOK},
				}},
			}
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

// X1: the config key and the flag set exclude_gpus; DET_EXCLUDE_GPUS does not.
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
