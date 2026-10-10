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
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	ostesting "github.com/containerd/containerd/v2/pkg/os/testing"
	"github.com/containerd/platforms"

	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/containerd/containerd/v2/internal/cri/constants"
	"github.com/containerd/containerd/v2/internal/cri/opts"
	"github.com/containerd/containerd/v2/pkg/oci"
)

var currentPlatform = platforms.DefaultSpec()

func checkMount(t *testing.T, mounts []runtimespec.Mount, src, dest, typ string,
	contains, notcontains []string) {
	found := false
	for _, m := range mounts {
		if m.Source == src && m.Destination == dest {
			assert.Equal(t, m.Type, typ)
			for _, c := range contains {
				assert.Contains(t, m.Options, c)
			}
			for _, n := range notcontains {
				assert.NotContains(t, m.Options, n)
			}
			found = true
			break
		}
	}
	assert.True(t, found, "mount from %q to %q not found", src, dest)
}

const testImageName = "container-image-name"

func TestGeneralContainerSpec(t *testing.T) {
	testID := "test-id"
	testPid := uint32(1234)
	containerConfig, sandboxConfig, imageConfig, specCheck := getCreateContainerTestData()
	ociRuntime := config.Runtime{}
	c := newTestCRIService()
	testSandboxID := "sandbox-id"
	testContainerName := "container-name"
	spec, err := c.buildContainerSpec(currentPlatform, testID, testSandboxID, testPid, "", testContainerName, testImageName, containerConfig, sandboxConfig, imageConfig, nil, ociRuntime, nil)
	require.NoError(t, err)
	specCheck(t, testID, testSandboxID, testPid, spec)
}

func TestPodAnnotationPassthroughContainerSpec(t *testing.T) {
	switch goruntime.GOOS {
	case "darwin":
		t.Skip("not implemented on Darwin")
	case "freebsd":
		t.Skip("not implemented on FreeBSD")
	}

	testID := "test-id"
	testSandboxID := "sandbox-id"
	testContainerName := "container-name"
	testPid := uint32(1234)

	for _, test := range []struct {
		desc           string
		podAnnotations []string
		configChange   func(*runtime.PodSandboxConfig)
		specCheck      func(*testing.T, *runtimespec.Spec)
	}{
		{
			desc:           "a passthrough annotation should be passed as an OCI annotation",
			podAnnotations: []string{"c"},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				assert.Equal(t, spec.Annotations["c"], "d")
			},
		},
		{
			desc: "a non-passthrough annotation should not be passed as an OCI annotation",
			configChange: func(c *runtime.PodSandboxConfig) {
				c.Annotations["d"] = "e"
			},
			podAnnotations: []string{"c"},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				assert.Equal(t, spec.Annotations["c"], "d")
				_, ok := spec.Annotations["d"]
				assert.False(t, ok)
			},
		},
		{
			desc: "passthrough annotations should support wildcard match",
			configChange: func(c *runtime.PodSandboxConfig) {
				c.Annotations["t.f"] = "j"
				c.Annotations["z.g"] = "o"
				c.Annotations["z"] = "o"
				c.Annotations["y.ca"] = "b"
				c.Annotations["y"] = "b"
			},
			podAnnotations: []string{"t*", "z.*", "y.c*"},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				t.Logf("%+v", spec.Annotations)
				assert.Equal(t, spec.Annotations["t.f"], "j")
				assert.Equal(t, spec.Annotations["z.g"], "o")
				assert.Equal(t, spec.Annotations["y.ca"], "b")
				_, ok := spec.Annotations["y"]
				assert.False(t, ok)
				_, ok = spec.Annotations["z"]
				assert.False(t, ok)
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			c := newTestCRIService()
			containerConfig, sandboxConfig, imageConfig, specCheck := getCreateContainerTestData()
			if test.configChange != nil {
				test.configChange(sandboxConfig)
			}

			ociRuntime := config.Runtime{
				PodAnnotations: test.podAnnotations,
			}
			spec, err := c.buildContainerSpec(currentPlatform, testID, testSandboxID, testPid, "", testContainerName, testImageName,
				containerConfig, sandboxConfig, imageConfig, nil, ociRuntime, nil)
			assert.NoError(t, err)
			assert.NotNil(t, spec)
			specCheck(t, testID, testSandboxID, testPid, spec)
			if test.specCheck != nil {
				test.specCheck(t, spec)
			}
		})
	}
}

