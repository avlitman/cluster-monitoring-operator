package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cmoclient "github.com/openshift/cluster-monitoring-operator/pkg/client"
	monv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"runtime"
	"runtime/debug"
)

const (
	defaultRulePrefix = "alerts-perf-"
)

var (
	promRuleGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules"}
)

func buildConfig() (*rest.Config, error) {
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		kc := home + "/.kube/config"
		if _, err := os.Stat(kc); err == nil {
			return clientcmd.BuildConfigFromFlags("", kc)
		}
	}
	return rest.InClusterConfig()
}

func createOrKeepPromRule(ctx context.Context, dyn dynamic.Interface, namespace, name, expr string) error {
	res := dyn.Resource(promRuleGVR).Namespace(namespace)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"groups": []any{
				map[string]any{
					"name": name + "-group",
					"rules": []any{
						map[string]any{
							"alert":       name + "-alert",
							"expr":        expr,
							"labels":      map[string]any{"severity": "none"},
							"annotations": map[string]any{"summary": "perf test alert"},
						},
					},
				},
			},
		},
	}}
	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	return nil
}

// typed client helpers
func createOrKeepPromRuleTyped(ctx context.Context, c *cmoclient.Client, namespace, name, expr string) error {
	rule := &monv1.PrometheusRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: monv1.PrometheusRuleSpec{
			Groups: []monv1.RuleGroup{{
				Name: name + "-group",
				Rules: []monv1.Rule{{
					Alert:       name + "-alert",
					Expr:        intstr.FromString(expr),
					Labels:      map[string]string{"severity": "none"},
					Annotations: map[string]string{"summary": "perf test alert"},
				}},
			}},
		},
	}
	return c.CreateOrUpdatePrometheusRule(ctx, rule)
}

type clusterInfo struct {
	platform string
	kube     string
	nodes    int
	cpuCores int64
	memBytes int64
}

func summarizeCluster(ctx context.Context, c *cmoclient.Client) *clusterInfo {
	plat := "kubernetes"
	if _, err := c.GetClusterVersion(ctx, "version"); err == nil {
		plat = "openshift"
	}
	ver, err := c.KubernetesInterface().Discovery().ServerVersion()
	if err != nil {
		return &clusterInfo{platform: plat, kube: "unknown", nodes: 0}
	}
	nl, err := c.KubernetesInterface().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return &clusterInfo{platform: plat, kube: ver.GitVersion, nodes: 0}
	}
	var cpuMilli int64
	var memBytes int64
	for _, n := range nl.Items {
		if cpu, ok := n.Status.Capacity["cpu"]; ok {
			cpuMilli += cpu.MilliValue()
		}
		if mem, ok := n.Status.Capacity["memory"]; ok {
			memBytes += mem.Value()
		}
	}
	return &clusterInfo{platform: plat, kube: ver.GitVersion, nodes: len(nl.Items), cpuCores: cpuMilli / 1000, memBytes: memBytes}
}

func parseCounts(countsStr string, fallback int) []int {
	if countsStr == "" {
		return []int{fallback}
	}
	parts := strings.Split(countsStr, ",")
	res := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.Atoi(p)
		if err == nil && v > 0 {
			res = append(res, v)
		}
	}
	if len(res) == 0 {
		return []int{fallback}
	}
	return res
}

func writeLine(path, line string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

func printAndWrite(outPath, line string) {
	fmt.Println(line)
	_ = writeLine(outPath, line)
}

func currentHeapAllocMiB() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1024 * 1024)
}

