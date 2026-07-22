# adept

**Adept**: an alchemist who has attained the secret knowledge and performs the operations.

The adept executes rituals for the Helmetica framework: it watches `Action`
CRs (`rituals.helmetica.io/v1`), creates a Kubernetes Job from the
`Definition` named by `spec.type`, and tracks the Job to `Succeeded` or
`Failed` on the Action status. Definitions are packaged into reagent charts
by the [transmuter](../transmuter); ferment ships defaults.

The Definition and Job live in the *instance namespace*, resolved from where
the Action was created:

* **Instance namespace** (label `chrysopoeia.io/instance`, e.g. the instance
  is plain helm-installed): the Action runs in place.
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
* Scheduling/cron (recurring rituals) and re-runs on spec change.