func TestContainerSpecCommand(t *testing.T) {
	for _, test := range []struct {
		desc            string
		criEntrypoint   []string
		criArgs         []string
		imageEntrypoint []string
		imageArgs       []string
		expected        []string
		expectErr       bool
	}{
		{
			desc:            "should use cri entrypoint if it's specified",
			criEntrypoint:   []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"a", "b"},
		},
		{
			desc:            "should use cri entrypoint if it's specified even if it's empty",
			criEntrypoint:   []string{},
			criArgs:         []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"a", "b"},
		},
		{
			desc:            "should use cri entrypoint and args if they are specified",
			criEntrypoint:   []string{"a", "b"},
			criArgs:         []string{"c", "d"},
			imageEntrypoint: []string{"e", "f"},
			imageArgs:       []string{"g", "h"},
			expected:        []string{"a", "b", "c", "d"},
		},
		{
			desc:            "should use image entrypoint if cri entrypoint is not specified",
			criArgs:         []string{"a", "b"},
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"c", "d", "a", "b"},
		},
		{
			desc:            "should use image args if both cri entrypoint and args are not specified",
			imageEntrypoint: []string{"c", "d"},
			imageArgs:       []string{"e", "f"},
			expected:        []string{"c", "d", "e", "f"},
		},
		{
			desc:      "should return error if both entrypoint and args are empty",
			expectErr: true,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			config, _, imageConfig, _ := getCreateContainerTestData()
			config.Command = test.criEntrypoint
			config.Args = test.criArgs
			imageConfig.Entrypoint = test.imageEntrypoint
			imageConfig.Cmd = test.imageArgs

			var spec runtimespec.Spec
			err := opts.WithProcessArgs(config, imageConfig)(context.Background(), nil, nil, &spec)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, test.expected, spec.Process.Args, test.desc)
		})
	}
}

