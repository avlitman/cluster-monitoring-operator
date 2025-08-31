### Setup
- KUBECONFIG points to a cluster with the PrometheusRule CRD installed
- Go installed (required to run `go run` locally)
- A target namespace where you have RBAC to read/write PrometheusRules (e.g., `openshift-monitoring`).

### Flags
- --namespace: target namespace (required)
- --n: comma-separated counts to test (e.g., `200,500`)
- --expr: PromQL for created rules (default: `up == 0`)
- --out: path to a .txt results file
- --mode: `dynamic` | `typed` | `both` (default: `typed`)

### How to run
1) Point KUBECONFIG to your cluster:
```bash
export KUBECONFIG=/path/to/kubeconfig
```
2) From this repository root, change to the tool directory:
```bash
cd hack/alerts-perf
```
3) Run:
```bash
go run . --namespace openshift-monitoring --n 200,500 --expr "up == 0" --out results.txt --mode both
```

### Example output
```text
Cluster: kubernetes, Kube: v1.33.5, Nodes: 1, CPU cores: 6, Mem: 4.80 GiB
[dynamic] Fetched 200 PrometheusRule objects containing 200 rules in 40.000495484s (avg 200.00 ms per GET)
[dynamic] Fetched 500 PrometheusRule objects containing 500 rules in 1m39.997884506s (avg 199.99 ms per GET)
[typed] Fetched 200 PrometheusRule objects containing 200 rules in 38.002282429s (avg 190.01 ms per GET)
[typed] Fetched 500 PrometheusRule objects containing 500 rules in 1m38.00588788s (avg 196.01 ms per GET)
[typed-cache] Fetched 200 PrometheusRule objects containing 200 rules in 140.33µs (avg 0.00 ms per GET) [mem 13.43 MiB]
[typed-cache] Fetched 500 PrometheusRule objects containing 500 rules in 290.699µs (avg 0.00 ms per GET) [mem 13.45 MiB]
```
