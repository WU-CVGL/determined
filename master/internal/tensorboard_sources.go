package internal

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"

	"github.com/pkg/errors"
	"golang.org/x/exp/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	exputil "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/trials"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The files and environment of a shell's tensorboard service that reads the TensorBoard logs of
// experiments and trials. The service's loader reads tensorBoardSourcesFile, fetches each source
// with the checkpoint storage of storageConfPath and publishes it under tensorBoardSourcesLogDir,
// which TensorBoard reads. The task's environment holds no storage credentials.
const (
	// tensorBoardSourcesDir holds the service's files and tensorBoardSourcesLogDir.
	tensorBoardSourcesDir = "/run/determined/tensorboard"
	// tensorBoardSourcesFile lists the sources of the service, without credentials.
	tensorBoardSourcesFile = "/run/determined/tensorboard/sources.json"
	// tensorBoardSourcesLogDir is the directory that TensorBoard reads in sources mode.
	tensorBoardSourcesLogDir = "/run/determined/tensorboard/logs"
	// tensorBoardSourcesEnvVar is the path of tensorBoardSourcesFile, set only in sources mode.
	tensorBoardSourcesEnvVar = "DET_TENSORBOARD_SOURCES"
	// tensorBoardSourcesVersion is the version of the format of tensorBoardSourcesFile.
	tensorBoardSourcesVersion = 1
)

// tensorBoardSource is one log directory that a tensorboard service reads.
type tensorBoardSource struct {
	// Run is "experiment/<id>" or "experiment/<id>/trial/<tid>": where the loader publishes the
	// source under the service's log directory.
	Run          string `json:"run"`
	ExperimentID int32  `json:"experiment_id"`
	TrialID      *int32 `json:"trial_id,omitempty"`
	// Path is the source's log directory, as tensorBoardSourcePath builds it.
	Path string `json:"path"`
}

// tensorBoardSources is the resolved data source of a shell's tensorboard service.
type tensorBoardSources struct {
	// ExperimentIDs and TrialIDs are the canonical IDs (see canonicalTensorBoardIDs).
	ExperimentIDs, TrialIDs []int32
	// StorageType is the checkpoint storage type of every source: s3, gcs, azure, shared_fs or
	// directory.
	StorageType string
	Sources     []tensorBoardSource
	// StorageConfig is the JSON of one source experiment's checkpoint_storage, which the loader
	// reads at storageConfPath. It may hold credentials: it is never logged or put in an error.
	StorageConfig []byte
}

// tensorBoardLocation is where an experiment's checkpoint storage keeps its TensorBoard logs.
type tensorBoardLocation struct {
	// Type is the storage type, BasePath the base of the logs' paths, and Key the storage's
	// identity: two experiments with the same Key keep their logs in the same storage.
	Type, BasePath, Key string
	// SharedFSHostPath is the host path of shared_fs storage.
	SharedFSHostPath string
}

// tensorBoardSourcesFileContent is the content of tensorBoardSourcesFile.
type tensorBoardSourcesFileContent struct {
	Version       int                 `json:"version"`
	StorageType   string              `json:"storage_type"`
	StorageConfig string              `json:"storage_config"`
	Sources       []tensorBoardSource `json:"sources"`
}

