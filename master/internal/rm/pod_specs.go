package rm

// PodSpecApplier is a resource manager that says whether the containers it starts for the tasks
// of a resource pool get their pod spec (environment.pod_spec, or the pod spec of the task
// container defaults): its volumes and the node it pins them to. The Kubernetes resource manager
// builds every pod from it. The agent resource manager starts a container from its Docker spec,
// with the task's bind mounts and no pod spec, and the dispatcher resource managers (Slurm, PBS)
// read no pod spec either.
type PodSpecApplier interface {
	AppliesPodSpecs(pool ResourcePoolName) (bool, error)
}
