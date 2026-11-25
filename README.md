# adept

**Adept**: an alchemist who has attained the secret knowledge and performs the operations.

The adept executes rituals for the Helmetica framework: it runs the Job of the `Definition` an `Action` names and tracks it to completion, and schedules each instance's maintenance inside a `MaintenanceWindow`.

Documentation: https://docs.helmetica.io/adept/

## Development

```bash
kubectl apply -k config/crd --server-side
just run          # in a second terminal
kubectl apply -k config/samples
just test
just docs         # preview the docs
```