// resolveTensorBoardSources resolves the experiment_ids and trial_ids of a shell's tensorboard
// service, when it runs with them, and returns nil otherwise. It checks the launcher's artifact
// permission on every experiment and trial, which a missing or invisible one fails with NotFound,
// that every source shares one checkpoint storage and its credentials, and that the launcher may
// use inline storage credentials (see checkTensorBoardSourceCredentials). Then it writes the
// canonical IDs into the spec's config and metadata and, for shared_fs storage, mounts the host
// path read-only unless the config already mounts it. It reads the database and changes nothing
// else.
func (a *apiServer) resolveTensorBoardSources(
	ctx context.Context, user *model.User, spec *tasks.GenericCommandSpec,
) (*tensorBoardSources, error) {
	svc := spec.Config.Services[model.ServiceTensorBoard]
	if !svc.Runs() || !svc.HasSources() {
		return nil, nil
	}

	owners := map[int32]*model.UserID{}
	storages := map[int32]expconf.CheckpointStorageConfig{}
	expIDs := sortedUniqueIDs(svc.ExperimentIDs)
	for _, id := range expIDs {
		exp, _, err := a.getExperimentAndCheckCanDoActions(ctx, int(id),
			exputil.AuthZProvider.Get().CanGetExperimentArtifacts)
		if err != nil {
			return nil, err
		}
		conf, err := a.m.db.LegacyExperimentConfigByID(int(id))
		if err != nil {
			return nil, err
		}
		owners[id] = exp.OwnerID
		storages[id] = conf.CheckpointStorage
	}

	trialExperiment := map[int32]int32{}
	for _, tid := range sortedUniqueIDs(svc.TrialIDs) {
		if err := trials.CanGetTrialsExperimentAndCheckCanDoAction(ctx, int(tid), user,
			exputil.AuthZProvider.Get().CanGetExperimentArtifacts); err != nil {
			return nil, err
		}
		expID, err := a.m.db.ExperimentIDByTrialID(int(tid))
		if err != nil {
			return nil, err
		}
		trialExperiment[tid] = int32(expID)
		// An experiment of the request, whose trials are all read, or of an earlier trial.
		if _, ok := storages[int32(expID)]; ok {
			continue
		}
		exp, err := db.ExperimentByID(ctx, expID)
		if err != nil {
			return nil, errors.Wrapf(err, "error loading experiment: %d", expID)
		}
		conf, err := a.m.db.LegacyExperimentConfigByID(expID)
		if err != nil {
			return nil, err
		}
		owners[int32(expID)] = exp.OwnerID
		storages[int32(expID)] = conf.CheckpointStorage
	}

	exps, trialIDs := canonicalTensorBoardIDs(svc.ExperimentIDs, svc.TrialIDs, trialExperiment)
	sourceExps := slices.Clone(exps)
	for _, tid := range trialIDs {
		sourceExps = append(sourceExps, trialExperiment[tid])
	}
	sourceExps = sortedUniqueIDs(sourceExps)

	locs := make(map[int32]tensorBoardLocation, len(sourceExps))
	for _, id := range sourceExps {
		loc, err := tensorBoardStorageLocation(storages[id])
		if status.Code(err) == codes.Unknown {
			return nil, status.Errorf(codes.InvalidArgument,
				"services: tensorboard: experiment %d: %s", id, err)
		} else if err != nil {
			return nil, err
		}
		locs[id] = loc
	}
	first := sourceExps[0]
	for _, id := range sourceExps[1:] {
		if locs[id].Key != locs[first].Key {
			return nil, status.Errorf(codes.InvalidArgument,
				"services: tensorboard: experiments %d and %d use different checkpoint storage",
				first, id)
		}
	}
	firstCredentials := tensorBoardInlineCredentials(storages[first])
	for _, id := range sourceExps[1:] {
		if !equalTensorBoardCredentials(firstCredentials, tensorBoardInlineCredentials(storages[id])) {
			return nil, status.Errorf(codes.InvalidArgument,
				"services: tensorboard: experiments %d and %d use different storage credentials",
				first, id)
		}
	}
	sourceStorages := make(map[int32]expconf.CheckpointStorageConfig, len(sourceExps))
	for _, id := range sourceExps {
		sourceStorages[id] = storages[id]
	}
	if err := checkTensorBoardSourceCredentials(*user, owners, sourceStorages); err != nil {
		return nil, err
	}

	loc := locs[first]
	var mount *model.BindMount
	if loc.Type == "shared_fs" {
		var err error
		if mount, err = tensorBoardSharedFSMount(spec.Config.BindMounts, loc.SharedFSHostPath); err != nil {
			return nil, err
		}
	}

	storageConfig, err := json.Marshal(storages[first])
	if err != nil {
		return nil, status.Error(codes.Internal,
			"services: tensorboard: encoding the checkpoint storage")
	}

	var sources []tensorBoardSource
	for _, id := range exps {
		sources = append(sources, tensorBoardSource{
			Run:          tensorBoardSourceRun(id, nil),
			ExperimentID: id,
			Path:         tensorBoardSourcePath(loc.BasePath, spec.Base.ClusterID, id, nil),
		})
	}
	for _, tid := range trialIDs {
		expID := trialExperiment[tid]
		sources = append(sources, tensorBoardSource{
			Run:          tensorBoardSourceRun(expID, ptrs.Ptr(tid)),
			ExperimentID: expID,
			TrialID:      ptrs.Ptr(tid),
			Path: tensorBoardSourcePath(
				loc.BasePath, spec.Base.ClusterID, expID, ptrs.Ptr(tid)),
		})
	}

	svc.ExperimentIDs = nilIfEmpty(exps)
	svc.TrialIDs = nilIfEmpty(trialIDs)
	spec.Config.Services[model.ServiceTensorBoard] = svc
	spec.Metadata.ExperimentIDs = nilIfEmpty(slices.Clone(exps))
	spec.Metadata.TrialIDs = nilIfEmpty(slices.Clone(trialIDs))
	if mount != nil {
		spec.Config.BindMounts = append(spec.Config.BindMounts, *mount)
	}

	return &tensorBoardSources{
		ExperimentIDs: exps,
		TrialIDs:      trialIDs,
		StorageType:   loc.Type,
		Sources:       sources,
		StorageConfig: storageConfig,
	}, nil
}

