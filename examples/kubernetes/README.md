# Kubernetes example

A service whose configuration lives in a ConfigMap and changes without a
restart.

```bash
kubectl apply -f manifests/
kubectl logs -f deploy/example

# In another terminal:
kubectl edit configmap example-config
```

Within the kubelet's sync period the log shows a new generation. The pod's
UID does not change, and no container is restarted.

## What makes this work

A ConfigMap volume is not a directory of files. It is a symlink farm:

```text
/etc/example/
├── ..2026_08_26_10_00_00.123456789/   the real data
│   └── config.yaml
├── ..data -> ..2026_08_26_10_00_00.123456789
└── config.yaml -> ..data/config.yaml
```

An update writes a whole new timestamped directory and then renames a new
`..data` symlink over the old one. The rename is atomic, and it is the only
filesystem event the update produces — `config.yaml` itself is never
written to. A watcher looking for writes to `config.yaml` sees nothing at
all, which is why this package watches the directory and treats `..data` as
the file's proxy.

## The two ways to break it

**`subPath`.** A `subPath` volume mount is copied once when the container
starts and is never updated afterwards. The file will never change. Mount
the directory, as `deployment.yaml` does.

**A bad ConfigMap.** An edit that does not validate is rejected, reported
through the error subscriber, and otherwise ignored: the pod keeps serving
the configuration it already had. Fix the ConfigMap and the next
republication is picked up. Note the asymmetry with startup — a pod that
*starts* with a bad ConfigMap fails fast and lands in `CrashLoopBackOff`,
which is what you want, because there is no last-known-good configuration to
fall back to.

See [docs/kubernetes.md](../../docs/kubernetes.md) for Secret volumes,
sync-period expectations and the `immutable: true` case.
