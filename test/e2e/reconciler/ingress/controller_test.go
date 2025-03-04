/*
Copyright 2021 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package workspace

import (
	"context"
	"embed"
	"path/filepath"
	"testing"
	"time"

	"github.com/kcp-dev/logicalcluster"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/networking/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	kubernetesclientset "k8s.io/client-go/kubernetes"
	networkingclient "k8s.io/client-go/kubernetes/typed/networking/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/kcp/pkg/syncer/shared"
	kubefixtures "github.com/kcp-dev/kcp/test/e2e/fixtures/kube"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

//go:embed *.yaml
var embeddedResources embed.FS

const testNamespace = "ingress-controller-test"
const existingServiceName = "existing-service"

func TestIngressController(t *testing.T) {
	t.Parallel()

	var testCases = []struct {
		name string
		work func(ctx context.Context, t *testing.T, sourceClient, sinkClient networkingclient.NetworkingV1Interface)
	}{
		{
			name: "ingress lifecycle",
			work: func(ctx context.Context, t *testing.T, sourceClient, sinkClient networkingclient.NetworkingV1Interface) {
				// We create a root ingress. Ingress is excluded (through a hack) in namespace controller to be labeled.
				// The ingress-controller will take over the labelling of the leaves. After that the normal syncer will
				// sync the leaves into the physical cluster.

				t.Logf("Creating ingress in source cluster")
				ingressYaml, err := embeddedResources.ReadFile("ingress.yaml")
				require.NoError(t, err, "failed to read ingress")
				var rootIngress *v1.Ingress
				err = yaml.Unmarshal(ingressYaml, &rootIngress)
				require.NoError(t, err, "failed to unmarshal ingress")
				rootIngress, err = sourceClient.Ingresses(testNamespace).Create(ctx, rootIngress, metav1.CreateOptions{})
				require.NoError(t, err, "failed to create ingress")

				nsLocator := shared.NamespaceLocator{LogicalCluster: logicalcluster.From(rootIngress), Namespace: rootIngress.Namespace}
				targetNamespace, err := shared.PhysicalClusterNamespaceName(nsLocator)
				require.NoError(t, err, "error determining namespace mapping for %v", nsLocator)

				t.Logf("Waiting for ingress to be synced to sink cluster to namespace %s", targetNamespace)
				require.Eventually(t, func() bool {
					got, err := sinkClient.Ingresses(targetNamespace).List(ctx, metav1.ListOptions{})
					if err != nil {
						klog.Errorf("failed to list ingresses in sink cluster: %v", err)
						return false
					}
					if len(got.Items) != 1 {
						return false
					}
					framework.RequireNoDiff(t, got.Items[0].Spec, rootIngress.Spec)
					return true
				}, wait.ForeverTestTimeout, time.Second, "did not see the ingress synced on sink cluster")

				t.Logf("Updating root ingress in source cluster")
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					got, err := sourceClient.Ingresses(testNamespace).Get(ctx, rootIngress.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					got.Spec.Rules[0].Host = "valid-ingress-2.kcp-apps.127.0.0.1.nip.io"
					_, err = sourceClient.Ingresses(testNamespace).Update(ctx, got, metav1.UpdateOptions{})
					return err
				})
				require.NoError(t, err, "failed updating the ingress object in the source cluster")

				t.Logf("Waiting for ingress update to be synced to sink cluster")
				require.Eventually(t, func() bool {
					got, err := sinkClient.Ingresses(targetNamespace).List(ctx, metav1.ListOptions{})
					if err != nil {
						klog.Errorf("failed to list ingresses in sink cluster: %v", err)
						return false
					}
					if len(got.Items) != 1 {
						return false
					}
					return got.Items[0].Spec.Rules[0].Host == "valid-ingress-2.kcp-apps.127.0.0.1.nip.io"
				}, wait.ForeverTestTimeout, time.Second, "did not see Ingress spec updated on sink cluster")
			},
		},
	}

	source := framework.SharedKcpServer(t)
	orgClusterName := framework.NewOrganizationFixture(t, source)

	for i := range testCases {
		testCase := testCases[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancelFunc := context.WithCancel(context.Background())
			t.Cleanup(cancelFunc)

			clusterName := framework.NewWorkspaceFixture(t, source, orgClusterName, "Universal")

			// clients
			sourceConfig := source.DefaultConfig(t)
			sourceKubeClusterClient, err := kubernetesclientset.NewClusterForConfig(sourceConfig)
			require.NoError(t, err)
			sourceKubeClient := sourceKubeClusterClient.Cluster(clusterName)

			syncerFixture := framework.SyncerFixture{
				ResourcesToSync:      sets.NewString("ingresses.networking.k8s.io", "services"),
				UpstreamServer:       source,
				WorkspaceClusterName: clusterName,
				InstallCRDs: func(config *rest.Config, isLogicalCluster bool) {
					if !isLogicalCluster {
						// Only need to install services and ingresses in a logical cluster
						return
					}
					sinkCrdClient, err := apiextensionsclientset.NewForConfig(config)
					require.NoError(t, err, "failed to create apiextensions client")
					t.Logf("Installing test CRDs into sink cluster...")
					kubefixtures.Create(t, sinkCrdClient.ApiextensionsV1().CustomResourceDefinitions(),
						metav1.GroupResource{Group: "core.k8s.io", Resource: "services"},
						metav1.GroupResource{Group: "networking.k8s.io", Resource: "ingresses"},
					)
					require.NoError(t, err)
				},
			}.Start(t)

			t.Log("Waiting for ingresses crd to be imported and available in the source cluster...")
			require.Eventually(t, func() bool {
				_, err := sourceKubeClient.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Logf("error seen waiting for ingresses crd to become active: %v", err)
					return false
				}
				return true
			}, wait.ForeverTestTimeout, time.Millisecond*100)

			t.Log("Waiting for services crd to be imported and available in the source cluster...")
			require.Eventually(t, func() bool {
				_, err := sourceKubeClient.CoreV1().Services("").List(ctx, metav1.ListOptions{})
				if err != nil {
					t.Logf("error seen waiting for services crd to become active: %v", err)
					return false
				}
				return true
			}, wait.ForeverTestTimeout, time.Millisecond*100)

			t.Log("Creating namespace in source cluster...")
			_, err = sourceKubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			t.Log("Creating service in source cluster...")
			_, err = sourceKubeClient.CoreV1().Services(testNamespace).Create(ctx, &corev1.Service{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name: existingServiceName,
				},
				Spec: corev1.ServiceSpec{
					Ports: []corev1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: corev1.ProtocolTCP,
						},
					},
					Selector: map[string]string{
						"app": existingServiceName,
					},
				},
				Status: corev1.ServiceStatus{},
			}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to install service in source cluster")

			t.Log("Starting ingress-controller...")
			envoyListenerPort, err := framework.GetFreePort(t)
			require.NoError(t, err, "failed to pick envoy listener port")
			xdsListenerPort, err := framework.GetFreePort(t)
			require.NoError(t, err, "failed to pick xds listener port")
			artifactDir, err := framework.CreateTempDirForTest(t, "artifacts")
			require.NoError(t, err, "failed to create artifact dir for ingress-controller")
			kubeconfigPath := filepath.Join(artifactDir, "ingress-controller.kubeconfig")
			adminConfig, err := source.RawConfig()
			require.NoError(t, err)

			ingressConfig := clientcmdapi.Config{
				Clusters: map[string]*clientcmdapi.Cluster{
					"ingress-workspace": adminConfig.Clusters["system:admin"],
				},
				Contexts: map[string]*clientcmdapi.Context{
					"ingress-workspace": {
						Cluster:  "ingress-workspace",
						AuthInfo: "admin",
					},
				},
				AuthInfos: map[string]*clientcmdapi.AuthInfo{
					"admin": adminConfig.AuthInfos["admin"],
				},
				CurrentContext: "ingress-workspace",
			}
			err = clientcmd.WriteToFile(ingressConfig, kubeconfigPath)
			require.NoError(t, err, "failed to write kubeconfig to file")

			executableName := "ingress-controller"
			cmd := append(framework.DirectOrGoRunCommand(executableName),
				"--kubeconfig="+kubeconfigPath,
				"--envoy-listener-port="+envoyListenerPort,
				"--envoy-xds-port="+xdsListenerPort,
			)
			ingressController := framework.NewAccessory(t, artifactDir, executableName, cmd...)
			err = ingressController.Run(t, framework.WithLogStreaming)
			require.NoError(t, err, "failed to start ingress controller")

			t.Log("Starting test...")
			testCase.work(ctx, t, sourceKubeClient.NetworkingV1(), syncerFixture.DownstreamKubeClient.NetworkingV1())
		})
	}
}