func TestVolumeMounts(t *testing.T) {
	testContainerRootDir := "test-container-root"
	idmap := []*runtime.IDMapping{
		{
			ContainerId: 0,
			HostId:      100,
			Length:      1,
		},
	}

	for _, test := range []struct {
		desc              string
		platform          imagespec.Platform
		criMounts         []*runtime.Mount
		usernsEnabled     bool
		imageVolumes      map[string]struct{}
		expectedMountDest []string
		expectedMappings  []*runtime.IDMapping
	}{
		{
			desc: "should setup rw mount for image volumes",
			imageVolumes: map[string]struct{}{
				"/test-volume-1": {},
				"/test-volume-2": {},
			},
			expectedMountDest: []string{
				"/test-volume-1",
				"/test-volume-2",
			},
		},
		{
			desc: "should skip image volumes if already mounted by CRI",
			criMounts: []*runtime.Mount{
				{
					ContainerPath: "/test-volume-1",
					HostPath:      "/test-hostpath-1",
				},
			},
			imageVolumes: map[string]struct{}{
				"/test-volume-1": {},
				"/test-volume-2": {},
			},
			expectedMountDest: []string{
				"/test-volume-2",
			},
		},
		{
			desc: "should compare and return cleanpath",
			criMounts: []*runtime.Mount{
				{
					ContainerPath: "/test-volume-1",
					HostPath:      "/test-hostpath-1",
				},
			},
			imageVolumes: map[string]struct{}{
				"/test-volume-1/": {},
				"/test-volume-2/": {},
			},
			expectedMountDest: []string{
				"/test-volume-2/",
			},
		},
		{
			desc:     "should make relative paths absolute on Linux",
			platform: imagespec.Platform{OS: "linux"},
			imageVolumes: map[string]struct{}{
				"./test-volume-1":     {},
				"C:/test-volume-2":    {},
				"../../test-volume-3": {},
				"/abs/test-volume-4":  {},
			},
			expectedMountDest: []string{
				"/test-volume-1",
				"/C:/test-volume-2",
				"/test-volume-3",
				"/abs/test-volume-4",
			},
		},
		{
			desc:          "should include mappings for image volumes on Linux",
			platform:      imagespec.Platform{OS: "linux"},
			usernsEnabled: true,
			imageVolumes: map[string]struct{}{
				"/test-volume-1/": {},
				"/test-volume-2/": {},
			},
			expectedMountDest: []string{
				"/test-volume-2/",
				"/test-volume-2/",
			},
			expectedMappings: idmap,
		},
		{
			desc:          "should NOT include mappings for image volumes on Linux if !userns",
			platform:      imagespec.Platform{OS: "linux"},
			usernsEnabled: false,
			imageVolumes: map[string]struct{}{
				"/test-volume-1/": {},
				"/test-volume-2/": {},
			},
			expectedMountDest: []string{
				"/test-volume-2/",
				"/test-volume-2/",
			},
		},
		{
			desc:          "should convert rel imageVolume paths to abs paths and add userns mappings",
			platform:      imagespec.Platform{OS: "linux"},
			usernsEnabled: true,
			imageVolumes: map[string]struct{}{
				"test-volume-1/":       {},
				"C:/test-volume-2/":    {},
				"../../test-volume-3/": {},
			},
			expectedMountDest: []string{
				"/test-volume-1",
				"/C:/test-volume-2",
				"/test-volume-3",
			},
			expectedMappings: idmap,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			config := &imagespec.ImageConfig{
				Volumes: test.imageVolumes,
			}
			containerConfig := &runtime.ContainerConfig{Mounts: test.criMounts}
			if test.usernsEnabled {
				containerConfig.Linux = &runtime.LinuxContainerConfig{
					SecurityContext: &runtime.LinuxContainerSecurityContext{
						NamespaceOptions: &runtime.NamespaceOption{
							UsernsOptions: &runtime.UserNamespace{
								Mode: runtime.NamespaceMode_POD,
								Uids: idmap,
								Gids: idmap,
							},
						},
					},
				}
			}

			c := newTestCRIService()
			got := c.volumeMounts(test.platform, testContainerRootDir, containerConfig, config)
			assert.Len(t, got, len(test.expectedMountDest))
			for _, dest := range test.expectedMountDest {
				found := false
				for _, m := range got {
					if m.ContainerPath != dest {
						continue
					}
					found = true
					assert.Equal(t,
						filepath.Dir(m.HostPath),
						filepath.Join(testContainerRootDir, "volumes"))
					if test.expectedMappings != nil {
						assert.Equal(t, test.expectedMappings, m.UidMappings)
						assert.Equal(t, test.expectedMappings, m.GidMappings)
					}
					break
				}
				assert.True(t, found)
			}
		})
	}
}

