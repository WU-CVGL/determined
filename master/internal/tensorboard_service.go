package internal

import (
	"encoding/json"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The tensorboard service of a shell: TensorBoard on services.tensorboard.logdir, from the image,
// under the path at which the master proxies it, tasks.TensorBoardPathPrefixEnvVar, which
// Command.Start sets from the service's record.
const (
	// tensorBoardPortEnvVar is the container port of a shell's TensorBoard.
	tensorBoardPortEnvVar = "DET_TENSORBOARD_PORT"
	// tensorBoardLogDirEnvVar is the directory that a shell's TensorBoard reads, as the config
	// gives it: the unit resolves a relative one against the task's work directory.
	tensorBoardLogDirEnvVar = "DET_TENSORBOARD_LOGDIR"
	// tensorBoardArgsEnvVar holds the more arguments of a shell's TensorBoard as a JSON array of
	// strings, which the unit decodes, so that an argument may hold any character but NUL, which
	// the config refuses.
	tensorBoardArgsEnvVar = "DET_TENSORBOARD_ARGS"
)

// tensorBoardService is the tensorboard service of a shell: at "<task>:<port>", reached over plain
// HTTP. TensorBoard has no login: the master's proxy authenticates its requests, and Exec limits
// them to the task's owner and admins, as for the other services that run as the owner, since
// TensorBoard reads the owner's files. TensorBoard serves under its path prefix, so the proxy
// keeps the path.
func tensorBoardService(port int) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name: interactiveServiceTensorBoard,
		Port: port,
		Exec: true,
	}
}

// addShellTensorBoard adds the tensorboard service to a shell spec: a port from the services'
// range, outside the spec's other ports, and the port, the log directory and the arguments of the
// config in the container's environment. The service has no files.
func addShellTensorBoard(spec *tasks.GenericCommandSpec) (archive.Archive, error) {
	service := spec.Config.Services[interactiveServiceTensorBoard]
	if service.LogDir == "" {
		return nil, status.Error(codes.InvalidArgument, "services: tensorboard: logdir is required")
	}
	args := service.Args
	if args == nil {
		args = []string{}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encoding the tensorboard arguments: %s", err)
	}
	lo, hi := servicePortRange(interactiveServiceTensorBoard)
	port, err := freeServicePort(spec, interactiveServiceTensorBoard, lo, hi)
	if err != nil {
		return nil, err
	}
	spec.AddService(tensorBoardService(port))
	addExtraEnvVars(spec, map[string]string{
		tensorBoardPortEnvVar:   strconv.Itoa(port),
		tensorBoardLogDirEnvVar: service.LogDir,
		tensorBoardArgsEnvVar:   string(encoded),
	})
	return nil, nil
}
