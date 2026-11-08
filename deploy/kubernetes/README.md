# comebye hub on Kubernetes (optional)

A generic, single-replica hub: comebye in [hub-only mode](../../docs/setup.md#a-hub-with-no-herdr-containers-kubernetes)
(no herdr, hosts no agents) plus a `tailscaled` sidecar that publishes it on
your tailnet with Tailscale Serve. The agents run on **peers** (your machines),
which you add to the hub as usual. Running comebye on a plain host stays the
default and needs none of this.

## Use

1. Copy this directory. Edit everything marked `CHANGE-ME` (image, login,
   push subject, tailnet name, storage class). The base has no cluster-specific
   settings.
2. Create the auth key secret for the first start:
   `kubectl -n comebye create secret generic comebye-tailscale-auth --from-literal=authkey=tskey-...`
3. `kubectl apply -k .`
4. Open `https://<TS_HOSTNAME>.<tailnet>.ts.net`, then add peers
   (`comebye peer add`, see docs/setup.md).

## What it relies on

- `replicas: 1`, never more. The state directory is single-writer;
  `hub.lock` refuses a second server.
- A `ReadWriteOnce` volume at `/var/lib/comebye` (the state directory).
- Probes: `/healthz` (liveness) and `/readyz` (readiness) on `:8743`, served by
  `COMEBYE_HEALTH_ADDR`; the main listener stays on loopback.
- `COMEBYE_HOST_NAME` is part of the stored data; set it once and keep it.
- Back up the volume: `docs/setup.md#backup-and-restore`. Backups hold secrets.

This is a starting point, not a supported product: pin images, add network
policy and your own backup job.
