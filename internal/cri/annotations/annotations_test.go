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

package annotations

import (
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestDefaultCRISnapshotLabels(t *testing.T) {
	config := &runtime.PodSandboxConfig{Metadata: &runtime.PodSandboxMetadata{
		Name: "pod-name", Uid: "pod-uid", Namespace: "pod-namespace",
	}}
	common := map[string]string{
		"containerd.io/snapshot/io.kubernetes.cri.sandbox-id":        "sandbox-id",
		"containerd.io/snapshot/io.kubernetes.cri.sandbox-name":      "pod-name",
		"containerd.io/snapshot/io.kubernetes.cri.sandbox-uid":       "pod-uid",
		"containerd.io/snapshot/io.kubernetes.cri.sandbox-namespace": "pod-namespace",
	}
	t.Run("sandbox", func(t *testing.T) {
		want := MergeSnapshotLabels(common, map[string]string{
			"containerd.io/snapshot/io.kubernetes.cri.container-type":        "sandbox",
			"containerd.io/snapshot/io.kubernetes.cri.podsandbox.image-name": "pause-image",
		})
		require.Equal(t, want, DefaultCRISnapshotLabelsForSandbox("sandbox-id", "pause-image", config))
	})
	for _, tc := range []struct {
		attempt uint32
		want    string
	}{
		{0, "0"}, {1, "1"}, {10, "10"}, {4294967295, "4294967295"},
	} {
		t.Run("container/"+tc.want, func(t *testing.T) {
			want := MergeSnapshotLabels(common, map[string]string{
				"containerd.io/snapshot/io.kubernetes.cri.container-type":    "container",
				"containerd.io/snapshot/io.kubernetes.cri.container-name":    "container-name",
				"containerd.io/snapshot/io.kubernetes.cri.image-name":        "container-image",
				"containerd.io/snapshot/io.kubernetes.cri.container-id":      "container-id",
				"containerd.io/snapshot/io.kubernetes.cri.container-attempt": tc.want,
			})
			require.Equal(t, want, DefaultCRISnapshotLabelsForContainer("sandbox-id", "container-name", "container-image", config, "container-id", tc.attempt))
		})
	}
	t.Run("missing_metadata", func(t *testing.T) {
		labels := DefaultCRISnapshotLabelsForSandbox("sandbox-id", "pause-image", nil)
		require.Equal(t, "", labels[SnapshotLabel(SandboxUID)])
		require.Equal(t, "", labels[SnapshotLabel(SandboxName)])
		require.Equal(t, "", labels[SnapshotLabel(SandboxNamespace)])
	})
}

func TestMergeSnapshotLabelsCanonicalIdentity(t *testing.T) {
	for _, canonical := range []map[string]string{
		DefaultCRISnapshotLabelsForSandbox("sandbox-id", "pause-image", nil),
		DefaultCRISnapshotLabelsForContainer("sandbox-id", "container-name", "container-image", nil, "container-id", 10),
	} {
		inherited := map[string]string{"containerd.io/snapshot/custom": "custom-value"}
		for key := range canonical {
			inherited[key] = "forged"
		}
		got := MergeSnapshotLabels(snapshots.FilterInheritedLabels(inherited), canonical)
		for key, value := range canonical {
			require.Equal(t, value, got[key], key)
			require.Equal(t, "forged", inherited[key], "merge must not mutate inherited labels")
		}
		require.Equal(t, "custom-value", got["containerd.io/snapshot/custom"])
		// Platform mappings remain independent of CRI identity.
		info := snapshots.Info{}
		require.NoError(t, snapshots.WithLabels(got)(&info))
		require.NoError(t, snapshots.WithLabels(map[string]string{
			snapshots.LabelSnapshotUIDMapping: "0:100000:65536",
			snapshots.LabelSnapshotGIDMapping: "0:200000:65536",
		})(&info))
		require.Equal(t, "0:100000:65536", info.Labels[snapshots.LabelSnapshotUIDMapping])
		require.Equal(t, "0:200000:65536", info.Labels[snapshots.LabelSnapshotGIDMapping])
		for key, value := range canonical {
			require.Equal(t, value, info.Labels[key], key)
		}
	}
}
