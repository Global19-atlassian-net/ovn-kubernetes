package ovn

import (
	"context"
	"fmt"
	"net"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/urfave/cli/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Local gateway mode EgressIP Operations with", func() {
	var (
		app     *cli.App
		fakeOvn *FakeOVN
		tExec   *ovntest.FakeExec
	)

	getEgressIPStatusLenSafely := func(egressIPName string) func() int {
		return func() int {
			fakeOvn.controller.eIPAllocatorMutex.Lock()
			defer fakeOvn.controller.eIPAllocatorMutex.Unlock()
			tmp, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			return len(tmp.Status.Items)
		}
	}

	getEgressIPStatusSafely := func(egressIPName string) []egressipv1.EgressIPStatusItem {
		fakeOvn.controller.eIPAllocatorMutex.Lock()
		defer fakeOvn.controller.eIPAllocatorMutex.Unlock()
		tmp, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), egressIPName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		return tmp.Status.Items
	}

	BeforeEach(func() {
		// Restore global default values before each testcase
		config.PrepareTestConfig()

		app = cli.NewApp()
		app.Name = "test"
		app.Flags = config.Flags

		tExec = ovntest.NewLooseCompareFakeExec()
		fakeOvn = NewFakeOVN(tExec)

		config.Gateway.Mode = config.GatewayModeLocal
	})

	AfterEach(func() {
		fakeOvn.shutdown()
	})

	Context("IPv6 on pod UPDATE", func() {

		It("should remove OVN pod egress setup when EgressIP stops matching pod label", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressNamespace := newNamespace(namespace)
				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)

				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				mark := util.IPToUint32(egressIP.String())

				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", egressPod.Status.PodIP, fakeOvn.ovnNBClient)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf("lb_force_snat_ip=%s", nodeGatewayRouterIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", egressPod.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				podUpdate := newPod(namespace, podName, node1Name, podV6IP)

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-del ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0", egressPod.Status.PodIP),
					},
				)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not treat pod update if pod already had assigned IP when it got the ADD", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				mark := util.IPToUint32(egressIP.String())

				// Mock the fact that we have an IP in the OVN DB for this pod
				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", egressPod.Status.PodIP, fakeOvn.ovnNBClient)
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf("lb_force_snat_ip=%s", nodeGatewayRouterIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", egressPod.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				// Update a label, but continue matching
				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, map[string]string{
					"egress": "needed",
					"some":   "update",
				})

				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should treat pod update if pod did not have an assigned IP when it got the ADD", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				// Mock no IP in OVN DB for this pod
				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", "", fakeOvn.ovnNBClient)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				podUpdate := newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)

				mark := util.IPToUint32(egressIP.String())

				// Mock pod IP found in OVN DB
				cmd, err := fakeOvn.ovnNBClient.LSPSetDynamicAddresses(lsp, fmt.Sprintf("0a:00:00:00:00:01 %s", podUpdate.Status.PodIP))
				Expect(err).NotTo(HaveOccurred())
				err = cmd.Execute()
				Expect(err).NotTo(HaveOccurred())

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf("lb_force_snat_ip=%s", nodeGatewayRouterIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", podUpdate.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				_, err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Update(context.TODO(), podUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not treat pod DLETE if pod did not have an assigned IP when it got the ADD and we receive a DELETE before the IP UPDATE", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespace(namespace)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								"name": egressNamespace.Name,
							},
						},
					},
				}

				// Mock no pod IP in OVN DB
				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", "", fakeOvn.ovnNBClient)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				// Should not perform any ovn-nbctl commands as we never really added it to begin with
				err = fakeOvn.fakeClient.CoreV1().Pods(egressPod.Namespace).Delete(context.TODO(), egressPod.Name, *metav1.NewDeleteOptions(0))
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("IPv6 on namespace UPDATE", func() {

		It("should remove OVN pod egress setup when EgressIP stops matching", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				mark := util.IPToUint32(egressIP.String())

				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", egressPod.Status.PodIP, fakeOvn.ovnNBClient)

				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf("lb_force_snat_ip=%s", nodeGatewayRouterIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", egressPod.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-del ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0", egressPod.Status.PodIP),
					},
				)

				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not remove OVN pod egress setup when EgressIP stops matching, but pod never had any IP to begin with", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, "", egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)
				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", "", fakeOvn.ovnNBClient)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				namespaceUpdate := newNamespace(namespace)

				_, err = fakeOvn.fakeClient.CoreV1().Namespaces().Update(context.TODO(), namespaceUpdate, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("IPv6 on EgressIP UPDATE", func() {

		It("should delete and re-create", func() {
			app.Action = func(ctx *cli.Context) error {

				egressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8e0d")
				updatedEgressIP := net.ParseIP("0:0:0:0:0:feff:c0a8:8ffd")

				egressPod := *newPodWithLabels(namespace, podName, node1Name, podV6IP, egressPodLabel)
				egressNamespace := newNamespaceWithLabels(namespace, egressPodLabel)
				fakeOvn.start(ctx,
					&v1.NamespaceList{
						Items: []v1.Namespace{*egressNamespace},
					},
					&v1.PodList{
						Items: []v1.Pod{egressPod},
					},
				)

				node1 := setupNode(node1Name, []string{"0:0:0:0:0:feff:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e32", "0:0:0:0:0:feff:c0a8:8e1e"})
				node2 := setupNode(node2Name, []string{"0:0:0:0:0:fedf:c0a8:8e0c/64"}, []string{"0:0:0:0:0:feff:c0a8:8e23"})

				fakeOvn.controller.eIPAllocator[node1.name] = &node1
				fakeOvn.controller.eIPAllocator[node2.name] = &node2

				eIP := egressipv1.EgressIP{
					ObjectMeta: newEgressIPMeta(egressIPName),
					Spec: egressipv1.EgressIPSpec{
						EgressIPs: []string{
							egressIP.String(),
						},
						PodSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
						NamespaceSelector: metav1.LabelSelector{
							MatchLabels: egressPodLabel,
						},
					},
				}

				mark := util.IPToUint32(egressIP.String())

				lsp := fmt.Sprintf("%s_%s", egressPod.Namespace, egressPod.Name)
				populatePortAddresses(node1Name, lsp, "0a:00:00:00:00:01", egressPod.Status.PodIP, fakeOvn.ovnNBClient)
				fakeOvn.fakeExec.AddFakeCmd(
					&ovntest.ExpectedCmd{
						Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --format=table --no-heading --columns=options find logical_router name=GR_%s options:lb_force_snat_ip!=-", node2.name),
						Output: fmt.Sprintf("lb_force_snat_ip=%s", nodeGatewayRouterIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", egressPod.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				fakeOvn.controller.WatchEgressIP()

				_, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Create(context.TODO(), &eIP, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)

				statuses := getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(egressIP.String()))

				updateEIP, err := fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Get(context.TODO(), eIP.Name, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())

				updateEIP.Spec = egressipv1.EgressIPSpec{
					EgressIPs: []string{
						updatedEgressIP.String(),
					},
					PodSelector: metav1.LabelSelector{
						MatchLabels: egressPodLabel,
					},
					NamespaceSelector: metav1.LabelSelector{
						MatchLabels: egressPodLabel,
					},
				}

				mark = util.IPToUint32(updatedEgressIP.String())

				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-del ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0", egressPod.Status.PodIP),
					},
				)
				fakeOvn.fakeExec.AddFakeCmdsNoOutputNoError(
					[]string{
						fmt.Sprintf("ovn-nbctl --timeout=15 lr-policy-add ovn_cluster_router 100 ip6.src == %s && ip6.dst == ::/0 reroute %s pkt_mark=%v", egressPod.Status.PodIP, nodeGatewayRouterIP, mark),
					},
				)

				_, err = fakeOvn.fakeEgressIPClient.K8sV1().EgressIPs().Update(context.TODO(), updateEIP, metav1.UpdateOptions{})
				Expect(err).ToNot(HaveOccurred())
				Eventually(fakeOvn.fakeExec.CalledMatchesExpected).Should(BeTrue(), fakeOvn.fakeExec.ErrorDesc)
				Eventually(getEgressIPStatusLenSafely(eIP.Name)).Should(Equal(1))

				statuses = getEgressIPStatusSafely(eIP.Name)
				Expect(statuses[0].Node).To(Equal(node2.name))
				Expect(statuses[0].EgressIP).To(Equal(updatedEgressIP.String()))
				return nil
			}

			err := app.Run([]string{app.Name})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
