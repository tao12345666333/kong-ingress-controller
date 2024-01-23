//go:build e2e_tests

package e2e

import (
	"bytes"
	"fmt"
	"math/rand"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kong/kubernetes-ingress-controller/v3/test"
	"github.com/kong/kubernetes-ingress-controller/v3/test/internal/helpers"
	"github.com/kong/kubernetes-testing-framework/pkg/utils/kubernetes/generators"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// -----------------------------------------------------------------------------
// E2E Performance tests
// -----------------------------------------------------------------------------

// TestBasicHTTPRoute will create a basic HTTP route and test its functionality
// against a Kong proxy. This test will be used to measure the performance of
// the KIC with OpenTelemetry.
func TestBasicPerf(t *testing.T) {
	t.Log("configuring all-in-one-dbless.yaml manifest test")
	t.Parallel()
	ctx, env := setupE2ETest(t)

	t.Log("deploying kong components")
	ManifestDeploy{Path: dblessPath}.Run(ctx, t, env)

	t.Log("deploying a minimal HTTP container deployment to test Ingress routes")
	container := generators.NewContainer("httpbin", test.HTTPBinImage, test.HTTPBinPort)
	deployment := generators.NewDeploymentForContainer(container)
	deployment, err := env.Cluster().Client().AppsV1().Deployments("default").Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Logf("exposing deployment %s via service", deployment.Name)
	service := generators.NewServiceForDeployment(deployment, corev1.ServiceTypeLoadBalancer)
	_, err = env.Cluster().Client().CoreV1().Services("default").Create(ctx, service, metav1.CreateOptions{})
	require.NoError(t, err)

	// I want to to create a large YAML file,
	// it includes 1000 ingress rules, every rule has a different host name and path.
	ingressTpl := `
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: test-ingress-%d
spec:
  ingressClassName: kong
  rules:
  - host: example-%d.com
    http:
      paths:
      - backend:
          service:
            name: httpbin
            port:
              number: 80
        path: /get
        pathType: Exact

`

	ingressYaml := ""
	for i := 0; i < 10000; i++ {
		ingressYaml += fmt.Sprintf(ingressTpl, i, i)
	}

	t1 := time.Now()
	// use kubectl apply the ingressYAML to kubernetes
	kubeconfig := getTemporaryKubeconfig(t, env)
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(ingressYaml)
	_, err = cmd.CombinedOutput()
	require.NoError(t, err)
	t2 := time.Now()

	t.Log("getting kong proxy IP after LB provisioning")
	proxyURLForDefaultIngress := "http://" + getKongProxyIP(ctx, t, env)

	t.Log("waiting for routes from Ingress to be operational")

	// create wait group to wait for all ingress rules to take effect
	randomList := getRandomList(10000)
	var wg sync.WaitGroup
	wg.Add(len(randomList))

	for _, i := range randomList {
		go func(i int) {
			defer wg.Done()

			req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/get", proxyURLForDefaultIngress), nil)
			require.NoError(t, err)
			req.Host = fmt.Sprintf("example-%d.com", i)

			require.Eventually(t, func() bool {
				resp, err := helpers.DefaultHTTPClient().Do(req)
				if err != nil {
					t.Logf("WARNING: error while waiting for %s: %v", proxyURLForDefaultIngress, err)
					return false
				}
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					// now that the ingress backend is routable
					b := new(bytes.Buffer)
					n, err := b.ReadFrom(resp.Body)
					require.NoError(t, err)
					require.True(t, n > 0)
					return strings.Contains(b.String(), "origin")
				}
				return false
			}, ingressWait, time.Millisecond*500)
		}(i)
	}

	wg.Wait()

	t4 := time.Now()

	t.Logf("time to apply 10000 ingress rules: %v", t2.Sub(t1))
	t.Logf("time to make 10000 ingress rules take effect: %v", t4.Sub(t2))
}

func getRandomList(n int) []int {
	if n <= 10 {
		return []int{0, n}
	}
	randPerm := rand.Perm(n)
	randPerm = randPerm[:10]
	randPerm = append(randPerm, 0, n-1)

	return randPerm
}
