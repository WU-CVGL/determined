package internal

import (
	"archive/tar"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

func s3Storage(s3 expconf.S3Config) expconf.CheckpointStorageConfig {
	return expconf.CheckpointStorageConfig{RawS3Config: &s3}
}

func azureStorage(azure expconf.AzureConfig) expconf.CheckpointStorageConfig {
	return expconf.CheckpointStorageConfig{RawAzureConfig: &azure}
}

func sharedFSStorage(hostPath string, storagePath *string) expconf.CheckpointStorageConfig {
	return expconf.CheckpointStorageConfig{RawSharedFSConfig: &expconf.SharedFSConfig{
		RawHostPath:    ptrs.Ptr(hostPath),
		RawStoragePath: storagePath,
		RawPropagation: ptrs.Ptr(expconf.DefaultSharedFSPropagation),
	}}
}

func TestCanonicalTensorBoardIDs(t *testing.T) {
	trialExperiment := map[int32]int32{40: 13, 41: 13, 50: 12, 51: 12, 60: 14}
	exps, trials := canonicalTensorBoardIDs(
		[]int32{14, 12, 14}, []int32{41, 50, 40, 41, 60, 51}, trialExperiment)
	// Experiments 12 and 14 hold all their trials, so their trials are dropped.
	require.Equal(t, []int32{12, 14}, exps)
	require.Equal(t, []int32{40, 41}, trials)

	exps, trials = canonicalTensorBoardIDs(nil, []int32{41, 40}, trialExperiment)
	require.Nil(t, exps)
	require.Equal(t, []int32{40, 41}, trials)

	exps, trials = canonicalTensorBoardIDs([]int32{3, 1, 2, 1}, nil, nil)
	require.Equal(t, []int32{1, 2, 3}, exps)
	require.Nil(t, trials)
}

func TestTensorBoardStorageLocation(t *testing.T) {
	cases := []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		want    tensorBoardLocation
	}{
		{
			name:    "s3",
			storage: s3Storage(expconf.S3Config{RawBucket: ptrs.Ptr("b")}),
			want:    tensorBoardLocation{Type: "s3", BasePath: "s3://b", Key: "s3\x00b\x00\x00"},
		},
		{
			name: "s3 with prefix and endpoint",
			storage: s3Storage(expconf.S3Config{
				RawBucket: ptrs.Ptr("b"), RawPrefix: ptrs.Ptr("p/q"),
				RawEndpointURL: ptrs.Ptr("http://minio:9000"),
			}),
			want: tensorBoardLocation{
				Type: "s3", BasePath: "s3://b/p/q", Key: "s3\x00b\x00p/q\x00http://minio:9000",
			},
		},
		{
			name: "gcs",
			storage: expconf.CheckpointStorageConfig{RawGCSConfig: &expconf.GCSConfig{
				RawBucket: ptrs.Ptr("g"),
			}},
			want: tensorBoardLocation{Type: "gcs", BasePath: "gs://g", Key: "gcs\x00g\x00"},
		},
		{
			name: "gcs with prefix",
			storage: expconf.CheckpointStorageConfig{RawGCSConfig: &expconf.GCSConfig{
				RawBucket: ptrs.Ptr("g"), RawPrefix: ptrs.Ptr("p"),
			}},
			want: tensorBoardLocation{Type: "gcs", BasePath: "gs://g/p", Key: "gcs\x00g\x00p"},
		},
		{
			name: "azure",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr("https://a.example"),
			}),
			want: tensorBoardLocation{
				Type: "azure", BasePath: "azure://c", Key: "azure\x00c\x00https://a.example",
			},
		},
		{
			name:    "shared_fs",
			storage: sharedFSStorage("/mnt/ckpt", nil),
			want: tensorBoardLocation{
				Type:             "shared_fs",
				BasePath:         expconf.DefaultSharedFSContainerPath,
				Key:              "shared_fs\x00/mnt/ckpt\x00" + expconf.DefaultSharedFSContainerPath,
				SharedFSHostPath: "/mnt/ckpt",
			},
		},
		{
			name:    "shared_fs with storage_path",
			storage: sharedFSStorage("/mnt/ckpt", ptrs.Ptr("runs")),
			want: tensorBoardLocation{
				Type:             "shared_fs",
				BasePath:         expconf.DefaultSharedFSContainerPath + "/runs",
				Key:              "shared_fs\x00/mnt/ckpt\x00" + expconf.DefaultSharedFSContainerPath + "/runs",
				SharedFSHostPath: "/mnt/ckpt",
			},
		},
		{
			name: "directory",
			storage: expconf.CheckpointStorageConfig{RawDirectoryConfig: &expconf.DirectoryConfig{
				RawContainerPath: ptrs.Ptr("/ckpt"),
			}},
			want: tensorBoardLocation{Type: "directory", BasePath: "/ckpt", Key: "directory\x00/ckpt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc, err := tensorBoardStorageLocation(tc.storage)
			require.NoError(t, err)
			require.Equal(t, tc.want, loc)
		})
	}

	key := func(cs expconf.CheckpointStorageConfig) string {
		loc, err := tensorBoardStorageLocation(cs)
		require.NoError(t, err)
		return loc.Key
	}
	base := s3Storage(expconf.S3Config{
		RawBucket: ptrs.Ptr("b"), RawPrefix: ptrs.Ptr("p"), RawEndpointURL: ptrs.Ptr("http://e"),
	})

	// The save_* options and the credentials are no part of the key.
	saved := base
	saved.RawSaveExperimentBest = ptrs.Ptr(3)
	saved.RawSaveTrialBest = ptrs.Ptr(2)
	saved.RawSaveTrialLatest = ptrs.Ptr(1)
	require.Equal(t, key(base), key(saved))
	require.Equal(t, key(base), key(s3Storage(expconf.S3Config{
		RawBucket: ptrs.Ptr("b"), RawPrefix: ptrs.Ptr("p"), RawEndpointURL: ptrs.Ptr("http://e"),
		RawAccessKey: ptrs.Ptr("k"),
	})))

	for name, other := range map[string]expconf.CheckpointStorageConfig{
		"bucket": s3Storage(expconf.S3Config{
			RawBucket: ptrs.Ptr("b2"), RawPrefix: ptrs.Ptr("p"), RawEndpointURL: ptrs.Ptr("http://e"),
		}),
		"prefix": s3Storage(expconf.S3Config{
			RawBucket: ptrs.Ptr("b"), RawPrefix: ptrs.Ptr("p2"), RawEndpointURL: ptrs.Ptr("http://e"),
		}),
		"endpoint": s3Storage(expconf.S3Config{
			RawBucket: ptrs.Ptr("b"), RawPrefix: ptrs.Ptr("p"), RawEndpointURL: ptrs.Ptr("http://e2"),
		}),
	} {
		require.NotEqual(t, key(base), key(other), name)
	}
	require.NotEqual(t, key(sharedFSStorage("/a", nil)), key(sharedFSStorage("/b", nil)))

	// The userinfo and query string of the endpoint_url and account_url are credentials, no part of
	// the key; another host is.
	azure := func(accountURL string) expconf.CheckpointStorageConfig {
		return azureStorage(expconf.AzureConfig{
			RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr(accountURL),
		})
	}
	require.Equal(t, key(azure("https://a.example/")), key(azure("https://a.example/?sv=1&sig=s1")))
	require.Equal(t, key(azure("https://a.example/?sv=1&sig=s1")), key(azure("https://a.example/?sig=s2")))
	require.Equal(t, key(azure("https://a.example/")), key(azure("https://u:p@a.example/")))
	require.NotEqual(t, key(azure("https://a.example/")), key(azure("https://b.example/")))
	require.NotEqual(t, key(azure("https://a.example/?sig=s1")), key(azure("https://b.example/?sig=s1")))
	s3 := func(endpoint string) expconf.CheckpointStorageConfig {
		return s3Storage(expconf.S3Config{RawBucket: ptrs.Ptr("b"), RawEndpointURL: ptrs.Ptr(endpoint)})
	}
	require.Equal(t, key(s3("http://e:9000")), key(s3("http://u:p@e:9000")))
	require.Equal(t, key(s3("http://e:9000")), key(s3("http://e:9000?token=t")))
	require.NotEqual(t, key(s3("http://e:9000")), key(s3("http://e2:9000")))
	require.NotEqual(t, key(s3("http://u:p@e:9000")), key(s3("http://u:p@e2:9000")))

	// An invalid shared_fs storage returns its plain error, for the caller to name the experiment.
	_, err := tensorBoardStorageLocation(expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{},
	})
	require.Error(t, err)
	require.Equal(t, codes.Unknown, status.Code(err))
}

