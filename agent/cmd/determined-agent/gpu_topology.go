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

// gpuTopologyDeps are the steps of the gpu-topology subcommand; tests replace them.
type gpuTopologyDeps struct {
	probe    func() detect.NVMLInitStatus
	detect   func(slotType, visibleGPUs string) ([]device.Device, error)
	topology func(devices, excluded []device.Device) *aproto.GPUTopology
}

var defaultGPUTopologyDeps = gpuTopologyDeps{
	probe: detect.ProbeNVMLInit,
	detect: func(slotType, visibleGPUs string) ([]device.Device, error) {
		devices, _, err := detect.Detect(slotType, "", visibleGPUs, nil, 0)
		return devices, err
	},
	topology: detect.DetectGPUTopology,
}

// gpuTopologyLink is a link with the pair's P2P state derived by the N3 rule.
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
		Short: "print the GPU topology, P2P status, PCIe links and NVML errors the agent would report",
		Long: "Initialize NVML on its own, then run the agent's device detection, exclude list and " +
			"GPU topology collection, and print the result as JSON. It exits 0 even without NVML.",
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
	// NVML first, independent of detection: the release check reads nvml_init in a GPU-less image.
	status := deps.probe()
	out := gpuTopologyOutput{
		NVMLInit:      status.Name,
		NVMLInitCode:  status.Code,
		DriverVersion: status.DriverVersion,
		Devices:       []device.Device{},
		Excluded:      []device.Device{},
	}

	detected, err := deps.detect(slotType, visibleGPUs)
	if err != nil {
		out.DetectError = err.Error()
		return out
	}
	devices, excluded, err := detect.SplitExcluded(detected, detect.ParseExcludeGPUs(excludeGPUs))
	if err != nil {
		out.ExcludeError = err.Error()
		devices, excluded = detected, nil
	}
	if devices != nil {
		out.Devices = devices
	}
	if excluded != nil {
		out.Excluded = excluded
	}

	if topo := deps.topology(devices, excluded); topo != nil {
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