func TestContainerAnnotationPassthroughContainerSpec(t *testing.T) {
	switch goruntime.GOOS {
	case "darwin":
		t.Skip("not implemented on Darwin")
	case "freebsd":
		t.Skip("not implemented on FreeBSD")
	}

	testID := "test-id"
	testSandboxID := "sandbox-id"
	testContainerName := "container-name"
	testPid := uint32(1234)

	for _, test := range []struct {
		desc                 string
		podAnnotations       []string
		containerAnnotations []string
		podConfigChange      func(*runtime.PodSandboxConfig)
		configChange         func(*runtime.ContainerConfig)
		specCheck            func(*testing.T, *runtimespec.Spec)
	}{
		{
			desc: "passthrough annotations from pod and container should be passed as an OCI annotation",
			podConfigChange: func(p *runtime.PodSandboxConfig) {
				p.Annotations["pod.annotation.1"] = "1"
				p.Annotations["pod.annotation.2"] = "2"
				p.Annotations["pod.annotation.3"] = "3"
			},
			configChange: func(c *runtime.ContainerConfig) {
				c.Annotations["container.annotation.1"] = "1"
				c.Annotations["container.annotation.2"] = "2"
				c.Annotations["container.annotation.3"] = "3"
			},
			podAnnotations:       []string{"pod.annotation.1"},
			containerAnnotations: []string{"container.annotation.1"},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				assert.Equal(t, "1", spec.Annotations["container.annotation.1"])
				_, ok := spec.Annotations["container.annotation.2"]
				assert.False(t, ok)
				_, ok = spec.Annotations["container.annotation.3"]
				assert.False(t, ok)
				assert.Equal(t, "1", spec.Annotations["pod.annotation.1"])
				_, ok = spec.Annotations["pod.annotation.2"]
				assert.False(t, ok)
				_, ok = spec.Annotations["pod.annotation.3"]
				assert.False(t, ok)
			},
		},
		{
			desc: "passthrough annotations from pod and container should support wildcard",
			podConfigChange: func(p *runtime.PodSandboxConfig) {
				p.Annotations["pod.annotation.1"] = "1"
				p.Annotations["pod.annotation.2"] = "2"
				p.Annotations["pod.annotation.3"] = "3"
			},
			configChange: func(c *runtime.ContainerConfig) {
				c.Annotations["container.annotation.1"] = "1"
				c.Annotations["container.annotation.2"] = "2"
				c.Annotations["container.annotation.3"] = "3"
			},
			podAnnotations:       []string{"pod.annotation.*"},
			containerAnnotations: []string{"container.annotation.*"},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				assert.Equal(t, "1", spec.Annotations["container.annotation.1"])
				assert.Equal(t, "2", spec.Annotations["container.annotation.2"])
				assert.Equal(t, "3", spec.Annotations["container.annotation.3"])
				assert.Equal(t, "1", spec.Annotations["pod.annotation.1"])
				assert.Equal(t, "2", spec.Annotations["pod.annotation.2"])
				assert.Equal(t, "3", spec.Annotations["pod.annotation.3"])
			},
		},
		{
			desc: "annotations should not pass through if no passthrough annotations are configured",
			podConfigChange: func(p *runtime.PodSandboxConfig) {
				p.Annotations["pod.annotation.1"] = "1"
				p.Annotations["pod.annotation.2"] = "2"
				p.Annotations["pod.annotation.3"] = "3"
			},
			configChange: func(c *runtime.ContainerConfig) {
				c.Annotations["container.annotation.1"] = "1"
				c.Annotations["container.annotation.2"] = "2"
				c.Annotations["container.annotation.3"] = "3"
			},
			podAnnotations:       []string{},
			containerAnnotations: []string{},
			specCheck: func(t *testing.T, spec *runtimespec.Spec) {
				_, ok := spec.Annotations["container.annotation.1"]
				assert.False(t, ok)
				_, ok = spec.Annotations["container.annotation.2"]
				assert.False(t, ok)
				_, ok = spec.Annotations["container.annotation.3"]
				assert.False(t, ok)
				_, ok = spec.Annotations["pod.annotation.1"]
				assert.False(t, ok)
				_, ok = spec.Annotations["pod.annotation.2"]
				assert.False(t, ok)
				_, ok = spec.Annotations["pod.annotation.3"]
				assert.False(t, ok)
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			c := newTestCRIService()
			containerConfig, sandboxConfig, imageConfig, specCheck := getCreateContainerTestData()
			if test.configChange != nil {
				test.configChange(containerConfig)
			}
			if test.podConfigChange != nil {
				test.podConfigChange(sandboxConfig)
			}
			ociRuntime := config.Runtime{
				PodAnnotations:       test.podAnnotations,
				ContainerAnnotations: test.containerAnnotations,
			}
			spec, err := c.buildContainerSpec(currentPlatform, testID, testSandboxID, testPid, "", testContainerName, testImageName,
				containerConfig, sandboxConfig, imageConfig, nil, ociRuntime, nil)
			assert.NoError(t, err)
			assert.NotNil(t, spec)
			specCheck(t, testID, testSandboxID, testPid, spec)
			if test.specCheck != nil {
				test.specCheck(t, spec)
			}
		})
	}
}

func resolveSnapshotOpts(t *testing.T, opts []snapshots.Opt) snapshots.Info {
	t.Helper()
	var info snapshots.Info
	for _, opt := range opts {
		require.NoError(t, opt(&info))
	}
	return info
}