func TestTensorBoardInlineCredentials(t *testing.T) {
	cases := []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		want    *[3]string
	}{
		{
			name: "s3 keys",
			storage: s3Storage(expconf.S3Config{
				RawBucket: ptrs.Ptr("b"), RawAccessKey: ptrs.Ptr("a"), RawSecretKey: ptrs.Ptr("s"),
			}),
			want: &[3]string{"a", "s", ""},
		},
		{
			name: "s3 endpoint only",
			storage: s3Storage(expconf.S3Config{
				RawBucket: ptrs.Ptr("b"), RawEndpointURL: ptrs.Ptr("http://e"),
			}),
		},
		{
			name: "s3 endpoint with userinfo",
			storage: s3Storage(expconf.S3Config{
				RawBucket: ptrs.Ptr("b"), RawEndpointURL: ptrs.Ptr("http://u:p@e:9000"),
			}),
			want: &[3]string{"", "", "u:p?"},
		},
		{
			name: "s3 endpoint with query",
			storage: s3Storage(expconf.S3Config{
				RawBucket: ptrs.Ptr("b"), RawEndpointURL: ptrs.Ptr("http://e:9000?token=t"),
			}),
			want: &[3]string{"", "", "?token=t"},
		},
		{
			name: "azure connection_string",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawConnectionString: ptrs.Ptr("cs"),
			}),
			want: &[3]string{"cs", "", ""},
		},
		{
			name: "azure credential",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr("https://a"),
				RawCredential: ptrs.Ptr("cred"),
			}),
			want: &[3]string{"", "cred", ""},
		},
		{
			name: "azure account_url only",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr("https://a.blob.example/"),
			}),
		},
		{
			name: "azure account_url with a SAS",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer:  ptrs.Ptr("c"),
				RawAccountURL: ptrs.Ptr("https://a.blob.example/?sv=2022-11-02&sig=s"),
			}),
			want: &[3]string{"", "", "?sv=2022-11-02&sig=s"},
		},
		{
			name: "azure account_url with userinfo",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr("https://u:p@a.blob.example/"),
			}),
			want: &[3]string{"", "", "u:p?"},
		},
		{
			name: "azure account_url that does not parse",
			storage: azureStorage(expconf.AzureConfig{
				RawContainer: ptrs.Ptr("c"), RawAccountURL: ptrs.Ptr("https://a%zz"),
			}),
			want: &[3]string{"", "", "https://a%zz"},
		},
		{
			name: "gcs",
			storage: expconf.CheckpointStorageConfig{RawGCSConfig: &expconf.GCSConfig{
				RawBucket: ptrs.Ptr("g"),
			}},
		},
		{name: "shared_fs", storage: sharedFSStorage("/mnt/ckpt", nil)},
		{
			name: "directory",
			storage: expconf.CheckpointStorageConfig{RawDirectoryConfig: &expconf.DirectoryConfig{
				RawContainerPath: ptrs.Ptr("/ckpt"),
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tensorBoardInlineCredentials(tc.storage))
		})
	}
}

