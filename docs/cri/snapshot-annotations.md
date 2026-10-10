# Application container snapshot annotations

Administrators can forward selected workload metadata to the snapshotter when CRI
creates an application container's writable rootfs snapshot. Select annotation
keys under `containerd.io/snapshot/` with the runtime's existing
`pod_annotations` and `container_annotations` allowlists. No additional
configuration option is required.

For example, to forward an opaque subject chosen by the administrator:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
  pod_annotations = ['containerd.io/snapshot/example.com.subject']
  container_annotations = ['containerd.io/snapshot/example.com.subject']
```

These lists select annotations supplied in CRI requests. Kubernetes does not
automatically copy arbitrary Pod annotations into CRI container annotations.
Use exact keys where possible; the lists also support Go `path.Match` patterns.
Allowing a key authorizes the caller to supply that snapshot metadata, so select
only keys supported by the configured snapshotter and appropriate for the
workload. Values are forwarded unchanged; their interpretation and validation
belong to the snapshotter.

CRI takes the annotations from the actual generated OCI spec, after applying the
runtime allowlists, before preparing the rootfs and before NRI adjustments. It
does not read raw container annotations at the snapshot option stage. Snapshot
annotations in an administrator's `base_runtime_spec` are also included, without
requiring a CRI allowlist entry. For the same key, an allowlisted Pod value
overrides the base spec value, and an allowlisted container value overrides the
Pod value. Later NRI annotation changes do not affect these snapshot labels.

Only labels in the inherited snapshot namespace are forwarded. CRI always
removes these reserved keys from the forwarded annotations, including base spec
annotations, even when no user namespace is requested:

- `containerd.io/snapshot.ref`
- `containerd.io/snapshot/uidmapping`
- `containerd.io/snapshot/gidmapping`

The forwarded labels are applied **before** platform-generated snapshot options,
so the platform options win on collision. Linux UID/GID mappings continue to come
from the CRI user namespace configuration. On Windows, a nonzero
`WindowsContainerResources.rootfs_size_in_bytes` takes precedence over an
allowlisted `containerd.io/snapshot/windows/rootfs.sizebytes` annotation. If no
platform value is generated, an allowlisted non-reserved annotation is forwarded
unchanged, including an empty or invalid value; the snapshotter validates it.

Each snapshot label's **key plus value must fit within 4096 bytes**. Snapshot
labels are stored as metadata and can appear in diagnostics. **Never send secret
decryption keys in annotations or snapshot labels.** An opaque subject or
reference is metadata, not proof of identity or an authorization decision.

This transport applies only to application container rootfs snapshot creation.
It does not change sandbox snapshots, image unpacking, or image volume snapshots.
The image service's `disable_snapshot_annotations` setting controls image
metadata during unpacking, independently of this workload metadata.

This uses the per-container rootfs snapshot option stage and platform precedence
proposed in [containerd/containerd#14070](https://github.com/containerd/containerd/pull/14070).
It explicitly extends that proposal's current `max-size`-only transport to
administrator-selected generic metadata through runtime allowlists and the
trusted base spec, while preserving CRI's reserved snapshot keys.
