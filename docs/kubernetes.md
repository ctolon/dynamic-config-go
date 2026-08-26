# Kubernetes

## Why this needs its own page

A ConfigMap mounted as a volume is not a directory of files. It is a symlink
farm, and it is updated in a way that produces no event for the file an
application thinks it is watching.

```text
/etc/myapp/
├── ..2026_08_26_10_00_00.123456789/   the real data
│   └── config.yaml
├── ..data -> ..2026_08_26_10_00_00.123456789
└── config.yaml -> ..data/config.yaml
```

An update writes a whole new timestamped directory, then renames a new
`..data` symlink over the old one. The rename is atomic — a reader never
sees a half-published ConfigMap — and it is the only event the update
produces. `config.yaml` is never written to.

A watcher that watches `config.yaml` for writes therefore sees nothing at
all. This package watches the directory and treats an event for any
`..`-prefixed name — `..data`, the `..data_tmp` link used to perform the
swap, and the staged `..<timestamp>` directory — as an event for the
configuration file. Every name the mechanism uses starts with two dots and
nothing else in a configuration directory does, which is what makes hot
reload work in a pod. There is an integration test that builds exactly the layout above,
performs the swap, and asserts that a new snapshot is published.

## The manifest

```yaml
volumeMounts:
  - name: config
    mountPath: /etc/myapp   # the directory, not a single file
    readOnly: true

volumes:
  - name: config
    configMap:
      name: myapp-config
```

```go
cfg, err := dynamicconfig.New[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
    dynamicconfig.WithValidator(validate),
)
```

Full manifests are in [examples/kubernetes](../examples/kubernetes).

## Do not use subPath

```yaml
volumeMounts:
  - name: config
    mountPath: /etc/myapp/config.yaml
    subPath: config.yaml   # ← the file will never update
```

A `subPath` mount is copied once when the container starts and is never
refreshed. The file cannot change, so there is nothing to watch and nothing
to reload. This is the most common reason hot reload appears not to work in
Kubernetes, and no library can work around it.

Mount the directory.

## What to expect, and when

```text
kubectl edit configmap
        │
        ▼
   API server                     immediately
        │
        ▼
     kubelet                      within its sync period (~1 minute by default,
        │                         plus cache TTL; not instant)
        ▼
projected volume swap             atomic
        │
        ▼
 filesystem event                 immediately
        │
        ▼
    debounce                      200 ms by default
        │
        ▼
read ─► decode ─► validate
        │
        ▼
atomic publication                Current() now returns the new snapshot
```

The pod is not restarted, and its UID does not change. The delay between
`kubectl edit` and the new generation is the kubelet's, not this package's;
`kubelet --sync-frequency` and the ConfigMap cache TTL govern it.

An **immutable** ConfigMap (`immutable: true`) is never updated in place, so
there is nothing to reload. Changing it means creating a new ConfigMap and
updating the pod spec, which restarts the pod.

## A ConfigMap and a Secret together

The usual production shape is two volumes: a ConfigMap with the
configuration, and a Secret with the credentials it deliberately leaves out.
They are layers of one configuration, so they belong in one instance:

```yaml
volumeMounts:
  - name: config
    mountPath: /etc/myapp
    readOnly: true
  - name: secrets
    mountPath: /etc/myapp/secrets
    readOnly: true
```

```go
cfg, err := dynamicconfig.NewSealed[AppConfig](
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/config.yaml"),
    dynamicconfig.WithConfigFile[AppConfig]("/etc/myapp/secrets/secret.yaml"),
    dynamicconfig.WithValidator(validate),
)
```

Both are projected volumes, both are watched — each in its own directory —
and either one being republished reloads the whole configuration. The
validator sees the merged result, so `database.dsn is empty` is a rule that
can exist even though the DSN and the pool settings arrive from different
volumes.

Rotating the Secret publishes a new snapshot without restarting the pod.
Deleting it does not fall back to the ConfigMap: the reload is rejected and
the last good configuration keeps serving.

## Secrets

Secret volumes use the same projected-volume mechanism and behave the same
way. Two things are worth saying out loud:

- Reload a Secret and the new value is in memory immediately. Anything the
  application derived from the old value — an open connection pool, a
  cached client, a signed token — is not reloaded by this package. Subscribe
  and re-create what needs re-creating.
- This package never logs configuration values, and errors name paths and
  stages rather than contents. An application can still leak them itself:
  `slog.Info("config", "value", cfg.Current())` prints the password. See
  [security.md](security.md).

## Probes

```go
mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
    if cfg.Current() == nil {
        w.WriteHeader(http.StatusServiceUnavailable)

        return
    }

    w.WriteHeader(http.StatusOK)
})
```

Readiness should stay true when a *reload* is rejected. The pod is still
serving the last good configuration, and removing it from the load balancer
because somebody typed a bad value into a ConfigMap turns a harmless mistake
into an outage. Report the rejection through logs, `Status().FailedReloads`
and an alert.

Startup is the opposite case. A pod that *starts* with a ConfigMap it cannot
understand has no last-known-good to fall back to, `New` fails, the process
exits, and `CrashLoopBackOff` makes the mistake loud. That is the intended
behaviour.

## Status as metrics

`Status()` carries no configuration values, so it is safe to expose:

```text
dynamic_config_generation              Status.Generation
dynamic_config_reload_success_total    Status.SuccessfulReloads
dynamic_config_reload_failure_total    Status.FailedReloads
dynamic_config_events_dropped_total    Status.DroppedEvents
dynamic_config_last_success_timestamp  Status.LastSuccess
```

A useful alert: `FailedReloads` increasing while `Generation` stays flat —
the ConfigMap changed and the pod is refusing it.