func TestCheckTensorBoardSourceCredentials(t *testing.T) {
	owner := model.UserID(7)
	storages := map[int32]expconf.CheckpointStorageConfig{
		3: sharedFSStorage("/mnt/ckpt", nil),
		5: s3Storage(expconf.S3Config{RawBucket: ptrs.Ptr("b"), RawSecretKey: ptrs.Ptr("s")}),
	}
	owners := map[int32]*model.UserID{3: nil, 5: &owner}

	require.NoError(t, checkTensorBoardSourceCredentials(model.User{ID: owner}, owners, storages))
	require.NoError(t, checkTensorBoardSourceCredentials(
		model.User{ID: 8, Admin: true}, owners, storages))
	err := checkTensorBoardSourceCredentials(model.User{ID: 8}, owners, storages)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Equal(t, "services: tensorboard: experiment 5: its checkpoint storage has credentials; "+
		"only its owner or an admin may read its logs", status.Convert(err).Message())
}

func TestTensorBoardSourcePath(t *testing.T) {
	require.Equal(t, "s3://b/p/c1/tensorboard/experiment/12/",
		tensorBoardSourcePath("s3://b/p", "c1", 12, nil))
	require.Equal(t, "/determined_shared_fs/c1/tensorboard/experiment/13/trial/40/",
		tensorBoardSourcePath("/determined_shared_fs", "c1", 13, ptrs.Ptr(int32(40))))
	require.Equal(t, "experiment/12", tensorBoardSourceRun(12, nil))
	require.Equal(t, "experiment/13/trial/40", tensorBoardSourceRun(13, ptrs.Ptr(int32(40))))

	// The run is the path after "<base>/<cluster>/tensorboard/", without the trailing slash.
	path := tensorBoardSourcePath("gs://g", "c1", 13, ptrs.Ptr(int32(40)))
	require.Equal(t, tensorBoardSourceRun(13, ptrs.Ptr(int32(40)))+"/",
		strings.TrimPrefix(path, "gs://g/c1/tensorboard/"))
}