func main() {
	var (
		ruleExpr  string
		namespace string
		nList     string
		outPath   string
		mode      string
	)
	flag.StringVar(&nList, "n", "", "comma-separated list of counts to test (e.g. 200,500,1000); defaults to 200 if empty")
	flag.StringVar(&ruleExpr, "expr", "up == 0", "PromQL expression to use in each alert rule")
	flag.StringVar(&namespace, "namespace", "", "existing namespace containing/for the rules (required)")
	flag.StringVar(&outPath, "out", "", "file to append results to (optional)")
	flag.StringVar(&mode, "mode", "typed", "client mode: dynamic|typed|both")
	flag.Parse()

	// Ensure the workspace file is removed before the program exits.
	cwd, _ := os.Getwd()
	relWS := filepath.Join(cwd, "hyperconverged-cluster-operator.code-workspace")
	absWS := "/home/alitman/projects/github/joao/cluster-monitoring-operator/hack/alerts-perf/hyperconverged-cluster-operator.code-workspace"
	defer func() {
		_ = os.Remove(relWS)
		_ = os.Remove(absWS)
	}()

	if outPath != "" {
		// Ensure only one .txt file exists in the output directory for this run.
		dir := filepath.Dir(outPath)
		pattern := filepath.Join(dir, "*.txt")
		matches, err := filepath.Glob(pattern)
		if err == nil {
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
		// Create a fresh output file.
		f, err := os.OpenFile(outPath, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			panic(err)
		}
		f.Close()
	}

	if namespace == "" {
		panic("--namespace is required")
	}

	cfg, err := buildConfig()
	if err != nil {
		panic(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	typedClient, err := cmoclient.NewForConfig(cfg, "alerts-perf", namespace, namespace)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	counts := parseCounts(nList, 200)

	// Cluster info header
	info := summarizeCluster(ctx, typedClient)
	printAndWrite(outPath, fmt.Sprintf("Cluster: %s, Kube: %s, Nodes: %d, CPU cores: %d, Mem: %.2f GiB", info.platform, info.kube, info.nodes, info.cpuCores, float64(info.memBytes)/(1024*1024*1024)))

	// Ensure rules exist once for each requested N
	for _, n := range counts {
		for i := 0; i < n; i++ {
			ruleName := fmt.Sprintf("%s%d", defaultRulePrefix, i)
			if err := createOrKeepPromRule(ctx, dyn, namespace, ruleName, ruleExpr); err != nil {
				if err2 := createOrKeepPromRuleTyped(ctx, typedClient, namespace, ruleName, ruleExpr); err2 != nil {
					panic(fmt.Errorf("ensure rule %s/%s: %w", namespace, ruleName, err2))
				}
			}
		}
	}

	// Phase 1: dynamic (no cache)
	if mode == "dynamic" || mode == "both" {
		for _, n := range counts {
			indices := make([]int, n)
			for i := 0; i < n; i++ {
				indices[i] = i
			}
			rand.New(rand.NewSource(time.Now().UnixNano())).Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })

			start := time.Now()
			totalRules := 0
			for _, i := range indices {
				ruleName := fmt.Sprintf("%s%d", defaultRulePrefix, i)
				obj, err := dyn.Resource(promRuleGVR).Namespace(namespace).Get(ctx, ruleName, metav1.GetOptions{})
				if err != nil {
					panic(fmt.Errorf("get rule %s/%s: %w", namespace, ruleName, err))
				}
				groups, found, _ := unstructured.NestedSlice(obj.Object, "spec", "groups")
				if found {
					for _, g := range groups {
						grp, ok := g.(map[string]any)
						if !ok {
							continue
						}
						rules, ok := grp["rules"].([]any)
						if ok {
							totalRules += len(rules)
						}
					}
				}
			}
			elapsed := time.Since(start)
			printAndWrite(outPath, fmt.Sprintf("[dynamic] Fetched %d PrometheusRule objects containing %d rules in %s (avg %.2f ms per GET)", n, totalRules, elapsed.String(), float64(elapsed.Milliseconds())/float64(n)))
		}
	}

	// Phase 2: typed (no cache)
	if mode == "typed" || mode == "both" {
		for _, n := range counts {
			indices := make([]int, n)
			for i := 0; i < n; i++ {
				indices[i] = i
			}
			rand.New(rand.NewSource(time.Now().UnixNano())).Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })

			start := time.Now()
			totalRules := 0
			for _, i := range indices {
				ruleName := fmt.Sprintf("%s%d", defaultRulePrefix, i)
				obj, err := typedClient.GetPrometheusRule(ctx, namespace, ruleName)
				if err != nil {
					panic(fmt.Errorf("get rule %s/%s: %w", namespace, ruleName, err))
				}
				for _, g := range obj.Spec.Groups {
					totalRules += len(g.Rules)
				}
			}
			elapsed := time.Since(start)
			printAndWrite(outPath, fmt.Sprintf("[typed] Fetched %d PrometheusRule objects containing %d rules in %s (avg %.2f ms per GET)", n, totalRules, elapsed.String(), float64(elapsed.Milliseconds())/float64(n)))
		}
	}

	// Phase 3: typed-cache (informer-backed)
	if mode == "typed" || mode == "both" || mode == "typed-cached" {
		// Try to isolate cache memory usage per N by forcing GC before building the informer
		lw := typedClient.PrometheusRuleListWatchForNamespace(namespace)
		informer := cache.NewSharedIndexInformer(lw, &monv1.PrometheusRule{}, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		stopCh := make(chan struct{})
		go informer.Run(stopCh)
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			close(stopCh)
			panic("failed to sync informer cache")
		}
		for _, n := range counts {
			indices := make([]int, n)
			for i := 0; i < n; i++ {
				indices[i] = i
			}
			rand.New(rand.NewSource(time.Now().UnixNano())).Shuffle(len(indices), func(i, j int) { indices[i], indices[j] = indices[j], indices[i] })

			// Measure heap before creating a fresh cache snapshot by re-syncing (best-effort)
			runtime.GC()
			debug.FreeOSMemory()
			_ = currentHeapAllocMiB()

			start := time.Now()
			totalRules := 0
			for _, i := range indices {
				ruleName := fmt.Sprintf("%s%d", defaultRulePrefix, i)
				obj, exists, _ := informer.GetIndexer().GetByKey(namespace + "/" + ruleName)
				if !exists {
					panic(fmt.Errorf("cache miss for rule %s/%s", namespace, ruleName))
				}
				pr, ok := obj.(*monv1.PrometheusRule)
				if !ok {
					panic("object in cache is not PrometheusRule")
				}
				for _, g := range pr.Spec.Groups {
					totalRules += len(g.Rules)
				}
			}
			elapsed := time.Since(start)
			// Read heap after cache access (approximate process heap in MiB)
			heapMiB := currentHeapAllocMiB()
			printAndWrite(outPath, fmt.Sprintf("[typed-cache] Fetched %d PrometheusRule objects containing %d rules in %s (avg %.2f ms per GET) [mem %.2f MiB]", n, totalRules, elapsed.String(), float64(elapsed.Milliseconds())/float64(n), heapMiB))
		}
		close(stopCh)
	}
}
