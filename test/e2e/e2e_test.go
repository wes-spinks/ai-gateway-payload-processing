package e2e

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// Provider represents a provider configuration for testing.
type Provider struct {
	Name     string
	Provider string
}

var providers = []Provider{
	{Name: "e2e-openai", Provider: "openai"},
	{Name: "e2e-anthropic", Provider: "anthropic"},
	{Name: "e2e-azure", Provider: "azure-openai"},
	{Name: "e2e-vertex", Provider: "vertex"},
	// bedrock-openai: uncomment once the translator is in the odh-stable image
	// {Name: "e2e-bedrock", Provider: "bedrock-openai"},
}

func createProviderResources(p Provider) {
	// Secret with dummy API key
	kubectlApplyLiteral(fmt.Sprintf(`
apiVersion: v1
kind: Secret
metadata:
  name: %s-api-key
  namespace: %s
  labels:
    inference.networking.k8s.io/bbr-managed: "true"
type: Opaque
stringData:
  api-key: e2e-test-key-%s
`, p.Name, nsName, p.Provider))

	// ExternalModel CR
	kubectlApplyLiteral(fmt.Sprintf(`
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: %s
  namespace: %s
spec:
  provider: %s
  endpoint: %s
  credentialRef:
    name: %s-api-key
`, p.Name, nsName, p.Provider, simulatorEP, p.Name))

	// ExternalName Service pointing to simulator
	kubectlApplyLiteral(fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: %s-backend
  namespace: %s
spec:
  type: ExternalName
  externalName: e2e-simulator.external
  ports:
  - port: 443
    protocol: TCP
`, p.Name, nsName))

	// HTTPRoute with path-based + header-based routing
	hostnamesYAML := ""
	if gatewayHostname != "" {
		hostnamesYAML = fmt.Sprintf("\n  hostnames:\n  - %s", gatewayHostname)
	}
	kubectlApplyLiteral(fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: e2e-%s
  namespace: %s
spec:%s
  parentRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: %s
    namespace: %s
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /%s/
    backendRefs:
    - name: %s-backend
      port: 443
    filters:
    - type: URLRewrite
      urlRewrite:
        path:
          type: ReplacePrefixMatch
          replacePrefixMatch: /
  - matches:
    - headers:
      - name: X-Gateway-Model-Name
        type: Exact
        value: %s
      path:
        type: PathPrefix
        value: /
    backendRefs:
    - name: %s-backend
      port: 443
`, p.Name, nsName, hostnamesYAML, gatewayName, gatewayNs, p.Name, p.Name, p.Name, p.Name))
}

func deleteProviderResources(p Provider) {
	kubectlDeleteResource("httproute", "e2e-"+p.Name, nsName)
	kubectlDeleteResource("service", p.Name+"-backend", nsName)
	kubectlDeleteResource("externalmodel", p.Name, nsName)
	kubectlDeleteResource("secret", p.Name+"-api-key", nsName)
}

// getCurlCommand builds a curl command to send a chat completion request
// from inside the cluster to the gateway service.
func getCurlCommand(modelName string) []string {
	body := map[string]any{
		"model":    modelName,
		"messages": []map[string]string{{"role": "user", "content": "hello from " + modelName}},
	}
	bodyBytes, _ := json.Marshal(body)

	// Access gateway service from inside the cluster via DNS.
	// Istio creates a service named <gateway-name>-istio for each Gateway (vanilla Istio).
	// OpenShift uses <gateway-name>-<gateway-class> pattern.
	gatewayURL := fmt.Sprintf("http://%s.%s.svc:80/%s/v1/chat/completions",
		gatewaySvcName, gatewayNs, modelName)

	cmd := []string{
		"curl", "-si", "--max-time", strconv.Itoa(int(curlTimeout.Seconds())),
		gatewayURL,
		"-H", "Content-Type: application/json",
		"-H", "Connection: close",
	}

	// Add Host header for OpenShift gateways with specific listener hostnames
	if gatewayHostname != "" {
		cmd = append(cmd, "-H", fmt.Sprintf("Host: %s", gatewayHostname))
	}

	cmd = append(cmd, "-d", string(bodyBytes))
	return cmd
}

var _ = ginkgo.Describe("BBR Plugin Chain", func() {
	ginkgo.When("BBR is deployed with all plugins", func() {
		for _, p := range providers {
			p := p // capture range variable

			ginkgo.It(fmt.Sprintf("should return 200 for provider %s", p.Provider), func() {
				curlCmd := getCurlCommand(p.Name)

				var resp string
				var err error

				// Retry with backoff (route propagation may take time)
				gomega.Eventually(func() bool {
					resp, err = execInPod("curl", nsName, "curl", curlCmd)
					if err != nil {
						_, _ = fmt.Fprintf(ginkgo.GinkgoWriter, "curl exec error: %v\n", err)
						return false
					}
					return strings.Contains(resp, "200 OK") || strings.Contains(resp, "HTTP/1.1 200")
				}, curlTimeout*3, 5*time.Second).Should(gomega.BeTrue(),
					fmt.Sprintf("Expected 200 for %s, got:\n%s", p.Provider, resp))
			})

			ginkgo.It(fmt.Sprintf("should return OpenAI format response for provider %s", p.Provider), func() {
				curlCmd := getCurlCommand(p.Name)

				var resp string
				gomega.Eventually(func() bool {
					var err error
					resp, err = execInPod("curl", nsName, "curl", curlCmd)
					return err == nil && strings.Contains(resp, "200 OK")
				}, curlTimeout*3, 5*time.Second).Should(gomega.BeTrue())

				// Extract JSON body from response (after empty line)
				parts := strings.SplitN(resp, "\r\n\r\n", 2)
				gomega.Expect(len(parts)).To(gomega.Equal(2), "Expected headers and body separated by empty line")

				body := strings.TrimSpace(parts[1])
				var result map[string]any
				err := json.Unmarshal([]byte(body), &result)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("Failed to parse JSON: %s", body))

				gomega.Expect(result).To(gomega.HaveKey("choices"), fmt.Sprintf("Missing 'choices': %v", result))
				gomega.Expect(result).To(gomega.HaveKey("model"), fmt.Sprintf("Missing 'model': %v", result))

				choices, ok := result["choices"].([]any)
				gomega.Expect(ok).To(gomega.BeTrue())
				gomega.Expect(len(choices)).To(gomega.BeNumerically(">", 0))
			})
		}
	})
})
