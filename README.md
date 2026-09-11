# adept

**Adept**: an alchemist who has attained the secret knowledge and performs the operations.

The adept executes rituals for the Helmetica framework: it watches `Action`
CRs (`rituals.helmetica.io/v1`), creates a Kubernetes Job from the
`Definition` named by `spec.type`, and tracks the Job to `Succeeded` or
`Failed` on the Action status. Definitions are packaged into reagent charts
by the [transmuter](https://github.com/helmetica-framework/transmuter); ferment ships defaults.

The Definition and Job live in the *instance namespace*, resolved from where
the Action was created:

* **Instance namespace** (label `chrysopoeia.io/instance`): the Action runs
  in place. A namespace without any `chrysopoeia.io/*` annotation is not
  chryso-managed (e.g. the instance is plain helm-installed) and the Action
  also runs in place, even if it carries a claim reference.
* **Claim namespace**: the Action names its claim (`spec.claim`,
  `spec.apiVersion`, `spec.kind`); the adept reads the claim's
  `status.instanceNamespace` and runs the Job there. Until the claim
  resolves, the failure is reported in the Action's `status.message` and
  retried with exponential backoff.

## Glossary

| Term | Meaning |
| ---- | ------- |
| **Ritual** | A packaged `Definition` manifest describing an operational action (e.g. restart, maintenance). See the [transmuter README](../transmuter/README.md#glossary) for the full framework glossary. |
| **Transmuter** | The framework's chart tool: scaffolds and assays reagent charts, including the ritual Definitions the adept executes. |
| **Reagent** | A service chart wrapping an upstream (prima materia) chart; ships the Definitions for its service instance. |
| **Definition** | The ritual's job template (`rituals.helmetica.io/v1`, namespaced, no status). Scaffolded and assayed by the transmuter, shipped by ferment. |
| **Action** | A request to run a ritual: names a Definition via `spec.type`. The adept creates one Job per Action and mirrors its outcome in `status.phase` (`Pending` → `Running` → `Succeeded`/`Failed`). Terminal phases are final; no re-runs. Failures to resolve the instance namespace or the Definition are reported in `status.message`. |
| **MaintenanceWindow** | A named span in which maintenance may start (`rituals.helmetica.io/v1`, cluster-scoped, no status). Operators own them; instances reference one by name instead of carrying their own schedule, which is what lets maintenance be batched. One window may set `spec.default`, and instances naming no window take it. A validating webhook rejects a second window claiming it. |
| **Maintenance** | One instance's maintenance schedule (`rituals.helmetica.io/v1`, namespaced). Names the window to start in and the ritual to run, and produces the CronJob that fires it. A reagent renders one per instance; an empty spec is a complete schedule. |

## Maintenance windows

A `MaintenanceWindow` says when maintenance may start, in an operator's local
terms:

```yaml
apiVersion: rituals.helmetica.io/v1
kind: MaintenanceWindow
metadata:
  name: sunday-night
spec:
  daysOfWeek: [sunday, monday, tuesday, wednesday, thursday]
  time: "22:00"
  duration: 6h
  timeZone: Europe/Zurich
```

They are cluster-scoped: a handful of named windows serve instances across
every namespace, so a lot of instances can be maintained on one shared
schedule.

The duration is the time span when maintenance jobs may start.
It's not bound, so a job that starts 5 minutes before the window ends will still continue.

One window may set `spec.default`. Instances that name no window take it, so an
operator can move the whole fleet by editing one object. The validating webhook
rejects a second window claiming the default, since two of them would make the
choice arbitrary.

## Maintenance per instance

A `Maintenance` is one instance's half of the arrangement. It lives in the
instance namespace, and a reagent chart renders one per instance:

```yaml
apiVersion: rituals.helmetica.io/v1
kind: Maintenance
metadata:
  name: my-service
spec:
  window: sunday-night   # empty takes the default window
  ritual: maintenance    # the Definition to run, in this namespace
  suspend: false         # stops maintenance without losing the schedule
```

Every field is optional, so `spec: {}` is a complete schedule: the default
window, and the `maintenance` ritual the reagent ships.

The adept turns it into a CronJob in the same namespace, owned by the
`Maintenance`, and reports what the instance settled on:

```bash
kubectl get maintenances
NAME         WINDOW         RITUAL        SCHEDULE            SUSPENDED
my-service   sunday-night   maintenance   27 3 * * 1,2,3,4,5   false
```

`status.schedule` is where to look for when an instance actually runs. Instances
sharing a window are spread across it by a hash of the `Maintenance`'s
`namespace/name`, so they do not all start at once, and the offset stays put
across chart upgrades. The offset can push a start past midnight, which is why
the days above are shifted one on from the window's. When nothing resolves, the
reason is in `status.message` and the adept retries with backoff, since a chart
may render the `Maintenance` before the window or the ritual exists.

`spec.suspend` sets the CronJob's own `suspend` rather than deleting it, so a
suspended instance still shows up in `kubectl get cronjobs`.

## Quickstart

Against a kind (or any) cluster:

```bash
kubectl apply -k config/crd --server-side
just run          # in a second terminal
kubectl apply -k config/samples
kubectl get actions -w   # TYPE=restart, PHASE Pending -> Running -> Succeeded/Failed
```

The samples create a `restart` Definition (kubectl rollout restart of a
deployment), a `restart-now` Action that executes it, a `sunday-night`
MaintenanceWindow, a `maintenance` Definition, and a `my-service` Maintenance
that schedules it:

```bash
kubectl get maintenances,cronjobs
```

Full deployment (CRDs, RBAC, manager, webhook) is packaged under
`config/default`:

```bash
kubectl apply -k config/default --server-side
```

That overlay needs [cert-manager](https://cert-manager.io) in the cluster. The
`MaintenanceWindow` validating webhook is served over TLS; cert-manager issues
the certificate and injects its CA into the webhook configuration.

## Out of scope (for now)

* `spec.args` injection into the Job — stored, not injected.
* Re-runs on spec change: terminal Action phases are final.
