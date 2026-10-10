/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"testing"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/introspection"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/internal/cri/annotations"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	imagestore "github.com/containerd/containerd/v2/internal/cri/store/image"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	osinterface "github.com/containerd/containerd/v2/pkg/os"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/containerd/v2/plugins/snapshots/native"
	"github.com/containerd/platforms"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var errSnapshotIdentityOptionsComplete = errors.New("container options applied")

// Stop after all real container options, before persisting a container or starting a task.
type snapshotIdentityContainerStore struct {
	containers.Store
	container *containers.Container
}

func (s *snapshotIdentityContainerStore) Create(_ context.Context, c containers.Container) (containers.Container, error) {
	s.container = &c
	return containers.Container{}, errSnapshotIdentityOptionsComplete
}

type snapshotIdentityImageService struct {
	fakeImageService
	image imagestore.Image
}

func (s *snapshotIdentityImageService) LocalResolve(string) (imagestore.Image, error) {
	return s.image, nil
}
func (*snapshotIdentityImageService) RuntimeSnapshotter(context.Context, criconfig.Runtime) string {
	return "test"
}
func (*snapshotIdentityImageService) Config() criconfig.ImageConfig {
	return criconfig.ImageConfig{DisableSnapshotAnnotations: true}
}

type snapshotIdentitySandboxService struct{ fakeSandboxService }

func (*snapshotIdentitySandboxService) SandboxStatus(_ context.Context, _ string, id string, _ bool) (sandbox.ControllerStatus, error) {
	return sandbox.ControllerStatus{SandboxID: id, Pid: 1234}, nil
}

type snapshotIdentityIntrospection struct{ introspection.Service }

func (*snapshotIdentityIntrospection) Plugins(context.Context, ...string) (*introspectionapi.PluginsResponse, error) {
	return &introspectionapi.PluginsResponse{Plugins: []*introspectionapi.Plugin{{ID: "test", Capabilities: []string{"remap-ids"}}}}, nil
}

type snapshotIdentityBackend struct {
	snapshots.Snapshotter
	labels      map[string]string
	key, parent string
	calls       int
}

func (s *snapshotIdentityBackend) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	if parent != "" {
		var info snapshots.Info
		for _, opt := range opts {
			if err := opt(&info); err != nil {
				return nil, err
			}
		}
		s.labels, s.key, s.parent = maps.Clone(info.Labels), key, parent
		s.calls++
	}
	return s.Snapshotter.Prepare(ctx, key, parent, opts...)
}

