# CRI snapshot identity

CRI supplies identity labels when creating the root filesystem snapshot for an
application container or a pause container managed by the `podsandbox` controller.
Snapshotters receive these labels at `Prepare`, before the container is persisted.
The labels use the `containerd.io/snapshot/` prefix and the existing CRI names:

| Label suffix | Value |
| --- | --- |
| `io.kubernetes.cri.container-type` | `container` or `sandbox` |
| `io.kubernetes.cri.sandbox-id` | containerd sandbox ID |
| `io.kubernetes.cri.sandbox-namespace` | Pod namespace |
| `io.kubernetes.cri.sandbox-uid` | Pod UID |
| `io.kubernetes.cri.sandbox-name` | Pod name |
| `io.kubernetes.cri.container-name` | Application container name; application snapshots only |
| `io.kubernetes.cri.image-name` | Application image name; application snapshots only |
| `io.kubernetes.cri.podsandbox.image-name` | Configured pause image name; sandbox snapshots only |
| `io.kubernetes.cri.container-id` | containerd-generated container ID; application snapshots only |
| `io.kubernetes.cri.container-attempt` | CRI `ContainerMetadata.Attempt`, unsigned decimal (`0` through `4294967295`); application snapshots only |

Pod identity comes from the stored sandbox configuration, including for
`CreateContainer`; its duplicate request sandbox configuration cannot replace
these values. The attempt is supplied by the CRI client, not a containerd-wide
counter. Application image names use `UserSpecifiedImage` when provided and the
resolved image name otherwise.

Canonical CRI labels override colliding inherited labels. Sandbox annotations
already inherited by CRI retain their existing behavior for other keys, and
platform snapshot options, including UID/GID mappings, retain their precedence.
Identity labels are always supplied, independently of image snapshot annotation
settings. This does not add configurable workload metadata forwarding or change
the existing OCI spec annotations.

These labels describe the rootfs snapshot being created. They do not provide pod
identity for image pull/unpack snapshots, image volumes, or other volume mounts,
and do not propagate annotations added later by NRI. Existing snapshots are not
relabelled. Snapshotters may use this context for their own implementation;
CRI does not add licensing, encryption, or authorization policy.