func TestSnapshotOptsPrecedence(t *testing.T) {
	const windowsRootfsSize = "containerd.io/snapshot/windows/rootfs.sizebytes"
	for _, test := range []struct {
		name           string
		annotations    map[string]string
		platformLabels map[string]string
		wantLabels     map[string]string
	}{
		{
			name:           "Windows CRI rootfs size overrides annotation",
			annotations:    map[string]string{windowsRootfsSize: "1"},
			platformLabels: map[string]string{windowsRootfsSize: "68719476736"},
			wantLabels:     map[string]string{windowsRootfsSize: "68719476736"},
		},
		{
			name:           "Windows CRI rootfs size overrides invalid annotation",
			annotations:    map[string]string{windowsRootfsSize: "not-a-size"},
			platformLabels: map[string]string{windowsRootfsSize: "68719476736"},
			wantLabels:     map[string]string{windowsRootfsSize: "68719476736"},
		},
		{
			name:           "Windows CRI rootfs size overrides empty annotation",
			annotations:    map[string]string{windowsRootfsSize: ""},
			platformLabels: map[string]string{windowsRootfsSize: "68719476736"},
			wantLabels:     map[string]string{windowsRootfsSize: "68719476736"},
		},
		{
			name:           "absent annotation preserves Windows CRI rootfs size",
			platformLabels: map[string]string{windowsRootfsSize: "68719476736"},
			wantLabels:     map[string]string{windowsRootfsSize: "68719476736"},
		},
		{
			name:        "annotation is forwarded without platform size",
			annotations: map[string]string{windowsRootfsSize: "33554432"},
			wantLabels:  map[string]string{windowsRootfsSize: "33554432"},
		},
		{
			name:        "invalid annotation is forwarded without platform size",
			annotations: map[string]string{windowsRootfsSize: "not-a-size"},
			wantLabels:  map[string]string{windowsRootfsSize: "not-a-size"},
		},
		{
			name:           "platform options override generic snapshot labels",
			annotations:    map[string]string{snapshots.LabelSnapshotMaxSize: "1", "containerd.io/snapshot/example.com.subject": "opaque"},
			platformLabels: map[string]string{snapshots.LabelSnapshotMaxSize: "33554432"},
			wantLabels:     map[string]string{snapshots.LabelSnapshotMaxSize: "33554432", "containerd.io/snapshot/example.com.subject": "opaque"},
		},
		{
			name: "no annotations or platform labels",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platformOpts := []snapshots.Opt{snapshots.WithParent("image-parent")}
			if test.platformLabels != nil {
				platformOpts = append(platformOpts, snapshots.WithLabels(test.platformLabels))
			}
			info := resolveSnapshotOpts(t, snapshotOpts(test.annotations, platformOpts))
			assert.Equal(t, test.wantLabels, info.Labels)
			assert.Equal(t, "image-parent", info.Parent, "non-label platform options are preserved")
		})
	}
}

