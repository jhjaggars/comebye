# comebye in-cluster KubeVirt provisioner (optional)

Example manifests for a second comebye peer, in its own pod next to the hub, that
wakes, stops, creates and destroys KubeVirt worker VMs for it (issue #78). The hub
stays as in `../`; this is an additional pod, not a change to it. Read
[docs/workers.md](../../../docs/workers.md#the-in-cluster-provisioner) and
[docs/security.md](../../../docs/security.md#the-in-cluster-provisioner-issue-78) first:
the Role includes Secrets in the VM namespace.

| File | What |
|---|---|
| `serviceaccount.yaml` | `comebye-provisioner` and its tailscaled-state Role |
| `rbac.yaml` | the narrow Role and RoleBinding in `comebye-workers` |
| `deployment.yaml` | 1 replica, Recreate, token mounted, tailscale sidecar |
| `pvc.yaml` | 100 Mi worker directory |
| `configmap.yaml`, `serve-config.yaml` | the worker tool's KubeVirt config; Tailscale Serve |
| `secrets.yaml` | **placeholders, no values**: Tailscale auth key and OAuth client |
| `Dockerfile` | the image (pinned, checksummed kubectl and virtctl; openssh-client; comebye and comebye-worker from this tree) |
| `manifests_test.go` | Role allow-list, no-Secret-value lint, `kubectl kustomize` |

Copy the directory, edit every `CHANGE-ME`, fill the Secrets elsewhere, `kubectl apply -k .`.
The kustomization sets no `namespace:` because the set spans two namespaces.

Build the image from the repository root (no image is published):

```sh
podman build -f deploy/kubernetes/provisioner/Dockerfile \
  --build-arg VERSION=$(git describe --tags --always) \
  -t registry.example.com/comebye-provisioner:$(git rev-parse --short HEAD) .
podman push registry.example.com/comebye-provisioner:...
```

Check: `go test ./deploy/kubernetes/provisioner/` (needs `kubectl` for the kustomize case).

Create and destroy stay off (every job is a dry run) until you uncomment `-provision-create` in
`deployment.yaml` and fill the OAuth Secret: see docs/workers.md, "Create and destroy from the pod".