func TestTensorBoardSharedFSMount(t *testing.T) {
	mount, err := tensorBoardSharedFSMount(nil, "/mnt/ckpt")
	require.NoError(t, err)
	require.Equal(t, &model.BindMount{
		HostPath:      "/mnt/ckpt",
		ContainerPath: expconf.DefaultSharedFSContainerPath,
		ReadOnly:      true,
		Propagation:   expconf.DefaultSharedFSPropagation,
	}, mount)

	existing := model.BindMountsConfig{
		{HostPath: "/data", ContainerPath: "/data"},
		{HostPath: "/mnt/ckpt", ContainerPath: expconf.DefaultSharedFSContainerPath},
	}
	mount, err = tensorBoardSharedFSMount(existing, "/mnt/ckpt")
	require.NoError(t, err)
	require.Nil(t, mount)

	_, err = tensorBoardSharedFSMount(existing, "/mnt/other")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t,
		"services: tensorboard: bind_mounts: /determined_shared_fs is mounted from another host path",
		status.Convert(err).Message())
}

func TestTensorBoardSourceFiles(t *testing.T) {
	const accessKey, secretKey = "SENTINEL-ACCESS-KEY", "SENTINEL-SECRET-KEY"
	storage := s3Storage(expconf.S3Config{
		RawBucket: ptrs.Ptr("b"), RawAccessKey: ptrs.Ptr(accessKey), RawSecretKey: ptrs.Ptr(secretKey),
	})
	storageConfig, err := json.Marshal(storage)
	require.NoError(t, err)
	sources := &tensorBoardSources{
		ExperimentIDs: []int32{12},
		TrialIDs:      []int32{40},
		StorageType:   "s3",
		Sources: []tensorBoardSource{
			{
				Run: "experiment/12", ExperimentID: 12,
				Path: tensorBoardSourcePath("s3://b", "c1", 12, nil),
			},
			{
				Run: "experiment/13/trial/40", ExperimentID: 13, TrialID: ptrs.Ptr(int32(40)),
				Path: tensorBoardSourcePath("s3://b", "c1", 13, ptrs.Ptr(int32(40))),
			},
		},
		StorageConfig: storageConfig,
	}
	files, err := tensorBoardSourceFiles(&model.AgentUserGroup{UID: 1000, GID: 1001}, sources)
	require.NoError(t, err)
	require.Len(t, files, 3)

	// The directory first, owned by the task's user, then the files.
	dir := files[0]
	require.Equal(t, tensorBoardSourcesDir, dir.Path)
	require.Equal(t, byte(tar.TypeDir), dir.Type)
	require.Equal(t, os.FileMode(0o700), dir.FileMode)
	require.Equal(t, 1000, dir.UserID)
	require.Equal(t, 1001, dir.GroupID)
	byPath := map[string][]byte{}
	for _, f := range files[1:] {
		require.Equal(t, byte(tar.TypeReg), f.Type, f.Path)
		require.Equal(t, os.FileMode(0o600), f.FileMode, f.Path)
		require.Equal(t, 1000, f.UserID, f.Path)
		require.Equal(t, 1001, f.GroupID, f.Path)
		byPath[f.Path] = f.Content
	}
	require.Equal(t, storageConfig, byPath[storageConfPath])

	content := byPath[tensorBoardSourcesFile]
	require.NotContains(t, string(content), accessKey)
	require.NotContains(t, string(content), secretKey)
	require.JSONEq(t, `{
		"version": 1,
		"storage_type": "s3",
		"storage_config": "/run/determined/tensorboard/storage_config.json",
		"sources": [
			{"run": "experiment/12", "experiment_id": 12,
			 "path": "s3://b/c1/tensorboard/experiment/12/"},
			{"run": "experiment/13/trial/40", "experiment_id": 13, "trial_id": 40,
			 "path": "s3://b/c1/tensorboard/experiment/13/trial/40/"}
		]
	}`, string(content))
}