func TestContainerSnapshotAnnotations(t *testing.T) {
	if currentPlatform.OS != "linux" && currentPlatform.OS != "windows" {
		t.Skip("container spec fixtures require Linux or Windows")
	}
	const subject = "containerd.io/snapshot/example.com.subject"
	for _, test := range []struct {
		name                 string
		containerAnnotations map[string]string
		podAnnotations       map[string]string
		baseAnnotations      map[string]string
		containerAllowlist   []string
		podAllowlist         []string
		wantLabels           map[string]string
		wantSpecAnnotations  map[string]string
	}{
		{
			name:                 "allowlisted container annotation",
			containerAnnotations: map[string]string{subject: "opaque:v1/a+b/= metadata-é"},
			containerAllowlist:   []string{subject},
			wantLabels:           map[string]string{subject: "opaque:v1/a+b/= metadata-é"},
		},
		{
			name:                 "wildcard allowlist",
			containerAnnotations: map[string]string{subject: "container", snapshots.LabelSnapshotMaxSize: "33554432"},
			containerAllowlist:   []string{"containerd.io/snapshot/*"},
			wantLabels:           map[string]string{subject: "container", snapshots.LabelSnapshotMaxSize: "33554432"},
		},
		{
			name:           "allowlisted pod annotation",
			podAnnotations: map[string]string{subject: "pod"},
			podAllowlist:   []string{subject},
			wantLabels:     map[string]string{subject: "pod"},
		},
		{
			name:                 "container overrides pod",
			containerAnnotations: map[string]string{subject: "container"},
			podAnnotations:       map[string]string{subject: "pod"},
			containerAllowlist:   []string{subject},
			podAllowlist:         []string{subject},
			wantLabels:           map[string]string{subject: "container"},
		},
		{
			name:                 "rejected container annotation preserves pod value",
			containerAnnotations: map[string]string{subject: "container"},
			podAnnotations:       map[string]string{subject: "pod"},
			podAllowlist:         []string{subject},
			wantLabels:           map[string]string{subject: "pod"},
		},
		{
			name:                 "no allowlist",
			containerAnnotations: map[string]string{subject: "container"},
			podAnnotations:       map[string]string{subject: "pod"},
		},
		{
			name:                 "unmatched allowlist",
			containerAnnotations: map[string]string{subject: "container"},
			containerAllowlist:   []string{"containerd.io/snapshot/other.*"},
		},
		{
			name:                 "allowlisted non-snapshot annotation",
			containerAnnotations: map[string]string{"example.com/subject": "container", "containerd.io/snapshotter/subject": "container"},
			containerAllowlist:   []string{"example.com/subject", "containerd.io/snapshotter/subject"},
			wantSpecAnnotations:  map[string]string{"example.com/subject": "container", "containerd.io/snapshotter/subject": "container"},
		},
		{
			name:                 "empty annotation is forwarded",
			containerAnnotations: map[string]string{subject: ""},
			containerAllowlist:   []string{subject},
			wantLabels:           map[string]string{subject: ""},
		},
		{
			name: "no annotations",
		},
		{
			name:                 "reserved annotations removed without userns",
			containerAnnotations: map[string]string{subject: "container", snapshots.LabelSnapshotRef: "spoofed", snapshots.LabelSnapshotUIDMapping: "0:1:1", snapshots.LabelSnapshotGIDMapping: "0:1:1"},
			containerAllowlist:   []string{"containerd.io/snapshot/*", snapshots.LabelSnapshotRef},
			wantLabels:           map[string]string{subject: "container"},
			wantSpecAnnotations:  map[string]string{snapshots.LabelSnapshotRef: "spoofed", snapshots.LabelSnapshotUIDMapping: "0:1:1", snapshots.LabelSnapshotGIDMapping: "0:1:1"},
		},
		{
			name:            "trusted base spec needs no allowlist",
			baseAnnotations: map[string]string{subject: "base", "example.com/subject": "base"},
			wantLabels:      map[string]string{subject: "base"},
		},
		{
			name:                "reserved base spec annotations removed without userns",
			baseAnnotations:     map[string]string{snapshots.LabelSnapshotRef: "spoofed", snapshots.LabelSnapshotUIDMapping: "0:1:1", snapshots.LabelSnapshotGIDMapping: "0:1:1"},
			wantSpecAnnotations: map[string]string{snapshots.LabelSnapshotRef: "spoofed", snapshots.LabelSnapshotUIDMapping: "0:1:1", snapshots.LabelSnapshotGIDMapping: "0:1:1"},
		},
		{
			name:            "pod overrides base spec",
			baseAnnotations: map[string]string{subject: "base"},
			podAnnotations:  map[string]string{subject: "pod"},
			podAllowlist:    []string{subject},
			wantLabels:      map[string]string{subject: "pod"},
		},
		{
			name:                 "container overrides pod and base spec",
			baseAnnotations:      map[string]string{subject: "base"},
			podAnnotations:       map[string]string{subject: "pod"},
			containerAnnotations: map[string]string{subject: "container"},
			podAllowlist:         []string{subject},
			containerAllowlist:   []string{subject},
			wantLabels:           map[string]string{subject: "container"},
		},
		{
			name:                 "unallowlisted annotations cannot override base spec",
			baseAnnotations:      map[string]string{subject: "base"},
			podAnnotations:       map[string]string{subject: "pod"},
			containerAnnotations: map[string]string{subject: "container"},
			wantLabels:           map[string]string{subject: "base"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newTestCRIService()
			containerConfig, sandboxConfig, imageConfig, _ := getCreateContainerTestData()
			containerConfig.Annotations = test.containerAnnotations
			sandboxConfig.Annotations = test.podAnnotations
			ociRuntime := config.Runtime{
				ContainerAnnotations: test.containerAllowlist,
				PodAnnotations:       test.podAllowlist,
			}
			if test.baseAnnotations != nil {
				base, err := c.buildContainerSpec(currentPlatform, "base-id", "sandbox-id", 1234, "", "container", testImageName,
					containerConfig, sandboxConfig, imageConfig, nil, config.Runtime{}, nil)
				require.NoError(t, err)
				base.Annotations = test.baseAnnotations
				ociRuntime.BaseRuntimeSpec = "/etc/containerd/cri-base.json"
				c.RuntimeService = &fakeRuntimeService{ocispecs: map[string]*oci.Spec{ociRuntime.BaseRuntimeSpec: base}}
			}
			spec, err := c.buildContainerSpec(currentPlatform, "test-id", "sandbox-id", 1234, "", "container", testImageName,
				containerConfig, sandboxConfig, imageConfig, nil, ociRuntime, nil)
			require.NoError(t, err)
			platformOpts, err := snapshotterOpts(containerConfig)
			require.NoError(t, err)
			before := maps.Clone(spec.Annotations)
			info := resolveSnapshotOpts(t, snapshotOpts(spec.Annotations, platformOpts))
			assert.Equal(t, test.wantLabels, info.Labels)
			assert.Equal(t, before, spec.Annotations, "snapshot filtering leaves OCI annotations intact")
			for key, value := range test.wantSpecAnnotations {
				assert.Equal(t, value, spec.Annotations[key])
			}
		})
	}
}