// sortedUniqueIDs returns a sorted copy of ids without duplicates.
func sortedUniqueIDs(ids []int32) []int32 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// stringValue returns *p, or "" when p is nil.
func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// nilIfEmpty returns nil for an empty list, so that a config or the metadata leaves it out.
func nilIfEmpty(ids []int32) []int32 {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// canonicalTensorBoardIDs returns the experiment and trial IDs sorted and without duplicates,
// without the trials whose experiment, by trialExperiment, is one of the experiments: an
// experiment's source holds all its trials.
func canonicalTensorBoardIDs(
	expIDs, trialIDs []int32, trialExperiment map[int32]int32,
) (exps, trials []int32) {
	exps = sortedUniqueIDs(expIDs)
	for _, tid := range sortedUniqueIDs(trialIDs) {
		if expID, ok := trialExperiment[tid]; ok && slices.Contains(exps, expID) {
			continue
		}
		trials = append(trials, tid)
	}
	return exps, trials
}

// tensorBoardStorageLocation returns where a checkpoint storage keeps TensorBoard logs. An invalid
// shared_fs storage returns its error as is, for the caller to name the experiment; an unknown
// storage type returns an Internal status error. The save_* options and the credentials of the
// endpoint_url and account_url (see tensorBoardURLParts) are no part of the storage's identity.
func tensorBoardStorageLocation(cs expconf.CheckpointStorageConfig) (tensorBoardLocation, error) {
	switch c := cs.GetUnionMember().(type) {
	case expconf.SharedFSConfig:
		base, err := c.PathInContainer()
		if err != nil {
			return tensorBoardLocation{}, err
		}
		return tensorBoardLocation{
			Type:             "shared_fs",
			BasePath:         base,
			Key:              "shared_fs\x00" + c.HostPath() + "\x00" + base,
			SharedFSHostPath: c.HostPath(),
		}, nil

	case expconf.DirectoryConfig:
		return tensorBoardLocation{
			Type:     "directory",
			BasePath: c.ContainerPath(),
			Key:      "directory\x00" + c.ContainerPath(),
		}, nil

	case expconf.S3Config:
		endpoint, _ := tensorBoardURLParts(c.EndpointURL())
		base := fmt.Sprintf("s3://%s", c.Bucket())
		if prefix := c.Prefix(); prefix != nil {
			base = fmt.Sprintf("s3://%s", filepath.Join(c.Bucket(), *prefix))
		}
		return tensorBoardLocation{
			Type:     "s3",
			BasePath: base,
			Key: "s3\x00" + c.Bucket() + "\x00" + stringValue(c.Prefix()) + "\x00" +
				endpoint,
		}, nil

	case expconf.AzureConfig:
		account, _ := tensorBoardURLParts(c.AccountURL())
		return tensorBoardLocation{
			Type:     "azure",
			BasePath: "azure://" + c.Container(),
			Key:      "azure\x00" + c.Container() + "\x00" + account,
		}, nil

	case expconf.GCSConfig:
		base := fmt.Sprintf("gs://%s", c.Bucket())
		if prefix := c.Prefix(); prefix != nil {
			base = fmt.Sprintf("gs://%s", filepath.Join(c.Bucket(), *prefix))
		}
		return tensorBoardLocation{
			Type:     "gcs",
			BasePath: base,
			Key:      "gcs\x00" + c.Bucket() + "\x00" + stringValue(c.Prefix()),
		}, nil

	default:
		return tensorBoardLocation{}, status.Errorf(codes.Internal,
			"unknown storage backend for experiment: %T", c)
	}
}

// checkTensorBoardSourceCredentials refuses a user who reads, by the storages of the source
// experiments, the inline credentials of an experiment's checkpoint storage, unless the user owns
// the experiment, by owners, or is an admin.
func checkTensorBoardSourceCredentials(
	user model.User,
	owners map[int32]*model.UserID,
	storages map[int32]expconf.CheckpointStorageConfig,
) error {
	ids := maps.Keys(storages)
	slices.Sort(ids)
	for _, id := range ids {
		if tensorBoardInlineCredentials(storages[id]) == nil {
			continue
		}
		owner := owners[id]
		if user.Admin || (owner != nil && *owner == user.ID) {
			continue
		}
		return status.Errorf(codes.PermissionDenied,
			"services: tensorboard: experiment %d: its checkpoint storage has credentials; "+
				"only its owner or an admin may read its logs", id)
	}
	return nil
}

// tensorBoardInlineCredentials returns the credentials that a checkpoint storage holds, or nil when
// it holds none: S3's keys and the credential of its endpoint_url, or Azure's connection string,
// credential and the credential of its account_url (see tensorBoardURLParts). The other storage
// types hold none.
func tensorBoardInlineCredentials(cs expconf.CheckpointStorageConfig) *[3]string {
	var creds [3]string
	switch c := cs.GetUnionMember().(type) {
	case expconf.S3Config:
		_, endpoint := tensorBoardURLParts(c.EndpointURL())
		if c.AccessKey() == nil && c.SecretKey() == nil && endpoint == "" {
			return nil
		}
		creds = [3]string{stringValue(c.AccessKey()), stringValue(c.SecretKey()), endpoint}
	case expconf.AzureConfig:
		_, account := tensorBoardURLParts(c.AccountURL())
		if c.ConnectionString() == nil && c.Credential() == nil && account == "" {
			return nil
		}
		creds = [3]string{stringValue(c.ConnectionString()), stringValue(c.Credential()), account}
	default:
		return nil
	}
	return &creds
}

// tensorBoardURLParts splits an S3 endpoint_url or an Azure account_url into the storage's
// identity, its scheme, host and path, and its credential, its userinfo and query string, which is
// "" when it has neither. Azure takes a SAS token as the query string of account_url, with no other
// credential, and a URL's userinfo may hold a password, so both are credentials, which the identity
// leaves out. A URL that does not parse, or is opaque, as "host:port" without a scheme, is a
// credential as a whole. A missing or empty URL has neither.
func tensorBoardURLParts(raw *string) (key, credential string) {
	if raw == nil || *raw == "" {
		return "", ""
	}
	u, err := url.Parse(*raw)
	if err != nil || u.Opaque != "" {
		return *raw, *raw
	}
	if u.User != nil || u.RawQuery != "" {
		credential = u.User.String() + "?" + u.RawQuery
	}
	return u.Scheme + "://" + u.Host + u.Path, credential
}

// equalTensorBoardCredentials returns whether two results of tensorBoardInlineCredentials are the
// same: both none, or the same values.
func equalTensorBoardCredentials(a, b *[3]string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// tensorBoardSourceRun returns the run name of an experiment's source, or of one of its trials
// when trialID is set: "experiment/<id>" or "experiment/<id>/trial/<tid>".
func tensorBoardSourceRun(experimentID int32, trialID *int32) string {
	if trialID == nil {
		return fmt.Sprintf("experiment/%d", experimentID)
	}
	return fmt.Sprintf("experiment/%d/trial/%d", experimentID, *trialID)
}

// tensorBoardSourcePath returns the log directory of an experiment, or of one of its trials when
// trialID is set, under the base path of its checkpoint storage and the cluster ID:
// "<base>/<cluster>/tensorboard/<run>/".
func tensorBoardSourcePath(base, clusterID string, experimentID int32, trialID *int32) string {
	return fmt.Sprintf("%s/%s/tensorboard/%s/",
		base, clusterID, tensorBoardSourceRun(experimentID, trialID))
}

// tensorBoardSharedFSMount returns the read-only mount of a shared_fs storage's host path at
// expconf.DefaultSharedFSContainerPath, or nil when mounts already mount the host path there. It
// refuses a mount of another host path there.
func tensorBoardSharedFSMount(mounts model.BindMountsConfig, hostPath string) (*model.BindMount, error) {
	for _, m := range mounts {
		if m.ContainerPath != expconf.DefaultSharedFSContainerPath {
			continue
		}
		if m.HostPath == hostPath {
			return nil, nil
		}
		return nil, status.Errorf(codes.InvalidArgument,
			"services: tensorboard: bind_mounts: %s is mounted from another host path",
			expconf.DefaultSharedFSContainerPath)
	}
	mount := model.ToModelBindMount(schemas.WithDefaults(expconf.BindMount{
		RawContainerPath: expconf.DefaultSharedFSContainerPath,
		RawHostPath:      hostPath,
		RawPropagation:   ptrs.Ptr(expconf.DefaultSharedFSPropagation),
		RawReadOnly:      ptrs.Ptr(true),
	}))
	return &mount, nil
}

// tensorBoardSourceFiles returns the files of a tensorboard service's sources, owned by the task's
// user and readable by it alone: tensorBoardSourcesDir first, so that the task's user may create the
// log directory in it, then the checkpoint storage at storageConfPath and the sources, which hold
// no credentials, at tensorBoardSourcesFile.
func tensorBoardSourceFiles(aug *model.AgentUserGroup, s *tensorBoardSources) (archive.Archive, error) {
	content, err := json.Marshal(tensorBoardSourcesFileContent{
		Version:       tensorBoardSourcesVersion,
		StorageType:   s.StorageType,
		StorageConfig: storageConfPath,
		Sources:       s.Sources,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "services: tensorboard: encoding the sources")
	}
	return archive.Archive{
		aug.OwnedArchiveItem(tensorBoardSourcesDir, nil, 0o700, tar.TypeDir),
		aug.OwnedArchiveItem(storageConfPath, s.StorageConfig, 0o600, tar.TypeReg),
		aug.OwnedArchiveItem(tensorBoardSourcesFile, content, 0o600, tar.TypeReg),
	}, nil
}