func TestRequestDisablesSSH(t *testing.T) {
	const refusal = "services: ssh: can be disabled only with resources.slots 0"
	cases := []struct {
		name    string
		config  map[string]any
		want    bool
		refused bool
	}{
		{name: "no config"},
		{name: "ssh enabled", config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": true}},
			"resources": map[string]any{"slots": 2},
		}},
		{name: "ssh without enabled", config: map[string]any{
			"services": map[string]any{"ssh": map[string]any{}},
		}},
		{name: "ssh disabled, slots absent", want: true, config: map[string]any{
			"services": map[string]any{"ssh": map[string]any{"enabled": false}},
		}},
		{name: "ssh disabled, slots null", want: true, config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": false}},
			"resources": map[string]any{"slots": nil},
		}},
		{name: "ssh disabled, slots 0", want: true, config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": false}},
			"resources": map[string]any{"slots": 0},
		}},
		{name: "ssh disabled, slots 1", refused: true, config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": false}},
			"resources": map[string]any{"slots": 1},
		}},
		{name: "ssh disabled, slots not a number", refused: true, config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": false}},
			"resources": map[string]any{"slots": "1"},
		}},
		{name: "enabled not a bool", config: map[string]any{
			"services":  map[string]any{"ssh": map[string]any{"enabled": "false"}},
			"resources": map[string]any{"slots": 1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *structpb.Struct
			if tc.config != nil {
				var err error
				cfg, err = structpb.NewStruct(tc.config)
				require.NoError(t, err)
			}
			got, err := requestDisablesSSH(cfg)
			if tc.refused {
				require.Equal(t, codes.InvalidArgument, status.Code(err))
				require.Equal(t, refusal, status.Convert(err).Message())
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAddShellTensorBoardSources(t *testing.T) {
	const secretKey = "SENTINEL-SECRET-KEY"
	storage := s3Storage(expconf.S3Config{
		RawBucket: ptrs.Ptr("b"), RawAccessKey: ptrs.Ptr("a"), RawSecretKey: ptrs.Ptr(secretKey),
	})
	storageConfig, err := json.Marshal(storage)
	require.NoError(t, err)
	sources := &tensorBoardSources{
		ExperimentIDs: []int32{12},
		StorageType:   "s3",
		Sources: []tensorBoardSource{{
			Run: "experiment/12", ExperimentID: 12,
			Path: tensorBoardSourcePath("s3://b", "c1", 12, nil),
		}},
		StorageConfig: storageConfig,
	}

	spec := &tasks.GenericCommandSpec{}
	spec.Base.AgentUserGroup = &model.AgentUserGroup{UID: 1000, GID: 1000}
	spec.Config.Services = map[string]model.ServiceConfig{
		model.ServiceSSH:         {Enabled: ptrs.Ptr(false)},
		model.ServiceTensorBoard: {ExperimentIDs: []int32{12}},
	}
	files, err := addShellTensorBoard(spec, sources)
	require.NoError(t, err)
	require.Len(t, files, 3)

	service, ok := spec.Service(model.ServiceTensorBoard)
	require.True(t, ok)
	require.True(t, service.Exec)
	require.True(t, service.SourcesCheck)
	env := spec.Base.ExtraEnvVars
	require.Equal(t, tensorBoardSourcesLogDir, env[tensorBoardLogDirEnvVar])
	require.Equal(t, tensorBoardSourcesFile, env[tensorBoardSourcesEnvVar])
	// The storage's credentials are in its file alone, not in the environment.
	for key, value := range env {
		require.False(t, strings.HasPrefix(key, "AWS_") || strings.HasPrefix(key, "S3_"), key)
		require.NotContains(t, value, secretKey, key)
	}

	// A logdir service has no sources: no files, no sources env, no sources check.
	spec = &tasks.GenericCommandSpec{}
	spec.Config.Services = map[string]model.ServiceConfig{
		model.ServiceTensorBoard: {LogDir: "/logs"},
	}
	files, err = addShellTensorBoard(spec, nil)
	require.NoError(t, err)
	require.Empty(t, files)
	service, ok = spec.Service(model.ServiceTensorBoard)
	require.True(t, ok)
	require.False(t, service.SourcesCheck)
	require.Equal(t, "/logs", spec.Base.ExtraEnvVars[tensorBoardLogDirEnvVar])
	require.NotContains(t, spec.Base.ExtraEnvVars, tensorBoardSourcesEnvVar)

	spec = &tasks.GenericCommandSpec{}
	spec.Config.Services = map[string]model.ServiceConfig{model.ServiceTensorBoard: {}}
	_, err = addShellTensorBoard(spec, nil)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "services: tensorboard: logdir, experiment_ids or trial_ids is required",
		status.Convert(err).Message())
}