func TestBaseRuntimeSpec(t *testing.T) {
	c := newTestCRIService(withRuntimeService(&fakeRuntimeService{
		ocispecs: map[string]*oci.Spec{
			"/etc/containerd/cri-base.json": {
				Version:  "1.0.2",
				Hostname: "old",
			},
		},
	}))

	out, err := c.runtimeSpec(
		"id1",
		platforms.DefaultSpec(),
		"/etc/containerd/cri-base.json",
		oci.WithHostname("new-host"),
		oci.WithDomainname("new-domain"),
	)
	assert.NoError(t, err)

	assert.Equal(t, "1.0.2", out.Version)
	assert.Equal(t, "new-host", out.Hostname)
	assert.Equal(t, "new-domain", out.Domainname)

	// Make sure original base spec not changed
	spec, err := c.LoadOCISpec("/etc/containerd/cri-base.json")
	assert.NoError(t, err)
	assert.NotEqual(t, out, spec)
	assert.Equal(t, spec.Hostname, "old")

	assert.Equal(t, filepath.Join("/", constants.K8sContainerdNamespace, "id1"), out.Linux.CgroupsPath)
}

func TestLinuxContainerMounts(t *testing.T) {
	const testSandboxID = "test-id"
	idmap := []*runtime.IDMapping{
		{
			ContainerId: 0,
			HostId:      100,
			Length:      1,
		},
	}

	for _, test := range []struct {
		desc            string
		statFn          func(string) (os.FileInfo, error)
		criMounts       []*runtime.Mount
		securityContext *runtime.LinuxContainerSecurityContext
		expectedMounts  []*runtime.Mount
	}{
		{
			desc: "should setup ro mount when rootfs is read-only",
			securityContext: &runtime.LinuxContainerSecurityContext{
				ReadonlyRootfs: true,
			},
			expectedMounts: []*runtime.Mount{
				{
					ContainerPath:  "/etc/hostname",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hostname"),
					Readonly:       true,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  "/etc/hosts",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hosts"),
					Readonly:       true,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  resolvConfPath,
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "resolv.conf"),
					Readonly:       true,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  "/dev/shm",
					HostPath:       filepath.Join(testStateDir, sandboxesDir, testSandboxID, "shm"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
			},
		},
		{
			desc:            "should setup rw mount when rootfs is read-write",
			securityContext: &runtime.LinuxContainerSecurityContext{},
			expectedMounts: []*runtime.Mount{
				{
					ContainerPath:  "/etc/hostname",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hostname"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  "/etc/hosts",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hosts"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  resolvConfPath,
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "resolv.conf"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  "/dev/shm",
					HostPath:       filepath.Join(testStateDir, sandboxesDir, testSandboxID, "shm"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
			},
		},
		{
			desc: "should setup uidMappings/gidMappings when userns is used",
			securityContext: &runtime.LinuxContainerSecurityContext{
				NamespaceOptions: &runtime.NamespaceOption{
					UsernsOptions: &runtime.UserNamespace{
						Mode: runtime.NamespaceMode_POD,
						Uids: idmap,
						Gids: idmap,
					},
				},
			},
			expectedMounts: []*runtime.Mount{
				{
					ContainerPath:  "/etc/hostname",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hostname"),
					Readonly:       false,
					SelinuxRelabel: true,
					UidMappings:    idmap,
					GidMappings:    idmap,
				},
				{
					ContainerPath:  "/etc/hosts",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hosts"),
					Readonly:       false,
					SelinuxRelabel: true,
					UidMappings:    idmap,
					GidMappings:    idmap,
				},
				{
					ContainerPath:  resolvConfPath,
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "resolv.conf"),
					Readonly:       false,
					SelinuxRelabel: true,
					UidMappings:    idmap,
					GidMappings:    idmap,
				},
				{
					ContainerPath:  "/dev/shm",
					HostPath:       filepath.Join(testStateDir, sandboxesDir, testSandboxID, "shm"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
			},
		},
		{
			desc: "should use host /dev/shm when host ipc is set",
			securityContext: &runtime.LinuxContainerSecurityContext{
				NamespaceOptions: &runtime.NamespaceOption{Ipc: runtime.NamespaceMode_NODE},
			},
			expectedMounts: []*runtime.Mount{
				{
					ContainerPath:  "/etc/hostname",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hostname"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  "/etc/hosts",
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "hosts"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath:  resolvConfPath,
					HostPath:       filepath.Join(testRootDir, sandboxesDir, testSandboxID, "resolv.conf"),
					Readonly:       false,
					SelinuxRelabel: true,
				},
				{
					ContainerPath: "/dev/shm",
					HostPath:      "/dev/shm",
					Readonly:      false,
				},
			},
		},
		{
			desc: "should skip container mounts if already mounted by CRI",
			criMounts: []*runtime.Mount{
				{
					ContainerPath: "/etc/hostname",
					HostPath:      "/test-etc-hostname",
				},
				{
					ContainerPath: "/etc/hosts",
					HostPath:      "/test-etc-host",
				},
				{
					ContainerPath: resolvConfPath,
					HostPath:      "test-resolv-conf",
				},
				{
					ContainerPath: "/dev/shm",
					HostPath:      "test-dev-shm",
				},
			},
			securityContext: &runtime.LinuxContainerSecurityContext{},
			expectedMounts:  nil,
		},
		{
			desc: "should skip sandbox mounts if the old sandbox doesn't have sandbox file",
			statFn: func(path string) (os.FileInfo, error) {
				sandboxRootDir := filepath.Join(testRootDir, sandboxesDir, testSandboxID)
				sandboxStateDir := filepath.Join(testStateDir, sandboxesDir, testSandboxID)
				switch path {
				case filepath.Join(sandboxRootDir, "hostname"), filepath.Join(sandboxRootDir, "hosts"),
					filepath.Join(sandboxRootDir, "resolv.conf"), filepath.Join(sandboxStateDir, "shm"):
					return nil, errors.New("random error")
				default:
					t.Fatalf("expected sandbox files, got: %s", path)
				}
				return nil, nil
			},
			securityContext: &runtime.LinuxContainerSecurityContext{},
			expectedMounts:  nil,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			config := &runtime.ContainerConfig{
				Metadata: &runtime.ContainerMetadata{
					Name:    "test-name",
					Attempt: 1,
				},
				Mounts: test.criMounts,
				Linux: &runtime.LinuxContainerConfig{
					SecurityContext: test.securityContext,
				},
			}
			c := newTestCRIService()
			c.os.(*ostesting.FakeOS).StatFn = test.statFn
			mounts := c.linuxContainerMounts(testSandboxID, config)
			assert.Equal(t, test.expectedMounts, mounts, test.desc)
		})
	}
}
