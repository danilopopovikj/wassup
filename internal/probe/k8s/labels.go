package k8s

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricLabels reads the metrics page of one ready pod that a selector
// names, through the API server as k8s.scrape does, and returns the label
// sets of every series of a metric on it. It is how wassup measure learns
// which services a router counts before it writes a binding that matches
// one of them.
func MetricLabels(ctx context.Context, c *Clients, namespace, selector, port, path, metric string) ([]map[string]string, error) {
	if c.Core == nil {
		return nil, fmt.Errorf("no core client")
	}
	list, err := c.Core.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var ready []corev1.Pod
	for _, pd := range list.Items {
		if pd.DeletionTimestamp == nil && pd.Status.Phase == corev1.PodRunning && podReady(&pd) {
			ready = append(ready, pd)
		}
	}
	if len(ready) == 0 {
		return nil, fmt.Errorf("no ready pod in namespace %s matches %q", namespace, selector)
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Name < ready[j].Name })
	cfg := scrapeConfig{namespace: namespace, port: port, path: path, scheme: "http"}
	body, err := c.proxy(ctx, cfg.pagePath(ready[0].Name))
	if err != nil {
		return nil, err
	}
	all, err := seriesOf(body, metric)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(all))
	for _, s := range all {
		out = append(out, s.labels)
	}
	return out, nil
}
