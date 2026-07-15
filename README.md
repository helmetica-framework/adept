# adept

**Adept**: an alchemist who has attained the secret knowledge and performs the operations.

The adept executes rituals for the Helmetica framework: it watches `Action`
CRs (`rituals.helmetica.io/v1`), creates a Kubernetes Job from the
same-namespace `Definition` named by `spec.type`, and tracks the Job to
`Succeeded` or `Failed` on the Action status. Definitions are packaged into
reagent charts by the [transmuter](../transmuter); ferment ships defaults.

## Glossary

| Term | Meaning |
| ---- | ------- |
| **Ritual** | A packaged `Definition` manifest describing an operational action (e.g. restart, maintenance). See the [transmuter README](../transmuter/README.md#glossary) for the full framework glossary. |
| **Definition** | The ritual's job template (`rituals.helmetica.io/v1`, namespaced, no status). Scaffolded and assayed by the transmuter, shipped by ferment. |
| **Action** | A request to run a ritual: names a Definition via `spec.type`. The adept creates one Job per Action and mirrors its outcome in `status.phase` (`Pending` → `Running` → `Succeeded`/`Failed`). Terminal phases are final; no re-runs. |

## Quickstart

Against a kind (or any) cluster:

```bash
kubectl apply -k config/crd
make run          # in a second terminal
kubectl apply -k config/samples
kubectl get actions -w   # TYPE=restart, PHASE Pending -> Running -> Succeeded/Failed
```

The samples create a `restart` Definition (kubectl rollout restart of a
deployment) and a `restart-now` Action that executes it.

Full deployment (CRDs, RBAC, manager) is packaged under `config/default`:

```bash
kubectl apply -k config/default
```

## Out of scope (for now)

* `spec.args` injection into the Job — stored, not injected.
* `spec.claim` semantics (platform/service-instance integration) — stored, unused.
* Scheduling/cron (recurring rituals) and re-runs on spec change.
