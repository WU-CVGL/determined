//go:build integration
// +build integration

package internal

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/registry"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// tensorboardImageCreds is what a TensorBoard can inherit from an experiment: its image, its
// image pull secrets and its registry_auth.
type tensorboardImageCreds struct {
	image       model.RuntimeItem
	pullSecrets []k8sV1.LocalObjectReference
	auth        *registry.AuthConfig
}

func experimentImageCreds(name string) tensorboardImageCreds {
	image := "registry.example.com/" + name + "/train:1"
	return tensorboardImageCreds{
		image:       model.RuntimeItem{CPU: image, CUDA: image, ROCM: image},
		pullSecrets: []k8sV1.LocalObjectReference{{Name: name + "-pull"}},
		auth: &registry.AuthConfig{
			Username:      name,
			Password:      name + "-registry-password",
			ServerAddress: "registry.example.com",
		},
	}
}

// createTestExpWithImage creates an experiment of owner that sets the image, pull secrets and
// registry_auth of experimentImageCreds(name).
//
// nolint: exhaustruct
func createTestExpWithImage(
	t *testing.T, api *apiServer, owner model.User, name string,
) *model.Experiment {
	creds := experimentImageCreds(name)
	conf := expconf.ExperimentConfig{
		RawName: expconf.Name{RawString: &name},
		RawEnvironment: &expconf.EnvironmentConfigV0{
			RawImage: &expconf.EnvironmentImageMapV0{
				RawCPU: &creds.image.CPU, RawCUDA: &creds.image.CUDA, RawROCM: &creds.image.ROCM,
			},
			RawRegistryAuth: creds.auth,
			RawPodSpec: &expconf.PodSpec{Spec: k8sV1.PodSpec{
				ImagePullSecrets: creds.pullSecrets,
			}},
		},
	}
	activeConfig := schemas.WithDefaults(schemas.Merge(minExpConfig, conf))
	return createTestExpWithActiveConfig(t, api, owner, 1, activeConfig)
}

// launchedImageCreds returns the image, pull secrets and registry_auth of a launched TensorBoard.
func launchedImageCreds(t *testing.T, resp *apiv1.LaunchTensorboardResponse) tensorboardImageCreds {
	raw, err := protojson.Marshal(resp.Config)
	require.NoError(t, err)
	var conf struct {
		Environment model.Environment `json:"environment"`
	}
	require.NoError(t, json.Unmarshal(raw, &conf))
	creds := tensorboardImageCreds{image: conf.Environment.Image, auth: conf.Environment.RegistryAuth}
	if conf.Environment.PodSpec != nil {
		creds.pullSecrets = conf.Environment.PodSpec.Spec.ImagePullSecrets
	}
	return creds
}

func TestLaunchTensorboardInheritsImageOnlyFromOwnExperiment(t *testing.T) {
	mockRM := MockRM()
	mockRM.On("SmallerValueIsHigherPriority", mock.Anything).Return(true, nil)
	api, admin, _ := setupAPITest(t, nil, mockRM)
	require.True(t, admin.Admin)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)

	alice := db.RequireMockUser(t, api.m.db)
	bob := db.RequireMockUser(t, api.m.db)

	// Experiment IDs increase in this order. A TensorBoard inherits from the newest experiment it
	// shows.
	bobOlderExp := createTestExpWithImage(t, api, bob, "bob-older")
	aliceExp := createTestExpWithImage(t, api, alice, "alice")
	aliceTrialID := int32(db.RequireMockTrialID(t, api.m.db, aliceExp))
	bobNewerExp := createTestExpWithImage(t, api, bob, "bob-newer")

	// What a TensorBoard gets when it inherits nothing: the task container defaults.
	notInherited := tensorboardImageCreds{
		image: model.DefaultEnvConfig(&api.m.config.TaskContainerDefaults).Image,
	}
	customImage := "registry.example.com/tensorboard/custom:1"
	customConfig, err := structpb.NewStruct(map[string]any{
		"environment": map[string]any{"image": customImage},
	})
	require.NoError(t, err)

	cases := []struct {
		name     string
		launcher model.User
		req      *apiv1.LaunchTensorboardRequest
		want     tensorboardImageCreds
	}{
		{
			"owner inherits from their experiment", alice,
			&apiv1.LaunchTensorboardRequest{ExperimentIds: []int32{int32(aliceExp.ID)}},
			experimentImageCreds("alice"),
		},
		{
			"owner inherits from their trial", alice,
			&apiv1.LaunchTensorboardRequest{TrialIds: []int32{aliceTrialID}},
			experimentImageCreds("alice"),
		},
		{
			"another user inherits nothing", bob,
			&apiv1.LaunchTensorboardRequest{ExperimentIds: []int32{int32(aliceExp.ID)}},
			notInherited,
		},
		{
			"an admin inherits nothing", admin,
			&apiv1.LaunchTensorboardRequest{ExperimentIds: []int32{int32(aliceExp.ID)}},
			notInherited,
		},
		{
			"another user inherits nothing from a trial", bob,
			&apiv1.LaunchTensorboardRequest{TrialIds: []int32{aliceTrialID}},
			notInherited,
		},
		{
			"another user's newest experiment is not inherited", bob,
			&apiv1.LaunchTensorboardRequest{
				ExperimentIds: []int32{int32(aliceExp.ID), int32(bobOlderExp.ID)},
			},
			notInherited,
		},
		{
			"the launcher's own newest experiment is inherited", bob,
			&apiv1.LaunchTensorboardRequest{
				ExperimentIds: []int32{int32(bobNewerExp.ID)}, TrialIds: []int32{aliceTrialID},
			},
			experimentImageCreds("bob-newer"),
		},
		{
			"another user's custom image is kept, without the experiment's credentials", bob,
			&apiv1.LaunchTensorboardRequest{
				ExperimentIds: []int32{int32(aliceExp.ID)}, Config: customConfig,
			},
			tensorboardImageCreds{
				image: model.RuntimeItem{CPU: customImage, CUDA: customImage, ROCM: customImage},
			},
		},
		{
			"the owner's custom image is kept, with the experiment's registry_auth", alice,
			&apiv1.LaunchTensorboardRequest{
				ExperimentIds: []int32{int32(aliceExp.ID)}, Config: customConfig,
			},
			tensorboardImageCreds{
				image: model.RuntimeItem{CPU: customImage, CUDA: customImage, ROCM: customImage},
				auth:  experimentImageCreds("alice").auth,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := api.LaunchTensorboard(ntscUserCtx(t, c.launcher), c.req)
			require.NoError(t, err)
			require.Equal(t, c.want, launchedImageCreds(t, resp))
		})
	}
}
