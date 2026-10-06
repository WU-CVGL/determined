package main

import (
	"encoding/json"
	"io"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/determined-ai/determined/agent/internal/detect"
	"github.com/determined-ai/determined/agent/internal/options"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/device"
)

// gpuTopologyDeps are the two steps of the gpu-topology subcommand; tests replace them.
type gpuTopologyDeps struct {
	detect  func(slotType, visibleGPUs string) ([]device.Device, error)
	collect func(devices, excluded []device.Device) detect.GPUCollection
}

var defaultGPUTopologyDeps = gpuTopologyDeps{
	detect: func(slotType, visibleGPUs string) ([]device.Device, error) {
		devices, _, err := detect.Detect(slotType, "", visibleGPUs, nil, 0)
		return devices, err
	},
	collect: detect.CollectGPUs,
}

// gpuTopologyLink is a link with the pair's P2P state: usable only when READ and WRITE are OK in
// both directions.
type gpuTopologyLink struct {
	aproto.GPULink
	P2P aproto.GPUP2PUsability `json:"p2p"`
}

// gpuTopologyReport is the topology as the agent would send it, with derived pair values.
type gpuTopologyReport struct {
	aproto.GPUTopology
	Links []gpuTopologyLink `json:"links"`
}

// gpuTopologyOutput is what the subcommand prints.
type gpuTopologyOutput struct {
	NVMLInit      string `json:"nvml_init"`
	NVMLInitCode  int    `json:"nvml_init_code"`
	DriverVersion string `json:"driver_version,omitempty"`
	// DetectError is set when device detection itself failed.
	DetectError string          `json:"detect_error,omitempty"`
	Devices     []device.Device `json:"devices"`
	Excluded    []device.Device `json:"excluded"`
	// ExcludeError is set when an --exclude-gpus entry matches no GPU. Devices are then the
	// detected devices unchanged, and Excluded is empty.
	ExcludeError string             `json:"exclude_error,omitempty"`
	Topology     *gpuTopologyReport `json:"topology"`
}

func newGPUTopologyCmd() *cobra.Command {
	defaults := options.DefaultOptions()
	var slotType, visibleGPUs, excludeGPUs string

	cmd := &cobra.Command{
		Use:   "gpu-topology",
		Short: "probe the GPU topology, P2P, PCIe links and NVML errors as the agent would with these flags",
		Long: "Run the agent's device detection and exclude list, then the agent's NVML session " +
			"(one Init and the GPU topology collection, within 60 s), and print the result as " +
			"JSON. NVML is loaded also without GPUs. Device detection runs nvidia-smi without a " +
			"timeout. It exits 0 even without NVML. It is a standalone probe: it starts from the " +
			"defaults and its own flags, never from a running agent's configuration file or " +
			"environment, also inside the agent container, so pass the agent's --slot-type, " +
			"--visible-gpus and --exclude-gpus to reproduce what the agent reports.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := check.In(slotType, []string{"gpu", "cuda", "rocm", "cpu", "auto", "none"}); err != nil {
				return errors.Wrap(err, "invalid --slot-type")
			}
			out := runGPUTopology(defaultGPUTopologyDeps, slotType, visibleGPUs, excludeGPUs)
			return writeGPUTopology(cmd.OutOrStdout(), out)
		},
	}
	cmd.Flags().StringVar(&slotType, "slot-type", defaults.SlotType, "slot type to detect")
	cmd.Flags().StringVar(&visibleGPUs, "visible-gpus", defaults.VisibleGPUs, "GPUs to expose as slots")
	cmd.Flags().StringVar(&excludeGPUs, "exclude-gpus", "",
		"comma-separated UUIDs of GPUs to report but never offer as slots")
	return cmd
}

func runGPUTopology(deps gpuTopologyDeps, slotType, visibleGPUs, excludeGPUs string) gpuTopologyOutput {
	out := gpuTopologyOutput{Devices: []device.Device{}, Excluded: []device.Device{}}
	var devices, excluded []device.Device
	detected, err := deps.detect(slotType, visibleGPUs)
	if err != nil {
		out.DetectError = err.Error()
	} else {
		devices, excluded, err = detect.SplitExcluded(detected, detect.ParseExcludeGPUs(excludeGPUs))
		if err != nil {
			out.ExcludeError = err.Error()
			devices, excluded = detected, nil
		}
	}
	if devices != nil {
		out.Devices = devices
	}
	if excluded != nil {
		out.Excluded = excluded
	}

	// Also after a detection error: the release check reads nvml_init in a GPU-less image.
	c := deps.collect(devices, excluded)
	out.NVMLInit, out.NVMLInitCode, out.DriverVersion = c.NVMLInit, c.NVMLInitCode, c.DriverVersion
	if topo := c.Topology; topo != nil {
		report := &gpuTopologyReport{GPUTopology: *topo, Links: []gpuTopologyLink{}}
		for _, l := range topo.Links {
			report.Links = append(report.Links, gpuTopologyLink{GPULink: l, P2P: aproto.P2PUsability(l)})
		}
		out.Topology = report
	}
	return out
}

func writeGPUTopology(w io.Writer, out gpuTopologyOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