func TestCreateContainerSnapshotIdentity(t *testing.T) {
	for _, tc := range []struct {
		attempt   uint32
		decimal   string
		remap     bool
		userImage string
	}{
		{attempt: 0, decimal: "0"},
		{attempt: 10, decimal: "10", userImage: "requested-image"},
		{attempt: 4294967295, decimal: "4294967295", remap: true},
	} {
		t.Run(tc.decimal, func(t *testing.T) {
			ctx := namespaces.WithNamespace(t.Context(), "snapshot-identity-test")
			root := t.TempDir()
			sn, err := native.NewSnapshotter(filepath.Join(root, "native"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sn.Close()) })
			backend := &snapshotIdentityBackend{Snapshotter: sn}
			cs, err := local.NewStore(filepath.Join(root, "content"))
			require.NoError(t, err)
			bdb, err := bolt.Open(filepath.Join(root, "metadata.db"), 0600, nil)
			require.NoError(t, err)
			db := metadata.NewDB(bdb, cs, map[string]snapshots.Snapshotter{"test": backend})
			require.NoError(t, db.Init(ctx))
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			store := &snapshotIdentityContainerStore{Store: metadata.NewContainerStore(db)}
			client, err := containerd.New("", containerd.WithServices(
				containerd.WithContentStore(db.ContentStore()),
				containerd.WithImageStore(metadata.NewImageStore(db)),
				containerd.WithSnapshotters(db.Snapshotters()),
				containerd.WithSandboxStore(metadata.NewSandboxStore(db)),
				containerd.WithContainerStore(store),
				containerd.WithLeasesService(metadata.NewLeaseManager(db)),
				containerd.WithIntrospectionService(&snapshotIdentityIntrospection{}),
			))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })

			config, sandboxConfig, imageConfig, _ := getCreateContainerTestData()
			config.Mounts = nil
			config.Metadata.Attempt = tc.attempt
			config.Image.UserSpecifiedImage = tc.userImage
			config.Linux.SecurityContext.SupplementalGroupsPolicy = runtime.SupplementalGroupsPolicy_Strict
			config.Linux.SecurityContext.Apparmor = &runtime.SecurityProfile{ProfileType: runtime.SecurityProfile_Unconfined}
			if tc.remap {
				userns := &runtime.UserNamespace{Mode: runtime.NamespaceMode_POD,
					Uids: []*runtime.IDMapping{{HostId: 100000, ContainerId: 0, Length: 65536}},
					Gids: []*runtime.IDMapping{{HostId: 200000, ContainerId: 0, Length: 65536}}}
				config.Linux.SecurityContext.NamespaceOptions = &runtime.NamespaceOption{UsernsOptions: userns}
				sandboxConfig.Linux.SecurityContext.NamespaceOptions = &runtime.NamespaceOption{UsernsOptions: userns}
			}
			canonical := annotations.DefaultCRISnapshotLabelsForContainer("sandbox-id", config.Metadata.Name, "image", sandboxConfig, "id", tc.attempt)
			imageConfig.Labels = make(map[string]string)
			for key := range canonical {
				config.Annotations[key] = "forged-container-value"
				sandboxConfig.Annotations[key] = "forged-pod-value"
				imageConfig.Labels[key] = "forged-image-value"
			}

			// A committed image parent exercises rootfs creation without an unpack or mount.
			diffID := digest.FromString("snapshot-identity-empty-layer")
			imageSpec := imagespec.Image{Platform: platforms.DefaultSpec(), Config: *imageConfig,
				RootFS: imagespec.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}}}
			writeJSON := func(value any, mediaType string) imagespec.Descriptor {
				data, err := json.Marshal(value)
				require.NoError(t, err)
				desc := imagespec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
				require.NoError(t, content.WriteBlob(ctx, db.ContentStore(), desc.Digest.String(), bytes.NewReader(data), desc))
				return desc
			}
			configDesc := writeJSON(imageSpec, imagespec.MediaTypeImageConfig)
			manifest := imagespec.Manifest{Config: configDesc, Layers: []imagespec.Descriptor{{MediaType: imagespec.MediaTypeImageLayer, Digest: diffID}}}
			manifest.SchemaVersion = 2
			target := writeJSON(manifest, imagespec.MediaTypeImageManifest)
			imageName := "example.org/snapshot-identity:test"
			_, err = client.ImageService().Create(ctx, images.Image{Name: imageName, Target: target})
			require.NoError(t, err)
			parent := identity.ChainID([]digest.Digest{diffID}).String()
			snapshotter := client.SnapshotService("test")
			_, err = snapshotter.Prepare(ctx, "fixture-unpack", "")
			require.NoError(t, err)
			require.NoError(t, snapshotter.Commit(ctx, parent, "fixture-unpack"))

			_, err = client.SandboxStore().Create(ctx, sandbox.Sandbox{ID: "sandbox-id", Sandboxer: "shim", Runtime: sandbox.RuntimeOpts{Name: "io.containerd.runc.v2"}})
			require.NoError(t, err)
			c := newTestCRIService()
			c.client = client
			c.config.RootDir, c.config.StateDir = filepath.Join(root, "cri-root"), filepath.Join(root, "cri-state")
			c.config.Runtimes = map[string]criconfig.Runtime{"runc": {Type: "io.containerd.runc.v2", Sandboxer: "shim"}}
			c.os = osinterface.RealOS{}
			c.sandboxService = &snapshotIdentitySandboxService{}
			c.ImageService = &snapshotIdentityImageService{image: imagestore.Image{ID: configDesc.Digest.String(), References: []string{imageName}, ImageSpec: imageSpec}}
			c.runtimeHandlers = map[string]*runtime.RuntimeHandler{"runc": {Features: &runtime.RuntimeHandlerFeatures{UserNamespaces: true}}}
			sb := sandboxstore.NewSandbox(sandboxstore.Metadata{ID: "sandbox-id", Config: sandboxConfig, RuntimeHandler: "runc"}, sandboxstore.Status{State: sandboxstore.StateReady})
			sb.Sandboxer = "shim"
			require.NoError(t, c.sandboxStore.Add(sb))
			requestSandboxConfig := proto.Clone(sandboxConfig).(*runtime.PodSandboxConfig)
			requestSandboxConfig.Metadata.Uid = "duplicate-request-uid"
			requestSandboxConfig.Metadata.Name = "duplicate-request-name"
			requestSandboxConfig.Metadata.Namespace = "duplicate-request-namespace"

			_, err = c.CreateContainer(ctx, &runtime.CreateContainerRequest{PodSandboxId: sb.ID, Config: config, SandboxConfig: requestSandboxConfig})
			require.ErrorIs(t, err, errSnapshotIdentityOptionsComplete)
			require.Equal(t, 1, backend.calls)
			require.NotNil(t, store.container)
			require.Len(t, store.container.ID, 64, "generated container ID")
			require.Equal(t, store.container.ID, store.container.SnapshotKey)
			require.Equal(t, store.container.ID, filepath.Base(backend.key))
			require.NotEmpty(t, backend.parent)
			wantImage := imageName
			if tc.userImage != "" {
				wantImage = tc.userImage
			}
			want := map[string]string{
				annotations.SnapshotLabel(annotations.SandboxID):        sb.ID,
				annotations.SnapshotLabel(annotations.SandboxNamespace): sb.Config.Metadata.Namespace,
				annotations.SnapshotLabel(annotations.SandboxName):      sb.Config.Metadata.Name,
				annotations.SnapshotLabel(annotations.SandboxUID):       sb.Config.Metadata.Uid,
				annotations.SnapshotLabel(annotations.ContainerType):    annotations.ContainerTypeContainer,
				annotations.SnapshotLabel(annotations.ContainerName):    config.Metadata.Name,
				annotations.SnapshotLabel(annotations.ImageName):        wantImage,
				annotations.SnapshotContainerID:                         store.container.ID,
				annotations.SnapshotContainerAttempt:                    tc.decimal,
			}
			if tc.remap {
				want[snapshots.LabelSnapshotUIDMapping] = "0:100000:65536"
				want[snapshots.LabelSnapshotGIDMapping] = "0:200000:65536"
			}
			require.Equal(t, want, backend.labels)
			info, err := sn.Stat(ctx, backend.key)
			require.NoError(t, err)
			require.Equal(t, want, info.Labels)
			require.Equal(t, snapshots.KindActive, info.Kind)
			spec, err := typeurl.UnmarshalAny(store.container.Spec)
			require.NoError(t, err)
			for key := range canonical {
				require.NotContains(t, spec.(*runtimespec.Spec).Annotations, key)
			}
		})
	}
}
