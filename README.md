<p align="center"><img width="400" src="determined-logo.svg" alt="Determined AI Logo"></p>

> **Community fork:** This repository continues open-source Determined development
> and maintenance. See the [installation instructions](#installation),
> [deployment guide](docs/maintenance/distribution.md), and
> [release notes](docs/release-notes.rst).

Determined is an open-source deep learning platform for teams that share GPU clusters. A master
schedules work onto the agents of its resource pools, tracks experiments, checkpoints and logs,
and serves a WebUI, a REST API and the `det` CLI. One cluster runs interactive development and
batch training side by side, with PyTorch, TensorFlow, DeepSpeed or any program in a container.

# Interactive

One shell task runs an SSH server, JupyterLab and VS Code in the browser (code-server served by
the master) in the same container, with the same user, files and GPUs. Start it with **Launch
Interactive Task** on the **Jobs** page and work on its task page, in the **Terminal**,
**JupyterLab**, **VS Code**, **Logs** and **Resources** tabs. A shell runs until it is killed,
or until it has been idle for the `idle_timeout` its configuration sets.

![The task page of a shell with its Terminal, JupyterLab, VS Code, Logs and Resources tabs, JupyterLab open](docs/assets/readme_images/interactive-task-page.png)

![The Launch Interactive Task form with a resource pool, slots, and the jupyter and vscode services checked](docs/assets/readme_images/launch-interactive-task.png)

```bash
det shell start                    # an SSH session; JupyterLab and VS Code run with it
det task services <shell id>       # the shell's services and their states
det task open <shell id> vscode    # VS Code in the browser
```

# Batch

- **Experiments** with hyperparameter search and distributed training, with metrics,
  checkpoints and a model registry.
- **Commands and TensorBoards** for one-off programs and for viewing training metrics.
- **GPU groups**: `resources.gpu_groups` places named groups of GPUs of exact models on one agent.
- **Resource-pool access control**: pools are public until an administrator restricts them to
  the users granted access.
- **GPU topology placement**: tasks get NUMA-packed or best-connected GPUs, and a pool's topology
  view outlines the GPUs each job holds.
- **Task resource charts**: the CPU, memory and GPU use of each task, read from Prometheus.

![A resource pool's topology view with one job's GPUs outlined, above its Active tab listing each job's GPUs](docs/assets/readme_images/resource-pool-topology.png)

![The Resources tab of a task with CPU, memory, GPU utilization and allocation lifecycle charts](docs/assets/readme_images/task-resources.png)

```yaml
resources:
  slots_per_trial: 2
  gpu_groups:
    - {name: simulation, count: 1, models: ["NVIDIA GeForce RTX 4090"]}
    - {name: learner, count: 1, models: ["NVIDIA H100 PCIe"]}
```

```bash
det experiment create experiment.yaml .
```

# Installation

To install this fork's CLI and Python SDK from the repository:

```bash
python -m pip install 'git+https://github.com/WU-CVGL/determined.git@main#subdirectory=harness'
```

Pin the release and its package version:

```bash
VERSION=0.42.5 python -m pip install 'git+https://github.com/WU-CVGL/determined.git@0.42.5#subdirectory=harness'
```

This installs the CLI and SDK only. To run this fork's master and agent, use matching
images and the [fork distribution guide](docs/maintenance/distribution.md).
`det deploy` uses this fork's images under `ghcr.io/wu-cvgl` by default (CLI 0.40.1 and
earlier default to upstream `determinedai`: pass `--image-repo-prefix ghcr.io/wu-cvgl`);
installing the fork CLI alone does not upgrade a cluster.

# Documentation

A master serves the documentation of its version at `/docs` and the REST API reference at
`/docs/rest-api`, for example `http://<master>:8080/docs`. The sources are in [docs/](docs/), and
the [examples folder](examples/) has example models and configurations.

# Contributing

[Contributor's Guide](CONTRIBUTING.md)

# License

[Apache V2](LICENSE)
