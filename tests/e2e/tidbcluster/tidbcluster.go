// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package tidbcluster

import (
	"context"
	errors1 "errors"
	"fmt"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/pingcap/advanced-statefulset/client/apis/apps/v1/helper"
	asclientset "github.com/pingcap/advanced-statefulset/client/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/client/clientset/versioned"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/features"
	"github.com/pingcap/tidb-operator/pkg/label"
	"github.com/pingcap/tidb-operator/pkg/manager/member"
	"github.com/pingcap/tidb-operator/pkg/monitor/monitor"
	"github.com/pingcap/tidb-operator/pkg/scheme"
	"github.com/pingcap/tidb-operator/pkg/util"
	tcconfig "github.com/pingcap/tidb-operator/pkg/util/config"
	"github.com/pingcap/tidb-operator/tests"
	e2econfig "github.com/pingcap/tidb-operator/tests/e2e/config"
	e2eframework "github.com/pingcap/tidb-operator/tests/e2e/framework"
	utilimage "github.com/pingcap/tidb-operator/tests/e2e/util/image"
	utilpod "github.com/pingcap/tidb-operator/tests/e2e/util/pod"
	"github.com/pingcap/tidb-operator/tests/e2e/util/portforward"
	"github.com/pingcap/tidb-operator/tests/e2e/util/proxiedpdclient"
	"github.com/pingcap/tidb-operator/tests/pkg/apimachinery"
	"github.com/pingcap/tidb-operator/tests/pkg/blockwriter"
	"github.com/pingcap/tidb-operator/tests/pkg/fixture"
	"github.com/pingcap/tidb-operator/tests/pkg/mock"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
	restclient "k8s.io/client-go/rest"
	aggregatorclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/log"
	"k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/utils"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = ginkgo.Describe("[tidb-operator] TiDBCluster", func() {
	f := e2eframework.NewDefaultFramework("tidb-cluster")

	var ns string
	var c clientset.Interface
	var cli versioned.Interface
	var asCli asclientset.Interface
	var aggrCli aggregatorclient.Interface
	var apiExtCli apiextensionsclientset.Interface
	var oa tests.OperatorActions
	var cfg *tests.Config
	var config *restclient.Config
	var ocfg *tests.OperatorConfig
	var genericCli client.Client
	var fwCancel context.CancelFunc
	var fw portforward.PortForward
	/**
	 * StatefulSet or AdvancedStatefulSet getter interface.
	 */
	var stsGetter typedappsv1.StatefulSetsGetter
	var crdUtil *tests.CrdTestUtil

	ginkgo.BeforeEach(func() {
		ns = f.Namespace.Name
		c = f.ClientSet
		var err error
		config, err = framework.LoadConfig()
		framework.ExpectNoError(err, "failed to load config")
		cli, err = versioned.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset for Pingcap")
		asCli, err = asclientset.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset for advanced-statefulset")
		genericCli, err = client.New(config, client.Options{Scheme: scheme.Scheme})
		framework.ExpectNoError(err, "failed to create clientset for controller-runtime")
		aggrCli, err = aggregatorclient.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset kube-aggregator")
		apiExtCli, err = apiextensionsclientset.NewForConfig(config)
		framework.ExpectNoError(err, "failed to create clientset apiextensions-apiserver")
		clientRawConfig, err := e2econfig.LoadClientRawConfig()
		framework.ExpectNoError(err, "failed to load raw config for tidb-operator")
		ctx, cancel := context.WithCancel(context.Background())
		fw, err = portforward.NewPortForwarder(ctx, e2econfig.NewSimpleRESTClientGetter(clientRawConfig))
		framework.ExpectNoError(err, "failed to create port forwarder")
		fwCancel = cancel
		cfg = e2econfig.TestConfig
		OperatorFeatures := map[string]bool{"AutoScaling": true}
		cfg.OperatorFeatures = OperatorFeatures
		ocfg = e2econfig.NewDefaultOperatorConfig(cfg)
		if ocfg.Enabled(features.AdvancedStatefulSet) {
			stsGetter = helper.NewHijackClient(c, asCli).AppsV1()
		} else {
			stsGetter = c.AppsV1()
		}
		oa = tests.NewOperatorActions(cli, c, asCli, aggrCli, apiExtCli, tests.DefaultPollInterval, ocfg, e2econfig.TestConfig, nil, fw, f)
		crdUtil = tests.NewCrdTestUtil(cli, c, asCli, stsGetter)
	})

	ginkgo.AfterEach(func() {
		if fwCancel != nil {
			fwCancel()
		}
	})

	ginkgo.Context("Basic: Deploying, Scaling, Update Configuration", func() {
		clusterCfgs := []struct {
			Version string
			Name    string
			Values  map[string]string
		}{
			{
				Version: utilimage.TiDBV3Version,
				Name:    "basic-v3",
			},
			{
				Version: utilimage.TiDBV4Version,
				Name:    "basic-v4",
			},
		}

		for _, clusterCfg := range clusterCfgs {
			localCfg := clusterCfg
			ginkgo.It(fmt.Sprintf("[TiDB Version: %s] %s", localCfg.Version, localCfg.Name), func() {
				tc := fixture.GetTidbCluster(ns, localCfg.Name, localCfg.Version)
				// support reclaim pv when scale in tikv or pd component
				tc.Spec.EnablePVReclaim = pointer.BoolPtr(true)
				// change tikv data directory to a subdirectory of data volume
				tc.Spec.TiKV.DataSubDir = "data"

				_, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Create(tc)
				framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
				err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
				framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
				err = crdUtil.CheckDisasterTolerance(tc)
				framework.ExpectNoError(err, "failed to check disaster tolerance for TidbCluster: %v", tc)

				// scale
				err = controller.GuaranteedUpdate(genericCli, tc, func() error {
					tc.Spec.TiDB.Replicas = 3
					tc.Spec.TiKV.Replicas = 5
					tc.Spec.PD.Replicas = 5
					return nil
				})
				framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
				err = crdUtil.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
				framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
				err = crdUtil.CheckDisasterTolerance(tc)
				framework.ExpectNoError(err, "failed to check disaster tolerance for TidbCluster: %v", tc)

				err = controller.GuaranteedUpdate(genericCli, tc, func() error {
					tc.Spec.TiDB.Replicas = 2
					tc.Spec.TiKV.Replicas = 4
					tc.Spec.PD.Replicas = 3
					return nil
				})
				framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
				err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
				framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
				err = crdUtil.CheckDisasterTolerance(tc)
				framework.ExpectNoError(err, "failed to check disaster tolerance for TidbCluster: %v", tc)

				// configuration change
				err = controller.GuaranteedUpdate(genericCli, tc, func() error {
					tc.Spec.ConfigUpdateStrategy = v1alpha1.ConfigUpdateStrategyRollingUpdate
					tc.Spec.PD.MaxFailoverCount = pointer.Int32Ptr(4)
					tc.Spec.TiKV.MaxFailoverCount = pointer.Int32Ptr(4)
					tc.Spec.TiDB.MaxFailoverCount = pointer.Int32Ptr(4)
					return nil
				})
				framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
				err = crdUtil.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
				framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
			})
		}
	})

	/**
	 * This test case switches back and forth between pod network and host network of a single cluster.
	 * Note that only one cluster can run in host network mode at the same time.
	 */
	ginkgo.It("Switching back and forth between pod network and host network", func() {
		if !ocfg.Enabled(features.AdvancedStatefulSet) {
			serverVersion, err := c.Discovery().ServerVersion()
			framework.ExpectNoError(err, "failed to fetch Kubernetes version")
			sv := utilversion.MustParseSemantic(serverVersion.GitVersion)
			log.Logf("ServerVersion: %v", serverVersion.String())
			if sv.LessThan(utilversion.MustParseSemantic("v1.13.11")) || // < v1.13.11
				(sv.AtLeast(utilversion.MustParseSemantic("v1.14.0")) && sv.LessThan(utilversion.MustParseSemantic("v1.14.7"))) || // >= v1.14.0 but < v1.14.7
				(sv.AtLeast(utilversion.MustParseSemantic("v1.15.0")) && sv.LessThan(utilversion.MustParseSemantic("v1.15.4"))) { // >= v1.15.0 but < v1.15.4
				// https://github.com/pingcap/tidb-operator/issues/1042#issuecomment-547742565
				framework.Skipf("Skipping HostNetwork test. Kubernetes %v has a bug that StatefulSet may apply revision incorrectly, HostNetwork cannot work well in this cluster", serverVersion)
			}
			ginkgo.By(fmt.Sprintf("Testing HostNetwork feature with Kubernetes %v", serverVersion))
		} else {
			ginkgo.By("Testing HostNetwork feature with Advanced StatefulSet")
		}

		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "host-network", "", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "1"
		cluster.Resources["tidb.replicas"] = "1"
		cluster.Resources["tikv.replicas"] = "1"
		oa.DeployTidbClusterOrDie(&cluster)

		ginkgo.By("switch to host network")
		cluster.RunInHost(true)
		oa.UpgradeTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)

		ginkgo.By("switch back to pod network")
		cluster.RunInHost(false)
		oa.UpgradeTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)
	})

	ginkgo.It("Upgrading TiDB Cluster", func() {
		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "cluster", "admin", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "3"

		ginkgo.By("Creating webhook certs and self signing it")
		svcName := "webhook"
		certCtx, err := apimachinery.SetupServerCert(ns, svcName)
		framework.ExpectNoError(err, "failed to setup certs for apimachinery webservice %s", tests.WebhookServiceName)

		ginkgo.By("Starting webhook pod")
		webhookPod, svc := startWebhook(c, cfg.E2EImage, ns, svcName, certCtx.Cert, certCtx.Key)

		ginkgo.By("Register webhook")
		oa.RegisterWebHookAndServiceOrDie(ocfg.WebhookConfigName, ns, svc.Name, certCtx)

		ginkgo.By(fmt.Sprintf("Deploying tidb cluster %s", cluster.ClusterVersion))
		oa.DeployTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)
		oa.CheckDisasterToleranceOrDie(&cluster)

		ginkgo.By(fmt.Sprintf("Upgrading tidb cluster from %s to %s", cluster.ClusterVersion, utilimage.TiDBV3UpgradeVersion))
		ctx, cancel := context.WithCancel(context.Background())
		cluster.UpgradeAll(utilimage.TiDBV3UpgradeVersion)
		oa.UpgradeTidbClusterOrDie(&cluster)
		oa.CheckUpgradeOrDie(ctx, &cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)
		cancel()

		ginkgo.By("Check webhook is still running")
		webhookPod, err = c.CoreV1().Pods(webhookPod.Namespace).Get(webhookPod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get pod %s/%s", webhookPod.Namespace, webhookPod.Name)
		if webhookPod.Status.Phase != v1.PodRunning {
			logs, err := pod.GetPodLogs(c, webhookPod.Namespace, webhookPod.Name, "webhook")
			framework.ExpectNoError(err, "failed to get pod log %s/%s", webhookPod.Namespace, webhookPod.Name)
			log.Logf("webhook logs: %s", logs)
			log.Fail("webhook pod is not running")
		}

		oa.CleanWebHookAndServiceOrDie(ocfg.WebhookConfigName)
	})

	ginkgo.It("Backup and restore TiDB Cluster", func() {
		clusterFrom := newTidbClusterConfig(e2econfig.TestConfig, ns, "from", "admin", utilimage.TiDBV3Version)
		clusterFrom.Resources["pd.replicas"] = "1"
		clusterFrom.Resources["tidb.replicas"] = "1"
		clusterFrom.Resources["tikv.replicas"] = "1"
		clusterTo := newTidbClusterConfig(e2econfig.TestConfig, ns, "to", "admin", utilimage.TiDBV3Version)
		clusterTo.Resources["pd.replicas"] = "1"
		clusterTo.Resources["tidb.replicas"] = "1"
		clusterTo.Resources["tikv.replicas"] = "1"
		oa.DeployTidbClusterOrDie(&clusterFrom)
		oa.DeployTidbClusterOrDie(&clusterTo)
		oa.CheckTidbClusterStatusOrDie(&clusterFrom)
		oa.CheckTidbClusterStatusOrDie(&clusterTo)
		oa.CheckDisasterToleranceOrDie(&clusterFrom)
		oa.CheckDisasterToleranceOrDie(&clusterTo)

		// backup and restore
		ginkgo.By(fmt.Sprintf("Backup %q and restore into %q", clusterFrom.ClusterName, clusterTo.ClusterName))
		oa.BackupRestoreOrDie(&clusterFrom, &clusterTo)
	})

	ginkgo.It("Service: Sync TiDB service", func() {
		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "service-it", "admin", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "1"
		cluster.Resources["tidb.replicas"] = "1"
		cluster.Resources["tikv.replicas"] = "1"
		oa.DeployTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)

		ns := cluster.Namespace
		tcName := cluster.ClusterName

		oldSvc, err := c.CoreV1().Services(ns).Get(controller.TiDBMemberName(tcName), metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get service for TidbCluster: %v", cluster)
		tc, err := cli.PingcapV1alpha1().TidbClusters(ns).Get(tcName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get TidbCluster: %v", cluster)
		if isNil, err := gomega.BeNil().Match(metav1.GetControllerOf(oldSvc)); !isNil {
			log.Failf("Expected TiDB service created by helm chart is orphaned: %v", err)
		}

		ginkgo.By("Adopt orphaned service created by helm")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.TiDB.Service = &v1alpha1.TiDBServiceSpec{}
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			svc, err := c.CoreV1().Services(ns).Get(controller.TiDBMemberName(tcName), metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					return false, err
				}
				log.Logf("error get TiDB service: %v", err)
				return false, nil
			}
			owner := metav1.GetControllerOf(svc)
			if owner == nil {
				log.Logf("tidb service has not been adopted by TidbCluster yet")
				return false, nil
			}
			framework.ExpectEqual(metav1.IsControlledBy(svc, tc), true, "Expected owner is TidbCluster")
			framework.ExpectEqual(svc.Spec.ClusterIP, oldSvc.Spec.ClusterIP, "ClusterIP should be stable across adopting and updating")
			return true, nil
		})
		framework.ExpectNoError(err, "failed to wait for TidbCluster managed svc to be ready: %v", tc)

		ginkgo.By("Sync TiDB service properties")

		ginkgo.By("Updating TiDB service")
		trafficPolicy := corev1.ServiceExternalTrafficPolicyTypeLocal
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.TiDB.Service.Type = corev1.ServiceTypeNodePort
			tc.Spec.TiDB.Service.ExternalTrafficPolicy = &trafficPolicy
			tc.Spec.TiDB.Service.Annotations = map[string]string{
				"test": "test",
			}
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		ginkgo.By("Waiting for the TiDB service to be synced")
		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			svc, err := c.CoreV1().Services(ns).Get(controller.TiDBMemberName(tcName), metav1.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					return false, err
				}
				log.Logf("error get TiDB service: %v", err)
				return false, nil
			}
			if isEqual, err := gomega.Equal(corev1.ServiceTypeNodePort).Match(svc.Spec.Type); !isEqual {
				log.Logf("tidb service type is not %s, %v", corev1.ServiceTypeNodePort, err)
				return false, nil
			}
			if isEqual, err := gomega.Equal(trafficPolicy).Match(svc.Spec.ExternalTrafficPolicy); !isEqual {
				log.Logf("tidb service traffic policy is not %s, %v", svc.Spec.ExternalTrafficPolicy, err)
				return false, nil
			}
			if haveKV, err := gomega.HaveKeyWithValue("test", "test").Match(svc.Annotations); !haveKV {
				log.Logf("tidb service has no annotation test=test, %v", err)
				return false, nil
			}

			return true, nil
		})

		framework.ExpectNoError(err, "failed to wait for TidbCluster managed svc to be ready: %v", tc)
	})

	updateStrategy := v1alpha1.ConfigUpdateStrategyInPlace
	// Basic IT for managed in TidbCluster CR
	// TODO: deploy pump through CR in backup and restore IT
	ginkgo.It("Pump: Test managing Pump in TidbCluster CRD", func() {
		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "pump-it", "admin", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "1"
		cluster.Resources["tikv.replicas"] = "1"
		cluster.Resources["tidb.replicas"] = "1"
		oa.DeployTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)

		ginkgo.By("Test adopting pump statefulset created by helm could avoid rolling-update.")
		err := oa.DeployAndCheckPump(&cluster)
		framework.ExpectNoError(err, "failed to deploy pump for TidbCluster: %v", cluster)

		tc, err := cli.PingcapV1alpha1().TidbClusters(cluster.Namespace).Get(cluster.ClusterName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get TidbCluster: %v", cluster)

		// If using advanced statefulset, we must upgrade all Kubernetes statefulsets to advanced statefulsets first.
		if ocfg.Enabled(features.AdvancedStatefulSet) {
			stsList, err := c.AppsV1().StatefulSets(ns).List(metav1.ListOptions{})
			framework.ExpectNoError(err, "failed to list statefulsets in ns %s", ns)
			for _, sts := range stsList.Items {
				_, err = helper.Upgrade(c, asCli, &sts)
				framework.ExpectNoError(err, "failed to upgrade statefulset %s/%s", sts.Namespace, sts.Name)
			}
		}

		oldPumpSet, err := stsGetter.StatefulSets(tc.Namespace).Get(controller.PumpMemberName(tc.Name), metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get statefulset for pump: %v", tc)

		oldRev := oldPumpSet.Status.CurrentRevision
		framework.ExpectEqual(oldPumpSet.Status.UpdateRevision, oldRev, "Expected pump is not upgrading")

		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			pullPolicy := corev1.PullIfNotPresent
			tc.Spec.Pump = &v1alpha1.PumpSpec{
				BaseImage: "pingcap/tidb-binlog",
				ComponentSpec: v1alpha1.ComponentSpec{
					Version:         &cluster.ClusterVersion,
					ImagePullPolicy: &pullPolicy,
					Affinity: &corev1.Affinity{
						PodAntiAffinity: &corev1.PodAntiAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: corev1.PodAffinityTerm{
										Namespaces:  []string{cluster.Namespace},
										TopologyKey: "rack",
									},
									Weight: 50,
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Effect:   corev1.TaintEffectNoSchedule,
							Key:      "node-role",
							Operator: corev1.TolerationOpEqual,
							Value:    "tidb",
						},
					},
					SchedulerName:        pointer.StringPtr("default-scheduler"),
					ConfigUpdateStrategy: &updateStrategy,
				},
				Replicas:         1,
				StorageClassName: pointer.StringPtr("local-storage"),
				ResourceRequirements: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
				Config: tcconfig.New(map[string]interface{}{
					"addr":               "0.0.0.0:8250",
					"gc":                 7,
					"data-dir":           "/data",
					"heartbeat-interval": 2,
				}),
			}
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			pumpSet, err := stsGetter.StatefulSets(tc.Namespace).Get(controller.PumpMemberName(tc.Name), metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return false, err
			}
			if err != nil {
				log.Logf("error get pump statefulset: %v", err)
				return false, nil
			}
			if !metav1.IsControlledBy(pumpSet, tc) {
				log.Logf("expect pump staetfulset adopted by tidbcluster, still waiting...")
				return false, nil
			}
			// The desired state encoded in CRD should be exactly same with the one created by helm chart
			framework.ExpectEqual(pumpSet.Status.CurrentRevision, oldRev, "Expected no rolling-update when adopting pump statefulset")
			framework.ExpectEqual(pumpSet.Status.UpdateRevision, oldRev, "Expected no rolling-update when adopting pump statefulset")

			usingName := member.FindConfigMapVolume(&pumpSet.Spec.Template.Spec, func(name string) bool {
				return strings.HasPrefix(name, controller.PumpMemberName(tc.Name))
			})
			if usingName == "" {
				log.Fail("cannot find configmap that used by pump statefulset")
			}
			pumpConfigMap, err := c.CoreV1().ConfigMaps(tc.Namespace).Get(usingName, metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return false, err
			}
			if err != nil {
				log.Logf("error get pump configmap: %v", err)
				return false, nil
			}
			if !metav1.IsControlledBy(pumpConfigMap, tc) {
				log.Logf("expect pump configmap adopted by tidbcluster, still waiting...")
				return false, nil
			}

			pumpPeerSvc, err := c.CoreV1().Services(tc.Namespace).Get(controller.PumpPeerMemberName(tc.Name), metav1.GetOptions{})
			if errors.IsNotFound(err) {
				return false, err
			}
			if err != nil {
				log.Logf("error get pump peer service: %v", err)
				return false, nil
			}
			if !metav1.IsControlledBy(pumpPeerSvc, tc) {
				log.Logf("expect pump peer service adopted by tidbcluster, still waiting...")
				return false, nil
			}
			return true, nil
		})

		framework.ExpectNoError(err, "failed to wait for pump synced for TidbCluster: %v", tc)
		// TODO: Add pump configmap rolling-update case
	})

	ginkgo.It("API: Migrate from helm to CRD", func() {
		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "helm-migration", "admin", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "1"
		cluster.Resources["tikv.replicas"] = "1"
		cluster.Resources["tidb.replicas"] = "1"
		oa.DeployTidbClusterOrDie(&cluster)
		oa.CheckTidbClusterStatusOrDie(&cluster)

		tc, err := cli.PingcapV1alpha1().TidbClusters(cluster.Namespace).Get(cluster.ClusterName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get TidbCluster: %v", cluster)

		ginkgo.By("Discovery service should be reconciled by tidb-operator")
		discoveryName := controller.DiscoveryMemberName(tc.Name)
		discoveryDep, err := c.AppsV1().Deployments(tc.Namespace).Get(discoveryName, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get discovery deployment for TidbCluster: %v", tc)
		WaitObjectToBeControlledByOrDie(genericCli, discoveryDep, tc, 5*time.Minute)

		err = utils.WaitForDeploymentComplete(c, discoveryDep, log.Logf, 10*time.Second, 5*time.Minute)
		framework.ExpectNoError(err, "waiting for discovery deployment timeout, should be healthy after managed by tidb-operator: %v", discoveryDep)

		err = genericCli.Delete(context.TODO(), discoveryDep)
		framework.ExpectNoError(err, "failed to delete discovery deployment: %v", discoveryDep)

		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			_, err := c.AppsV1().Deployments(tc.Namespace).Get(discoveryName, metav1.GetOptions{})
			if err != nil {
				log.Logf("wait discovery deployment get created again: %v", err)
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "Discovery Deployment should be recovered by tidb-operator after deletion")

		ginkgo.By("Managing TiDB configmap in TidbCluster CRD in-place should not trigger rolling-udpate")
		// TODO: modify other cases to manage TiDB configmap in CRD by default
		setNameToRevision := map[string]string{
			controller.PDMemberName(tc.Name):   "",
			controller.TiKVMemberName(tc.Name): "",
			controller.TiDBMemberName(tc.Name): "",
		}

		for setName := range setNameToRevision {
			oldSet, err := stsGetter.StatefulSets(tc.Namespace).Get(setName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Expected get statefulset %s", setName)

			oldRev := oldSet.Status.CurrentRevision
			framework.ExpectEqual(oldSet.Status.UpdateRevision, oldRev, "Expected statefulset %s is not upgrading", setName)

			setNameToRevision[setName] = oldRev
		}

		tc, err = cli.PingcapV1alpha1().TidbClusters(cluster.Namespace).Get(cluster.ClusterName, metav1.GetOptions{})
		framework.ExpectNoError(err, "Expected get tidbcluster")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.TiDB.Config = v1alpha1.NewTiDBConfig()
			tc.Spec.TiDB.ConfigUpdateStrategy = &updateStrategy
			tc.Spec.TiKV.Config = v1alpha1.NewTiKVConfig()
			tc.Spec.TiKV.ConfigUpdateStrategy = &updateStrategy
			tc.Spec.PD.Config = v1alpha1.NewPDConfig()
			tc.Spec.PD.ConfigUpdateStrategy = &updateStrategy
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		// check for 2 minutes to ensure the tidb statefulset do not get rolling-update
		err = wait.PollImmediate(10*time.Second, 2*time.Minute, func() (bool, error) {
			tc, err := cli.PingcapV1alpha1().TidbClusters(cluster.Namespace).Get(cluster.ClusterName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Expected get tidbcluster")
			framework.ExpectEqual(tc.Status.PD.Phase, v1alpha1.NormalPhase, "PD should not be updated")
			framework.ExpectEqual(tc.Status.TiKV.Phase, v1alpha1.NormalPhase, "TiKV should not be updated")
			framework.ExpectEqual(tc.Status.TiDB.Phase, v1alpha1.NormalPhase, "TiDB should not be updated")

			for setName, oldRev := range setNameToRevision {
				newSet, err := stsGetter.StatefulSets(tc.Namespace).Get(setName, metav1.GetOptions{})
				framework.ExpectNoError(err, "Expected get tidb statefulset")
				framework.ExpectEqual(newSet.Status.CurrentRevision, oldRev, "Expected no rolling-update of %s when manage config in-place", setName)
				framework.ExpectEqual(newSet.Status.UpdateRevision, oldRev, "Expected no rolling-update of %s when manage config in-place", setName)
			}
			return false, nil
		})

		if err != wait.ErrWaitTimeout {
			log.Failf("Unexpected error when checking tidb statefulset will not get rolling-update: %v", err)
		}

		err = wait.PollImmediate(5*time.Second, 3*time.Minute, func() (bool, error) {
			for setName := range setNameToRevision {
				newSet, err := stsGetter.StatefulSets(tc.Namespace).Get(setName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				usingName := member.FindConfigMapVolume(&newSet.Spec.Template.Spec, func(name string) bool {
					return strings.HasPrefix(name, setName)
				})
				if usingName == "" {
					log.Failf("cannot find configmap that used by %s", setName)
				}
				usingCm, err := c.CoreV1().ConfigMaps(tc.Namespace).Get(usingName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if !metav1.IsControlledBy(usingCm, tc) {
					log.Logf("expect configmap of %s adopted by tidbcluster, still waiting...", setName)
					return false, nil
				}
			}
			return true, nil
		})

		framework.ExpectNoError(err, "timeout waiting for statefulsets to be controlled by TidbCluster")
	})

	ginkgo.It("TidbMonitor: Deploying and checking monitor", func() {
		tc := fixture.GetTidbCluster(ns, "monitor-test", utilimage.TiDBV4UpgradeVersion)
		tc.Spec.PD.Replicas = 1
		tc.Spec.TiKV.Replicas = 1
		tc.Spec.TiDB.Replicas = 1
		tc, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Create(tc)
		framework.ExpectNoError(err, "Expected create tidbcluster")
		err = oa.WaitForTidbClusterReady(tc, 10*time.Minute, 5*time.Second)
		framework.ExpectNoError(err, "Expected get tidbcluster")

		tm := fixture.NewTidbMonitor("monitor-test", ns, tc, true, true, false)
		deletePVP := corev1.PersistentVolumeReclaimDelete
		tm.Spec.PVReclaimPolicy = &deletePVP
		_, err = cli.PingcapV1alpha1().TidbMonitors(ns).Create(tm)
		framework.ExpectNoError(err, "Expected tidbmonitor deployed success")
		err = tests.CheckTidbMonitor(tm, cli, c, fw)
		framework.ExpectNoError(err, "Expected tidbmonitor checked success")
		pvc, err := c.CoreV1().PersistentVolumeClaims(ns).Get(monitor.GetMonitorFirstPVCName(tm.Name), metav1.GetOptions{})
		framework.ExpectNoError(err, "Expected fetch tidbmonitor pvc success")

		pvName := pvc.Spec.VolumeName
		pv, err := c.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{})
		framework.ExpectNoError(err, "Expected fetch tidbmonitor pv success")

		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			value, existed := pv.Labels[label.ComponentLabelKey]
			if !existed || value != label.TiDBMonitorVal {
				return false, nil
			}
			value, existed = pv.Labels[label.InstanceLabelKey]
			if !existed || value != "monitor-test" {
				return false, nil
			}

			value, existed = pv.Labels[label.NameLabelKey]
			if !existed || value != "tidb-cluster" {
				return false, nil
			}
			value, existed = pv.Labels[label.ManagedByLabelKey]
			if !existed || value != label.TiDBOperator {
				return false, nil
			}
			if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				return false, fmt.Errorf("pv[%s] 's policy is not Delete", pv.Name)
			}
			return true, nil
		})
		framework.ExpectNoError(err, "monitor pv label error")

		// update TidbMonitor and check whether portName is updated and the nodePort is unchanged
		tm, err = cli.PingcapV1alpha1().TidbMonitors(ns).Get(tm.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "fetch latest tidbmonitor error")
		tm.Spec.Prometheus.Service.Type = corev1.ServiceTypeNodePort
		retainPVP := corev1.PersistentVolumeReclaimRetain
		tm.Spec.PVReclaimPolicy = &retainPVP
		tm, err = cli.PingcapV1alpha1().TidbMonitors(ns).Update(tm)
		framework.ExpectNoError(err, "update tidbmonitor service type error")

		var targetPort int32
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			prometheusSvc, err := c.CoreV1().Services(ns).Get(fmt.Sprintf("%s-prometheus", tm.Name), metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			if len(prometheusSvc.Spec.Ports) != 3 {
				return false, nil
			}
			if prometheusSvc.Spec.Type != corev1.ServiceTypeNodePort {
				return false, nil
			}
			targetPort = prometheusSvc.Spec.Ports[0].NodePort
			return true, nil
		})
		framework.ExpectNoError(err, "first update tidbmonitor service error")

		tm, err = cli.PingcapV1alpha1().TidbMonitors(ns).Get(tm.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "fetch latest tidbmonitor again error")
		newPortName := "any-other-word"
		tm.Spec.Prometheus.Service.PortName = &newPortName
		tm, err = cli.PingcapV1alpha1().TidbMonitors(ns).Update(tm)
		framework.ExpectNoError(err, "update tidbmonitor service portName error")

		pvc, err = c.CoreV1().PersistentVolumeClaims(ns).Get(monitor.GetMonitorFirstPVCName(tm.Name), metav1.GetOptions{})
		framework.ExpectNoError(err, "Expected fetch tidbmonitor pvc success")
		pvName = pvc.Spec.VolumeName
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			prometheusSvc, err := c.CoreV1().Services(ns).Get(fmt.Sprintf("%s-prometheus", tm.Name), metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			if len(prometheusSvc.Spec.Ports) != 3 {
				return false, nil
			}
			if prometheusSvc.Spec.Type != corev1.ServiceTypeNodePort {
				framework.Logf("prometheus service type haven't be changed")
				return false, nil
			}
			if prometheusSvc.Spec.Ports[0].Name != "any-other-word" {
				framework.Logf("prometheus port name haven't be changed")
				return false, nil
			}
			if prometheusSvc.Spec.Ports[0].NodePort != targetPort {
				return false, nil
			}
			pv, err = c.CoreV1().PersistentVolumes().Get(pvName, metav1.GetOptions{})
			if err != nil {
				return false, nil
			}
			if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
				framework.Logf("prometheus PersistentVolumeReclaimPolicy haven't be changed")
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "second update tidbmonitor service error")

		err = wait.Poll(5*time.Second, 3*time.Minute, func() (done bool, err error) {
			tc, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Get(tc.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			tm, err = cli.PingcapV1alpha1().TidbMonitors(ns).Get(tm.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if *tc.Spec.PVReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
				framework.Logf("tidbcluster PVReclaimPolicy changed into %v", *tc.Spec.PVReclaimPolicy)
				return true, nil
			}
			if *tm.Spec.PVReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
				framework.Logf("tidbmonitor PVReclaimPolicy changed into %v", *tm.Spec.PVReclaimPolicy)
				return true, nil
			}
			return false, nil
		})
		framework.ExpectEqual(err, wait.ErrWaitTimeout, "verify tidbmonitor and tidbcluster PVReclaimPolicy won't affect each other")

		err = cli.PingcapV1alpha1().TidbMonitors(tm.Namespace).Delete(tm.Name, &metav1.DeleteOptions{})
		framework.ExpectNoError(err, "delete tidbmonitor failed")
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			tc, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Get(tc.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if tc.Status.Monitor != nil {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(err, "tc monitorRef status failed to clean after monitor deleted")
	})

	ginkgo.It("TiDB cluster can be paused and unpaused", func() {
		tcName := "paused"
		tc := fixture.GetTidbCluster(ns, tcName, utilimage.TiDBV3Version)
		tc.Spec.PD.Replicas = 1
		tc.Spec.TiKV.Replicas = 1
		tc.Spec.TiDB.Replicas = 1
		err := genericCli.Create(context.TODO(), tc)
		framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
		err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

		podListBeforePaused, err := c.CoreV1().Pods(ns).List(metav1.ListOptions{})
		framework.ExpectNoError(err, "failed to list pods in ns %s", ns)

		ginkgo.By("Pause the tidb cluster")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.Paused = true
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
		ginkgo.By("Make a change")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.Version = utilimage.TiDBV3UpgradeVersion
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		ginkgo.By("Check pods are not changed when the tidb cluster is paused")
		err = utilpod.WaitForPodsAreChanged(c, podListBeforePaused.Items, time.Minute*5)
		framework.ExpectEqual(err, wait.ErrWaitTimeout, "Pods are changed when the tidb cluster is paused")

		ginkgo.By("Unpause the tidb cluster")
		err = controller.GuaranteedUpdate(genericCli, tc, func() error {
			tc.Spec.Paused = false
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

		ginkgo.By("Check the tidb cluster will be upgraded now")
		listOptions := metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(label.New().Instance(tcName).Component(label.TiKVLabelVal).Labels()).String(),
		}
		err = wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
			podList, err := c.CoreV1().Pods(ns).List(listOptions)
			if err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
			for _, pod := range podList.Items {
				for _, c := range pod.Spec.Containers {
					if c.Name == v1alpha1.TiKVMemberType.String() {
						if c.Image == tc.TiKVImage() {
							return true, nil
						}
					}
				}
			}
			return false, nil
		})
		framework.ExpectNoError(err, "wait for tikv upgraded timeout: %v", tc)
	})

	ginkgo.It("[Feature: AutoFailover] clear TiDB failureMembers when scale TiDB to zero", func() {
		cluster := newTidbClusterConfig(e2econfig.TestConfig, ns, "tidb-scale", "admin", utilimage.TiDBV3Version)
		cluster.Resources["pd.replicas"] = "3"
		cluster.Resources["tikv.replicas"] = "1"
		cluster.Resources["tidb.replicas"] = "1"

		cluster.TiDBPreStartScript = strconv.Quote("exit 1")
		oa.DeployTidbClusterOrDie(&cluster)

		log.Logf("checking tidb cluster [%s/%s] failed member", cluster.Namespace, cluster.ClusterName)
		ns := cluster.Namespace
		tcName := cluster.ClusterName
		err := wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
			var tc *v1alpha1.TidbCluster
			var err error
			if tc, err = cli.PingcapV1alpha1().TidbClusters(ns).Get(tcName, metav1.GetOptions{}); err != nil {
				log.Logf("failed to get tidbcluster: %s/%s, %v", ns, tcName, err)
				return false, nil
			}
			if len(tc.Status.TiDB.FailureMembers) == 0 {
				log.Logf("the number of failed member is zero")
				return false, nil
			}
			log.Logf("the number of failed member is not zero (current: %d)", len(tc.Status.TiDB.FailureMembers))
			return true, nil
		})
		framework.ExpectNoError(err, "tidb failover not work")

		cluster.ScaleTiDB(0)
		oa.ScaleTidbClusterOrDie(&cluster)

		log.Logf("checking tidb cluster [%s/%s] scale to zero", cluster.Namespace, cluster.ClusterName)
		err = wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
			var tc *v1alpha1.TidbCluster
			var err error
			if tc, err = cli.PingcapV1alpha1().TidbClusters(ns).Get(tcName, metav1.GetOptions{}); err != nil {
				log.Logf("failed to get tidbcluster: %s/%s, %v", ns, tcName, err)
				return false, nil
			}
			if tc.Status.TiDB.StatefulSet.Replicas != 0 {
				log.Logf("failed to scale tidb member to zero (current: %d)", tc.Status.TiDB.StatefulSet.Replicas)
				return false, nil
			}
			if len(tc.Status.TiDB.FailureMembers) != 0 {
				log.Logf("failed to clear fail member (current: %d)", len(tc.Status.TiDB.FailureMembers))
				return false, nil
			}
			log.Logf("scale tidb member to zero successfully")
			return true, nil
		})
		framework.ExpectNoError(err, "not clear TiDB failureMembers when scale TiDB to zero")
	})

	ginkgo.Context("[Feature: AutoScaling]", func() {
		ginkgo.It("auto-scaling TiKV", func() {
			clusterName := "auto-scaling-tikv"
			tc := fixture.GetTidbCluster(ns, clusterName, utilimage.TiDBNightlyVersion)
			tc.Spec.PD.Replicas = 1
			tc.Spec.TiKV.Replicas = 3
			tc.Spec.PD.Config.Set("pd-server.metric-storage", "http://monitor-prometheus:9090")

			_, err := cli.PingcapV1alpha1().TidbClusters(ns).Create(tc)
			framework.ExpectNoError(err, "Create TidbCluster error")
			err = oa.WaitForTidbClusterReady(tc, 10*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "Check TidbCluster error")
			monitor := fixture.NewTidbMonitor("monitor", ns, tc, false, false, false)

			// Replace Prometheus into Mock Prometheus
			a := e2econfig.TestConfig.E2EImage
			colonIdx := strings.LastIndexByte(a, ':')
			image := a[:colonIdx]
			tag := a[colonIdx+1:]
			monitor.Spec.Prometheus.BaseImage = image
			monitor.Spec.Prometheus.Version = tag

			_, err = cli.PingcapV1alpha1().TidbMonitors(ns).Create(monitor)
			framework.ExpectNoError(err, "Create TidbMonitor error")
			err = tests.CheckTidbMonitor(monitor, cli, c, fw)
			framework.ExpectNoError(err, "Check TidbMonitor error")
			tac := fixture.GetTidbClusterAutoScaler("auto-scaler", ns, tc, monitor)

			// TODO: This duration is now hard-coded in PD
			// It may become configurable in the future
			duration := "60s"
			setCPUUsageAndQuota := func(usage, quota, memberType string, insts []string) {
				mp := &mock.MonitorParams{
					Name:                tc.Name,
					KubernetesNamespace: tc.Namespace,
					MemberType:          memberType,
					Duration:            duration,
					Value:               usage,
					QueryType:           "cpu_usage",
					InstancesPod:        insts,
				}
				err = mock.SetPrometheusResponse(monitor.Name, monitor.Namespace, mp, fw)
				framework.ExpectNoError(err, "set %s cpu usage mock metrics error", memberType)

				mp = &mock.MonitorParams{
					Name:                tc.Name,
					KubernetesNamespace: tc.Namespace,
					MemberType:          memberType,
					Duration:            duration,
					Value:               quota,
					QueryType:           "cpu_quota",
					InstancesPod:        insts,
				}
				err = mock.SetPrometheusResponse(monitor.Name, monitor.Namespace, mp, fw)
				framework.ExpectNoError(err, "set %s cpu quota mock metrics error", memberType)
			}

			tac.Spec.TiKV = &v1alpha1.TikvAutoScalerSpec{}
			tac.Spec.TiKV.ScaleInIntervalSeconds = pointer.Int32Ptr(1)
			tac.Spec.TiKV.ScaleOutIntervalSeconds = pointer.Int32Ptr(1)
			tac.Spec.TiKV.Resources = map[string]v1alpha1.AutoResource{
				"storage": {
					CPU:     resource.MustParse("1024m"),
					Memory:  resource.MustParse("2Gi"),
					Storage: resource.MustParse("10Gi"),
					Count:   pointer.Int32Ptr(2),
				},
			}
			tac.Spec.TiKV.Rules = map[v1.ResourceName]v1alpha1.AutoRule{
				v1.ResourceCPU: {
					MaxThreshold: 0.5,
					MinThreshold: func() *float64 {
						v := 0.2
						return &v
					}(),
					ResourceTypes: []string{"storage"},
				},
			}

			_, err = cli.PingcapV1alpha1().TidbClusterAutoScalers(ns).Create(tac)
			framework.ExpectNoError(err, "Create TidbClusterAutoScaler error")

			pdClient, cancel, err := proxiedpdclient.NewProxiedPDClient(c, fw, ns, clusterName, false)
			framework.ExpectNoError(err, "create pdapi error")
			defer cancel()

			var autoTc v1alpha1.TidbCluster
			autoTcListOption := metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set{
					label.AutoInstanceLabelKey: tac.Name,
					label.BaseTCLabelKey:       tc.Name,
				}).String(),
			}

			// TiKV autoscaling
			baseTiKVs := make([]string, 0, tc.Spec.TiKV.Replicas)
			for i := int32(0); i < tc.Spec.TiKV.Replicas; i++ {
				baseTiKVs = append(baseTiKVs, util.GetPodName(tc, v1alpha1.TiKVMemberType, i))
			}
			var autoTiKV string
			// Case 1: No autoscaling cluster and CPU usage over max threshold
			setCPUUsageAndQuota("35.0", "1.0", v1alpha1.TiKVMemberType.String(), baseTiKVs)
			// A new cluster should be created and all TiKV stores are up
			err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
				tcList, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).List(autoTcListOption)

				if err != nil {
					return false, err
				}

				if len(tcList.Items) < 1 {
					framework.Logf("autoscaling tikv cluster is not created")
					return false, nil
				}

				autoTc = tcList.Items[0]

				if autoTc.Spec.TiKV == nil {
					return false, errors1.New("the created cluster has no tikv spec")
				}

				autoTiKV = util.GetPodName(&autoTc, v1alpha1.TiKVMemberType, 0)
				setCPUUsageAndQuota("20.0", "1.0", v1alpha1.TiKVMemberType.String(), append(baseTiKVs, autoTiKV))

				if len(autoTc.Status.TiKV.Stores) < int(autoTc.Spec.TiKV.Replicas) {
					return false, nil
				}

				for _, store := range autoTc.Status.TiKV.Stores {
					if store.State != v1alpha1.TiKVStateUp {
						framework.Logf("autoscaling tikv cluster not ready, store %s is not %s", store.PodName, v1alpha1.TiKVStateUp)
						return false, nil
					}
				}

				storeID := ""
				for k, v := range autoTc.Status.TiKV.Stores {
					if v.PodName == util.GetPodName(&autoTc, v1alpha1.TiKVMemberType, int32(0)) {
						storeID = k
						break
					}
				}
				if storeID == "" {
					return false, nil
				}
				sid, err := strconv.ParseUint(storeID, 10, 64)
				if err != nil {
					return false, err
				}
				info, err := pdClient.GetStore(sid)
				if err != nil {
					return false, err
				}

				// Check labels
				expectedLabels := map[string]string{
					"specialUse":    "hotRegion",
					"resource-type": "storage",
					"group":         "pd-auto-scaling-tikv", // This label is subject to change
				}
				for _, label := range info.Store.Labels {
					if value, ok := expectedLabels[label.Key]; ok && value != label.Value {
						return false, fmt.Errorf("expected label %s of tc[%s/%s]'s store %d to have value %s, got %s", label.Key, autoTc.Namespace, autoTc.Name, sid, expectedLabels[label.Key], label.Value)
					}
				}

				return true, nil
			})
			framework.ExpectNoError(err, "check create autoscaling tikv cluster error")
			framework.Logf("success to check create autoscaling tikv cluster")

			// Case 2: Has an autoscaling cluster and CPU usage between max threshold and min threshold
			setCPUUsageAndQuota("20.0", "1.0", v1alpha1.TiKVMemberType.String(), append(baseTiKVs, autoTiKV))
			// The TiKV replicas should remain unchanged
			err = wait.Poll(30*time.Second, 3*time.Minute, func() (done bool, err error) {
				tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

				if err != nil {
					return false, err
				}

				autoTc = *tcPtr

				if autoTc.Spec.TiKV.Replicas != 1 {
					framework.Logf("expected tc[%s/%s]'s tikv replicas to stay at 1, now %d", autoTc.Namespace, autoTc.Name, autoTc.Spec.TiKV.Replicas)
					return true, nil
				}

				framework.Logf("confirm autoscaling tikv is not scaled when normal utilization")
				return false, nil
			})
			framework.ExpectEqual(err, wait.ErrWaitTimeout, "expect tikv is not scaled when normal utilization for 5 minutes")

			// Case 3: Has an autoscaling cluster and CPU usage over max threshold
			setCPUUsageAndQuota("35.0", "1.0", v1alpha1.TiKVMemberType.String(), append(baseTiKVs, autoTiKV))
			// The existing autoscaling cluster should be scaled out
			err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
				tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

				if err != nil {
					return false, err
				}

				autoTc = *tcPtr

				if autoTc.Spec.TiKV.Replicas < 2 {
					framework.Logf("autoscaling tikv cluster is not scaled out")
					return false, nil
				}

				if len(autoTc.Status.TiKV.Stores) < int(autoTc.Spec.TiKV.Replicas) {
					return false, nil
				}

				for _, store := range autoTc.Status.TiKV.Stores {
					if store.State != v1alpha1.TiKVStateUp {
						framework.Logf("autoscaling tikv cluster scaled out but store %s is not %s", store.PodName, v1alpha1.TiKVStateUp)
						return false, nil
					}
				}

				return true, nil
			})
			framework.ExpectNoError(err, "check scale out existing autoscaling tikv cluster error")
			framework.Logf("success to check scale out existing autoscaling tikv cluster")

			pods := make([]string, len(baseTiKVs))
			copy(pods, baseTiKVs)
			for i := int32(0); i < autoTc.Spec.TiKV.Replicas; i++ {
				pods = append(pods, util.GetPodName(&autoTc, v1alpha1.TiKVMemberType, i))
			}

			// Case 4: CPU usage below min threshold
			setCPUUsageAndQuota("0.0", "1.0", v1alpha1.TiKVMemberType.String(), pods)
			// The autoscaling cluster should be scaled in
			err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
				tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

				if err != nil {
					if errors.IsNotFound(err) {
						return true, nil
					}
					return false, err
				}

				autoTc = *tcPtr

				if autoTc.Spec.TiKV.Replicas > 1 {
					framework.Logf("autoscaling tikv cluster is not scaled in, replicas=%d", autoTc.Spec.TiKV.Replicas)
					return false, nil
				}

				if autoTc.Spec.TiKV.Replicas <= 1 {
					framework.Logf("autoscaling tikv cluster tc[%s/%s] is scaled in", autoTc.Namespace, autoTc.Name)
					return true, nil
				}

				return false, nil
			})

			framework.ExpectNoError(err, "failed to check scale in autoscaling tikv cluster")
			framework.Logf("success to check scale in autoscaling tikv cluster")

			// Case 5: CPU usage below min threshold for a long time
			// The autoscaling cluster should be deleted
			err = wait.Poll(5*time.Second, 10*time.Minute, func() (done bool, err error) {
				tcList, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).List(autoTcListOption)

				if err != nil {
					return false, err
				}

				if len(tcList.Items) > 0 {
					framework.Logf("autoscaling tikv cluster is not deleted")
					return false, nil
				}

				framework.Logf("autoscaling tikv cluster deleted")
				return true, nil
			})
			framework.ExpectNoError(err, "check delete autoscaling tikv cluster error")
			framework.Logf("success to check delete autoscaling tikv cluster")
		})
	})

	ginkgo.It("auto-scaling TiDB", func() {
		clusterName := "auto-scaling-tidb"
		tc := fixture.GetTidbCluster(ns, clusterName, utilimage.TiDBNightlyVersion)
		tc.Spec.PD.Replicas = 1
		tc.Spec.TiDB.Replicas = 2
		tc.Spec.TiKV.Replicas = 3
		tc.Spec.PD.Config.Set("pd-server.metric-storage", "http://monitor-prometheus:9090")

		_, err := cli.PingcapV1alpha1().TidbClusters(ns).Create(tc)
		framework.ExpectNoError(err, "Create TidbCluster error")
		err = oa.WaitForTidbClusterReady(tc, 10*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Check TidbCluster error")
		monitor := fixture.NewTidbMonitor("monitor", ns, tc, false, false, false)

		// Replace Prometheus into Mock Prometheus
		a := e2econfig.TestConfig.E2EImage
		colonIdx := strings.LastIndexByte(a, ':')
		image := a[:colonIdx]
		tag := a[colonIdx+1:]
		monitor.Spec.Prometheus.BaseImage = image
		monitor.Spec.Prometheus.Version = tag

		_, err = cli.PingcapV1alpha1().TidbMonitors(ns).Create(monitor)
		framework.ExpectNoError(err, "Create TidbMonitor error")
		err = tests.CheckTidbMonitor(monitor, cli, c, fw)
		framework.ExpectNoError(err, "Check TidbMonitor error")
		tac := fixture.GetTidbClusterAutoScaler("auto-scaler", ns, tc, monitor)

		// TODO: This duration is now hard-coded in PD
		// It may become configurable in the future
		duration := "60s"
		setCPUUsageAndQuota := func(usage, quota, memberType string, insts []string) {
			mp := &mock.MonitorParams{
				Name:                tc.Name,
				KubernetesNamespace: tc.Namespace,
				MemberType:          memberType,
				Duration:            duration,
				Value:               usage,
				QueryType:           "cpu_usage",
				InstancesPod:        insts,
			}
			err = mock.SetPrometheusResponse(monitor.Name, monitor.Namespace, mp, fw)
			framework.ExpectNoError(err, "set %s cpu usage mock metrics error", memberType)

			mp = &mock.MonitorParams{
				Name:                tc.Name,
				KubernetesNamespace: tc.Namespace,
				MemberType:          memberType,
				Duration:            duration,
				Value:               quota,
				QueryType:           "cpu_quota",
				InstancesPod:        insts,
			}
			err = mock.SetPrometheusResponse(monitor.Name, monitor.Namespace, mp, fw)
			framework.ExpectNoError(err, "set %s cpu quota mock metrics error", memberType)
		}

		tac.Spec.TiDB = &v1alpha1.TidbAutoScalerSpec{}
		tac.Spec.TiDB.ScaleInIntervalSeconds = pointer.Int32Ptr(1)
		tac.Spec.TiDB.ScaleOutIntervalSeconds = pointer.Int32Ptr(1)
		tac.Spec.TiDB.Resources = map[string]v1alpha1.AutoResource{
			"compute": {
				CPU:    resource.MustParse("1024m"),
				Memory: resource.MustParse("2Gi"),
				Count:  pointer.Int32Ptr(2),
			},
		}
		tac.Spec.TiDB.Rules = map[v1.ResourceName]v1alpha1.AutoRule{
			v1.ResourceCPU: {
				MaxThreshold: 0.5,
				MinThreshold: func() *float64 {
					v := 0.2
					return &v
				}(),
				ResourceTypes: []string{"compute"},
			},
		}
		_, err = cli.PingcapV1alpha1().TidbClusterAutoScalers(ns).Create(tac)
		framework.ExpectNoError(err, "Create TidbClusterAutoScaler error")

		var autoTc v1alpha1.TidbCluster
		autoTcListOption := metav1.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{
				label.AutoInstanceLabelKey: tac.Name,
				label.BaseTCLabelKey:       tc.Name,
			}).String(),
		}

		// TiDB Autoscaling
		baseTiDBs := make([]string, 0, tc.Spec.TiDB.Replicas)
		for i := int32(0); i < tc.Spec.TiDB.Replicas; i++ {
			baseTiDBs = append(baseTiDBs, util.GetPodName(tc, v1alpha1.TiDBMemberType, i))
		}
		var autoTiDB string

		// Case 1: No autoscaling cluster and CPU usage over max threshold
		setCPUUsageAndQuota("35.0", "1.0", v1alpha1.TiDBMemberType.String(), baseTiDBs)
		// A new cluster should be created
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			tcList, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).List(autoTcListOption)

			if err != nil {
				return false, err
			}

			if len(tcList.Items) < 1 {
				framework.Logf("autoscaling tidb cluster is not created")
				return false, nil
			}

			autoTc = tcList.Items[0]
			autoTiDB = util.GetPodName(&autoTc, v1alpha1.TiDBMemberType, 0)
			setCPUUsageAndQuota("20.0", "1.0", v1alpha1.TiDBMemberType.String(), append(baseTiDBs, autoTiDB))
			return true, nil
		})
		framework.ExpectNoError(err, "check create autoscaling tidb cluster error")
		framework.Logf("success to check create autoscaling tidb cluster")

		autoTiDB = util.GetPodName(&autoTc, v1alpha1.TiDBMemberType, 0)
		// Case 2: Has an autoscaling cluster and CPU usage between max threshold and min threshold
		setCPUUsageAndQuota("20.0", "1.0", v1alpha1.TiDBMemberType.String(), append(baseTiDBs, autoTiDB))
		// The TiDB replicas should remain unchanged
		err = wait.Poll(30*time.Second, 3*time.Minute, func() (done bool, err error) {
			tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

			if err != nil {
				return false, err
			}

			autoTc = *tcPtr

			if autoTc.Spec.TiDB.Replicas != 1 {
				framework.Logf("expected tc[%s/%s]'s tidb replicas to stay at 1, now %d", autoTc.Namespace, autoTc.Name, autoTc.Spec.TiDB.Replicas)
				return true, nil
			}

			framework.Logf("confirm autoscaling tidb is not scaled when normal utilization")
			return false, nil
		})
		framework.ExpectEqual(err, wait.ErrWaitTimeout, "expect tidb is not scaled when normal utilization for 5 minutes")

		// Case 3: Has an autoscaling cluster and CPU usage over max threshold
		setCPUUsageAndQuota("35.0", "1.0", v1alpha1.TiDBMemberType.String(), append(baseTiDBs, autoTiDB))
		// The existing autoscaling cluster should be scaled out
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

			if err != nil {
				return false, err
			}

			autoTc = *tcPtr

			if autoTc.Spec.TiDB.Replicas < 2 {
				framework.Logf("autoscaling tidb cluster is not scaled out")
				return false, nil
			}

			return true, nil
		})
		framework.ExpectNoError(err, "check scale out existing autoscaling tidb cluster error")
		framework.Logf("success to check scale out existing autoscaling tidb cluster")

		pods := make([]string, len(baseTiDBs))
		copy(pods, baseTiDBs)
		for i := int32(0); i < autoTc.Spec.TiDB.Replicas; i++ {
			pods = append(pods, util.GetPodName(&autoTc, v1alpha1.TiDBMemberType, i))
		}

		// Case 4: CPU usage below min threshold
		setCPUUsageAndQuota("0.0", "1.0", v1alpha1.TiDBMemberType.String(), pods)
		// The autoscaling cluster should be scaled in
		err = wait.Poll(5*time.Second, 5*time.Minute, func() (done bool, err error) {
			tcPtr, err := cli.PingcapV1alpha1().TidbClusters(autoTc.Namespace).Get(autoTc.Name, metav1.GetOptions{})

			if err != nil {
				if errors.IsNotFound(err) {
					return true, nil
				}
				return false, err
			}

			autoTc = *tcPtr

			if autoTc.Spec.TiDB.Replicas > 1 {
				framework.Logf("autoscaling tidb cluster is not scaled in, replicas=%d", autoTc.Spec.TiDB.Replicas)
				return false, nil
			}

			if autoTc.Spec.TiDB.Replicas <= 1 {
				framework.Logf("autoscaling tidb cluster tc[%s/%s] is scaled in", autoTc.Namespace, autoTc.Name)
				return true, nil
			}

			return false, nil
		})

		framework.ExpectNoError(err, "failed to check scale in autoscaling tidb cluster")
		framework.Logf("success to check scale in autoscaling tidb cluster")

		// Case 5: CPU usage below min threshold for a long time
		// The autoscaling cluster should be deleted
		err = wait.Poll(5*time.Second, 10*time.Minute, func() (done bool, err error) {
			tcList, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).List(autoTcListOption)

			if err != nil {
				return false, err
			}

			if len(tcList.Items) > 0 {
				framework.Logf("autoscaling tidb cluster is not deleted")
				return false, nil
			}

			framework.Logf("autoscaling tidb cluster deleted")
			return true, nil
		})
		framework.ExpectNoError(err, "check delete autoscaling tidb cluster error")
		framework.Logf("success to check delete autoscaling tidb cluster")

		// Clean autoscaler
		err = cli.PingcapV1alpha1().TidbClusterAutoScalers(tac.Namespace).Delete(tac.Name, &metav1.DeleteOptions{})
		framework.ExpectNoError(err, "failed to delete auto-scaler")
	})

	ginkgo.Context("[Feature: TLS]", func() {
		ginkgo.It("TLS for MySQL Client and TLS between TiDB components", func() {
			tcName := "tls"

			ginkgo.By("Installing tidb issuer")
			err := installTiDBIssuer(ns, tcName)
			framework.ExpectNoError(err, "failed to generate tidb issuer template")

			ginkgo.By("Installing tidb server and client certificate")
			err = installTiDBCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install tidb server and client certificate")

			ginkgo.By("Installing separate tidbInitializer client certificate")
			err = installTiDBInitializerCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install separate tidbInitializer client certificate")

			ginkgo.By("Installing separate dashboard client certificate")
			err = installPDDashboardCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install separate dashboard client certificate")

			ginkgo.By("Installing tidb components certificates")
			err = installTiDBComponentsCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install tidb components certificates")

			ginkgo.By("Creating tidb cluster")
			dashTLSName := fmt.Sprintf("%s-dashboard-tls", tcName)
			tc := fixture.GetTidbCluster(ns, tcName, utilimage.TiDBV4Version)
			tc.Spec.PD.Replicas = 3
			tc.Spec.PD.TLSClientSecretName = &dashTLSName
			tc.Spec.TiKV.Replicas = 3
			tc.Spec.TiDB.Replicas = 2
			tc.Spec.TiDB.TLSClient = &v1alpha1.TiDBTLSClient{Enabled: true}
			tc.Spec.TLSCluster = &v1alpha1.TLSCluster{Enabled: true}
			tc.Spec.Pump = &v1alpha1.PumpSpec{
				Replicas:             1,
				BaseImage:            "pingcap/tidb-binlog",
				ResourceRequirements: fixture.WithStorage(fixture.BurstbleSmall, "1Gi"),
				Config: tcconfig.New(map[string]interface{}{
					"addr": "0.0.0.0:8250",
				}),
			}
			err = genericCli.Create(context.TODO(), tc)
			framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
			err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

			ginkgo.By("Ensure Dashboard use custom secret")
			foundSecretName := false
			pdSts, err := stsGetter.StatefulSets(ns).Get(controller.PDMemberName(tcName), metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get statefulsets for pd: %v", tc)
			for _, vol := range pdSts.Spec.Template.Spec.Volumes {
				if vol.Name == "tidb-client-tls" {
					foundSecretName = true
					framework.ExpectEqual(vol.Secret.SecretName, dashTLSName)
				}
			}
			framework.ExpectEqual(foundSecretName, true)

			ginkgo.By("Creating tidb initializer")
			passwd := "admin"
			initName := fmt.Sprintf("%s-initializer", tcName)
			initPassWDName := fmt.Sprintf("%s-initializer-passwd", tcName)
			initTLSName := fmt.Sprintf("%s-initializer-tls", tcName)
			initSecret := fixture.GetInitializerSecret(tc, initPassWDName, passwd)
			_, err = c.CoreV1().Secrets(ns).Create(initSecret)
			framework.ExpectNoError(err, "failed to create secret for TidbInitializer: %v", initSecret)

			ti := fixture.GetTidbInitializer(ns, tcName, initName, initPassWDName, initTLSName)
			err = genericCli.Create(context.TODO(), ti)
			framework.ExpectNoError(err, "failed to create TidbInitializer: %v", ti)

			source := &tests.TidbClusterConfig{
				Namespace:      ns,
				ClusterName:    tcName,
				OperatorTag:    cfg.OperatorTag,
				ClusterVersion: utilimage.TiDBV4Version,
			}
			targetTcName := "tls-target"
			targetTc := fixture.GetTidbCluster(ns, targetTcName, utilimage.TiDBV4Version)
			targetTc.Spec.PD.Replicas = 1
			targetTc.Spec.TiKV.Replicas = 1
			targetTc.Spec.TiDB.Replicas = 1
			err = genericCli.Create(context.TODO(), targetTc)
			framework.ExpectNoError(err, "failed to create TidbCluster: %v", targetTc)
			err = oa.WaitForTidbClusterReady(targetTc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster timeout: %v", targetTc)

			drainerConfig := &tests.DrainerConfig{
				DrainerName:       "tls-drainer",
				OperatorTag:       cfg.OperatorTag,
				SourceClusterName: tcName,
				Namespace:         ns,
				DbType:            tests.DbTypeTiDB,
				Host:              fmt.Sprintf("%s-tidb.%s.svc.cluster.local", targetTcName, ns),
				Port:              "4000",
				TLSCluster:        true,
				User:              "root",
				Password:          "",
			}

			ginkgo.By("Deploying tidb drainer")
			err = oa.DeployDrainer(drainerConfig, source)
			framework.ExpectNoError(err, "failed to deploy drainer: %v", drainerConfig)
			err = oa.CheckDrainer(drainerConfig, source)
			framework.ExpectNoError(err, "failed to check drainer: %v", drainerConfig)

			ginkgo.By("Inserting data into source db")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, insertIntoDataToSourceDB(fw, c, ns, tcName, passwd, true))
			framework.ExpectNoError(err, "insert data into source db timeout")

			ginkgo.By("Checking tidb-binlog works as expected")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, dataInClusterIsCorrect(fw, c, ns, targetTcName, "", false))
			framework.ExpectNoError(err, "check data correct timeout")

			ginkgo.By("Connecting to tidb server to verify the connection is TLS enabled")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, tidbIsTLSEnabled(fw, c, ns, tcName, passwd))
			framework.ExpectNoError(err, "connect to TLS tidb timeout")

			ginkgo.By("Scaling out tidb cluster")
			err = controller.GuaranteedUpdate(genericCli, tc, func() error {
				tc.Spec.PD.Replicas = 5
				tc.Spec.TiKV.Replicas = 5
				tc.Spec.TiDB.Replicas = 3
				return nil
			})
			framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
			err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

			ginkgo.By("Scaling in tidb cluster")
			err = controller.GuaranteedUpdate(genericCli, tc, func() error {
				tc.Spec.PD.Replicas = 3
				tc.Spec.TiKV.Replicas = 3
				tc.Spec.TiDB.Replicas = 2
				return nil
			})
			framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
			err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

			ginkgo.By("Upgrading tidb cluster")
			err = controller.GuaranteedUpdate(genericCli, tc, func() error {
				tc.Spec.Version = utilimage.TiDBV4UpgradeVersion
				return nil
			})
			framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)
			err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
		})

		ginkgo.It("TLS for MySQL Client and TLS between Heterogeneous TiDB components", func() {
			tcName := "origintls"
			heterogeneousTcName := "heterogeneoustls"

			ginkgo.By("Installing tidb issuer")
			err := installTiDBIssuer(ns, tcName)
			framework.ExpectNoError(err, "failed to generate tidb issuer template")

			ginkgo.By("Installing tidb server and client certificate")
			err = installTiDBCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install tidb server and client certificate")

			ginkgo.By("Installing heterogeneous tidb server and client certificate")
			err = installHeterogeneousTiDBCertificates(ns, heterogeneousTcName, tcName)
			framework.ExpectNoError(err, "failed to install heterogeneous tidb server and client certificate")

			ginkgo.By("Installing separate tidbInitializer client certificate")
			err = installTiDBInitializerCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install separate tidbInitializer client certificate")

			ginkgo.By("Installing separate dashboard client certificate")
			err = installPDDashboardCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install separate dashboard client certificate")

			ginkgo.By("Installing tidb components certificates")
			err = installTiDBComponentsCertificates(ns, tcName)
			framework.ExpectNoError(err, "failed to install tidb components certificates")
			err = installHeterogeneousTiDBComponentsCertificates(ns, heterogeneousTcName, tcName)
			framework.ExpectNoError(err, "failed to install heterogeneous tidb components certificates")

			ginkgo.By("Creating tidb cluster")
			dashTLSName := fmt.Sprintf("%s-dashboard-tls", tcName)
			tc := fixture.GetTidbCluster(ns, tcName, utilimage.TiDBV4UpgradeVersion)
			tc.Spec.PD.Replicas = 1
			tc.Spec.PD.TLSClientSecretName = &dashTLSName
			tc.Spec.TiKV.Replicas = 1
			tc.Spec.TiDB.Replicas = 1
			tc.Spec.TiDB.TLSClient = &v1alpha1.TiDBTLSClient{Enabled: true}
			tc.Spec.TLSCluster = &v1alpha1.TLSCluster{Enabled: true}
			tc.Spec.Pump = &v1alpha1.PumpSpec{
				Replicas:             1,
				BaseImage:            "pingcap/tidb-binlog",
				ResourceRequirements: fixture.WithStorage(fixture.BurstbleSmall, "1Gi"),
				Config: tcconfig.New(map[string]interface{}{
					"addr": "0.0.0.0:8250",
				}),
			}
			err = genericCli.Create(context.TODO(), tc)
			framework.ExpectNoError(err, "failed to create TidbCluster: %v", tc)
			err = oa.WaitForTidbClusterReady(tc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

			ginkgo.By("Creating heterogeneous tidb cluster")
			heterogeneousTc := fixture.GetTidbCluster(ns, heterogeneousTcName, utilimage.TiDBV4UpgradeVersion)
			heterogeneousTc.Spec.PD = nil
			heterogeneousTc.Spec.TiKV.Replicas = 1
			heterogeneousTc.Spec.TiDB.Replicas = 1
			heterogeneousTc.Spec.TiFlash = &v1alpha1.TiFlashSpec{Replicas: 1,
				BaseImage: "pingcap/tiflash", StorageClaims: []v1alpha1.StorageClaim{
					{Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceStorage: resource.MustParse("10G"),
						},
					}},
				}}
			heterogeneousTc.Spec.Cluster = &v1alpha1.TidbClusterRef{
				Name: tcName,
			}

			heterogeneousTc.Spec.TiDB.TLSClient = &v1alpha1.TiDBTLSClient{Enabled: true}
			heterogeneousTc.Spec.TLSCluster = &v1alpha1.TLSCluster{Enabled: true}
			err = genericCli.Create(context.TODO(), heterogeneousTc)
			framework.ExpectNoError(err, "failed to create heterogeneous TidbCluster: %v", heterogeneousTc)
			ginkgo.By("Waiting heterogeneous tls tidb cluster ready")
			err = oa.WaitForTidbClusterReady(heterogeneousTc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)
			ginkgo.By("Checking heterogeneous tls tidb cluster status")
			err = wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
				var err error
				if _, err = cli.PingcapV1alpha1().TidbClusters(ns).Get(heterogeneousTc.Name, metav1.GetOptions{}); err != nil {
					log.Logf("failed to get tidbcluster: %s/%s, %v", ns, heterogeneousTc.Name, err)
					return false, nil
				}
				log.Logf("start check heterogeneous cluster storeInfo: %s/%s", ns, heterogeneousTc.Name)
				pdClient, cancel, err := proxiedpdclient.NewProxiedPDClient(c, fw, ns, tcName, true)
				framework.ExpectNoError(err, "create pdClient error")
				defer cancel()
				storeInfo, err := pdClient.GetStores()
				if err != nil {
					log.Logf("failed to get stores, %v", err)
				}
				if storeInfo.Count != 3 {
					log.Logf("failed to check stores (current: %d)", storeInfo.Count)
					return false, nil
				}
				log.Logf("check heterogeneous tc successfully")
				return true, nil
			})
			framework.ExpectNoError(err, "check heterogeneous TidbCluster timeout: %v", heterogeneousTc)

			ginkgo.By("Ensure Dashboard use custom secret")
			foundSecretName := false
			pdSts, err := stsGetter.StatefulSets(ns).Get(controller.PDMemberName(tcName), metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get statefulsets for pd")
			for _, vol := range pdSts.Spec.Template.Spec.Volumes {
				if vol.Name == "tidb-client-tls" {
					foundSecretName = true
					framework.ExpectEqual(vol.Secret.SecretName, dashTLSName)
				}
			}
			framework.ExpectEqual(foundSecretName, true)

			ginkgo.By("Creating tidb initializer")
			passwd := "admin"
			initName := fmt.Sprintf("%s-initializer", tcName)
			initPassWDName := fmt.Sprintf("%s-initializer-passwd", tcName)
			initTLSName := fmt.Sprintf("%s-initializer-tls", tcName)
			initSecret := fixture.GetInitializerSecret(tc, initPassWDName, passwd)
			_, err = c.CoreV1().Secrets(ns).Create(initSecret)
			framework.ExpectNoError(err, "failed to create secret for TidbInitializer: %v", initSecret)

			ti := fixture.GetTidbInitializer(ns, tcName, initName, initPassWDName, initTLSName)
			err = genericCli.Create(context.TODO(), ti)
			framework.ExpectNoError(err, "failed to create TidbInitializer: %v", ti)

			source := &tests.TidbClusterConfig{
				Namespace:      ns,
				ClusterName:    tcName,
				OperatorTag:    cfg.OperatorTag,
				ClusterVersion: utilimage.TiDBV4Version,
			}
			targetTcName := "tls-target"
			targetTc := fixture.GetTidbCluster(ns, targetTcName, utilimage.TiDBV4Version)
			targetTc.Spec.PD.Replicas = 1
			targetTc.Spec.TiKV.Replicas = 1
			targetTc.Spec.TiDB.Replicas = 1
			err = genericCli.Create(context.TODO(), targetTc)
			framework.ExpectNoError(err, "failed to create TidbCluster: %v", targetTc)
			err = oa.WaitForTidbClusterReady(targetTc, 30*time.Minute, 15*time.Second)
			framework.ExpectNoError(err, "wait for TidbCluster ready timeout: %v", tc)

			drainerConfig := &tests.DrainerConfig{
				DrainerName:       "origintls-drainer",
				OperatorTag:       cfg.OperatorTag,
				SourceClusterName: tcName,
				Namespace:         ns,
				DbType:            tests.DbTypeTiDB,
				Host:              fmt.Sprintf("%s-tidb.%s.svc.cluster.local", targetTcName, ns),
				Port:              "4000",
				TLSCluster:        true,
				User:              "root",
				Password:          "",
			}

			ginkgo.By("Deploying tidb drainer")
			err = oa.DeployDrainer(drainerConfig, source)
			framework.ExpectNoError(err, "failed to deploy drainer: %v", drainerConfig)
			err = oa.CheckDrainer(drainerConfig, source)
			framework.ExpectNoError(err, "failed to check drainer: %v", drainerConfig)

			ginkgo.By("Inserting data into source db")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, insertIntoDataToSourceDB(fw, c, ns, tcName, passwd, true))
			framework.ExpectNoError(err, "insert data into source db timeout")

			ginkgo.By("Checking tidb-binlog works as expected")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, dataInClusterIsCorrect(fw, c, ns, targetTcName, "", false))
			framework.ExpectNoError(err, "check data correct timeout")

			ginkgo.By("Connecting to tidb server to verify the connection is TLS enabled")
			err = wait.PollImmediate(time.Second*5, time.Minute*5, tidbIsTLSEnabled(fw, c, ns, tcName, passwd))
			framework.ExpectNoError(err, "connect to TLS tidb timeout")

		})
	})

	ginkgo.It("Ensure Service NodePort Not Change", func() {
		// Create TidbCluster with NodePort to check whether node port would change
		nodeTc := fixture.GetTidbCluster(ns, "nodeport", utilimage.TiDBV3Version)
		nodeTc.Spec.PD.Replicas = 1
		nodeTc.Spec.TiKV.Replicas = 1
		nodeTc.Spec.TiDB.Replicas = 1
		nodeTc.Spec.TiDB.Service = &v1alpha1.TiDBServiceSpec{
			ServiceSpec: v1alpha1.ServiceSpec{
				Type: corev1.ServiceTypeNodePort,
			},
		}
		err := genericCli.Create(context.TODO(), nodeTc)
		framework.ExpectNoError(err, "Expected TiDB cluster created")
		err = oa.WaitForTidbClusterReady(nodeTc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Expected TiDB cluster ready")

		// expect tidb service type is Nodeport
		var s *corev1.Service
		err = wait.Poll(5*time.Second, 1*time.Minute, func() (done bool, err error) {
			s, err = c.CoreV1().Services(ns).Get("nodeport-tidb", metav1.GetOptions{})
			if err != nil {
				framework.Logf(err.Error())
				return false, nil
			}
			if s.Spec.Type != corev1.ServiceTypeNodePort {
				return false, fmt.Errorf("nodePort tidbcluster tidb service type isn't NodePort")
			}
			return true, nil
		})
		framework.ExpectNoError(err, "wait for tidb service sync timeout")
		ports := s.Spec.Ports

		// f is the function to check whether service NodePort have changed for 1 min
		ensureSvcNodePortUnchangedFor1Min := func() {
			err := wait.Poll(5*time.Second, 1*time.Minute, func() (done bool, err error) {
				s, err := c.CoreV1().Services(ns).Get("nodeport-tidb", metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if s.Spec.Type != corev1.ServiceTypeNodePort {
					return false, err
				}
				for _, dport := range s.Spec.Ports {
					for _, eport := range ports {
						if dport.Port == eport.Port && dport.NodePort != eport.NodePort {
							return false, fmt.Errorf("nodePort tidbcluster tidb service NodePort changed")
						}
					}
				}
				return false, nil
			})
			framework.ExpectEqual(err, wait.ErrWaitTimeout, "service NodePort should not change in 1 minute")
		}
		// check whether nodeport have changed for 1 min
		ensureSvcNodePortUnchangedFor1Min()
		framework.Logf("tidbcluster tidb service NodePort haven't changed")

		nodeTc, err = cli.PingcapV1alpha1().TidbClusters(ns).Get("nodeport", metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get TidbCluster")
		err = controller.GuaranteedUpdate(genericCli, nodeTc, func() error {
			nodeTc.Spec.TiDB.Service.Annotations = map[string]string{
				"foo": "bar",
			}
			return nil
		})
		framework.ExpectNoError(err, "failed to update TidbCluster: %v", nodeTc)

		// check whether the tidb svc have updated
		err = wait.Poll(5*time.Second, 2*time.Minute, func() (done bool, err error) {
			s, err := c.CoreV1().Services(ns).Get("nodeport-tidb", metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if s.Annotations == nil {
				return false, nil
			}
			v, ok := s.Annotations["foo"]
			if !ok {
				return false, nil
			}
			if v != "bar" {
				return false, fmt.Errorf("tidb svc annotation foo not equal bar")
			}
			return true, nil
		})
		framework.ExpectNoError(err, "wait for service sync timeout")
		framework.Logf("tidb nodeport svc updated")

		// check whether NodePort have changed for 1 min
		ensureSvcNodePortUnchangedFor1Min()
		framework.Logf("tidbcluster tidb service NodePort haven't changed after update")
	})

	ginkgo.It("Heterogeneous: Add heterogeneous cluster into an existing cluster  ", func() {
		// Create TidbCluster with NodePort to check whether node port would change
		originTc := fixture.GetTidbCluster(ns, "origin", utilimage.TiDBV4UpgradeVersion)
		originTc.Spec.PD.Replicas = 1
		originTc.Spec.TiKV.Replicas = 1
		originTc.Spec.TiDB.Replicas = 1
		err := genericCli.Create(context.TODO(), originTc)
		framework.ExpectNoError(err, "Expected TiDB cluster created")
		err = oa.WaitForTidbClusterReady(originTc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Expected TiDB cluster ready")

		heterogeneousTc := fixture.GetTidbCluster(ns, "heterogeneous", utilimage.TiDBV4UpgradeVersion)
		heterogeneousTc.Spec.PD = nil
		heterogeneousTc.Spec.TiKV.Replicas = 1
		heterogeneousTc.Spec.TiDB.Replicas = 1
		heterogeneousTc.Spec.TiFlash = &v1alpha1.TiFlashSpec{Replicas: 1,
			BaseImage: "pingcap/tiflash", StorageClaims: []v1alpha1.StorageClaim{
				{Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: resource.MustParse("10G"),
					},
				}},
			}}
		heterogeneousTc.Spec.Cluster = &v1alpha1.TidbClusterRef{
			Name: originTc.Name,
		}
		err = genericCli.Create(context.TODO(), heterogeneousTc)
		framework.ExpectNoError(err, "Expected Heterogeneous TiDB cluster created")
		err = oa.WaitForTidbClusterReady(heterogeneousTc, 15*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Expected Heterogeneous TiDB cluster ready")
		err = wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
			var err error
			if _, err = cli.PingcapV1alpha1().TidbClusters(ns).Get(heterogeneousTc.Name, metav1.GetOptions{}); err != nil {
				log.Logf("failed to get tidbcluster: %s/%s, %v", ns, heterogeneousTc.Name, err)
				return false, nil
			}
			log.Logf("start check heterogeneous cluster storeInfo: %s/%s", ns, heterogeneousTc.Name)
			pdClient, cancel, err := proxiedpdclient.NewProxiedPDClient(c, fw, ns, originTc.Name, false)
			framework.ExpectNoError(err, "create pdClient error")
			defer cancel()
			storeInfo, err := pdClient.GetStores()
			if err != nil {
				log.Logf("failed to get stores, %v", err)
			}
			if storeInfo.Count != 3 {
				log.Logf("failed to check stores (current: %d)", storeInfo.Count)
				return false, nil
			}
			log.Logf("check heterogeneous tc successfully")
			return true, nil
		})
		framework.ExpectNoError(err, "check heterogeneous timeout")

	})

	ginkgo.It("[Feature: CDC]", func() {
		ginkgo.By("Creating cdc cluster")
		fromTc := fixture.GetTidbCluster(ns, "cdc-source", utilimage.TiDBV4Version)
		fromTc.Spec.PD.Replicas = 3
		fromTc.Spec.TiKV.Replicas = 3
		fromTc.Spec.TiDB.Replicas = 2
		fromTc.Spec.TiCDC = &v1alpha1.TiCDCSpec{
			BaseImage: "pingcap/ticdc",
			Replicas:  3,
		}
		err := genericCli.Create(context.TODO(), fromTc)
		framework.ExpectNoError(err, "Expected TiDB cluster created")
		err = oa.WaitForTidbClusterReady(fromTc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Expected TiDB cluster ready")

		ginkgo.By("Creating cdc-sink cluster")
		toTc := fixture.GetTidbCluster(ns, "cdc-sink", utilimage.TiDBV4Version)
		toTc.Spec.PD.Replicas = 1
		toTc.Spec.TiKV.Replicas = 1
		toTc.Spec.TiDB.Replicas = 1
		err = genericCli.Create(context.TODO(), toTc)
		framework.ExpectNoError(err, "Expected TiDB cluster created")
		err = oa.WaitForTidbClusterReady(toTc, 30*time.Minute, 15*time.Second)
		framework.ExpectNoError(err, "Expected TiDB cluster ready")

		ginkgo.By("Creating change feed task")
		fromTCName := fromTc.Name
		toTCName := toTc.Name
		args := []string{
			"exec", "-n", ns,
			fmt.Sprintf("%s-0", controller.TiCDCMemberName(fromTCName)),
			"--",
			"/cdc", "cli", "changefeed", "create",
			fmt.Sprintf("--sink-uri=tidb://root:@%s:4000/", controller.TiDBMemberName(toTCName)),
			fmt.Sprintf("--pd=http://%s:2379", controller.PDMemberName(fromTCName)),
		}
		data, err := framework.RunKubectl(args...)
		framework.ExpectNoError(err, "failed to create change feed task: %s, %v", string(data), err)

		ginkgo.By("Inserting data to cdc cluster")
		err = wait.PollImmediate(time.Second*5, time.Minute*5, insertIntoDataToSourceDB(fw, c, ns, fromTCName, "", false))
		framework.ExpectNoError(err, "insert data to cdc cluster timeout")

		ginkgo.By("Checking cdc works as expected")
		err = wait.PollImmediate(time.Second*5, time.Minute*5, dataInClusterIsCorrect(fw, c, ns, toTCName, "", false))
		framework.ExpectNoError(err, "check cdc timeout")

		framework.Logf("CDC works as expected")
	})

	ginkgo.Context("when stores number is equal to 3", func() {
		ginkgo.It("forbid to scale in TiKV and the state of all stores are up", func() {
			tc := fixture.GetTidbCluster(ns, "scale-in-tikv-test", utilimage.TiDBV4Version)
			tc, err := cli.PingcapV1alpha1().TidbClusters(tc.Namespace).Create(tc)
			framework.ExpectNoError(err, "Expected create tidbcluster")
			err = oa.WaitForTidbClusterReady(tc, 10*time.Minute, 5*time.Second)
			framework.ExpectNoError(err, "Expected get tidbcluster")

			// scale in tikv
			err = controller.GuaranteedUpdate(genericCli, tc, func() error {
				tc.Spec.TiKV.Replicas = 2
				return nil
			})
			framework.ExpectNoError(err, "failed to update TidbCluster: %v", tc)

			pdClient, cancel, err := proxiedpdclient.NewProxiedPDClient(c, fw, ns, tc.Name, false)
			framework.ExpectNoError(err, "create pdClient error")
			defer cancel()
			storesInfo, err := pdClient.GetStores()
			framework.ExpectNoError(err, "get stores info error")

			_ = wait.PollImmediate(5*time.Second, 3*time.Minute, func() (bool, error) {
				framework.ExpectEqual(storesInfo.Count, 3, "Expect number of stores is 3")
				for _, store := range storesInfo.Stores {
					framework.ExpectEqual(store.Store.StateName, "Up", "Expect state of stores are Up")
				}
				return false, nil
			})
		})
	})

	ginkgo.It("TiKV mount multiple pvc", func() {

		clusterName := "tidb-multiple-pvc-scale"
		tc := fixture.GetTidbCluster(ns, clusterName, utilimage.TiDBV4Version)
		tc.Spec.TiKV.StorageVolumes = []v1alpha1.StorageVolume{
			{
				Name:        "wal",
				StorageSize: "2Gi",
				MountPath:   "/var/lib/wal",
			},
			{
				Name:        "titan",
				StorageSize: "2Gi",
				MountPath:   "/var/lib/titan",
			},
		}
		tc.Spec.TiDB.StorageVolumes = []v1alpha1.StorageVolume{
			{
				Name:        "log",
				StorageSize: "2Gi",
				MountPath:   "/var/log",
			},
		}
		tc.Spec.PD.StorageVolumes = []v1alpha1.StorageVolume{
			{
				Name:        "log",
				StorageSize: "2Gi",
				MountPath:   "/var/log",
			},
		}

		tc.Spec.PD.Config.Set("log.file.filename", "/var/log/tidb/tidb.log")
		tc.Spec.PD.Config.Set("log.level", "warn")
		tc.Spec.TiDB.Config.Set("log.file.max-size", "300")
		tc.Spec.TiDB.Config.Set("log.file.max-days", "1")
		tc.Spec.TiDB.Config.Set("log.file.filename", "/var/log/tidb/tidb.log")
		tc.Spec.TiDB.Config.Set("log.level", "warn")
		tc.Spec.TiDB.Config.Set("log.file.max-size", "300")
		tc.Spec.TiDB.Config.Set("log.file.max-days", "1")
		tc.Spec.TiKV.Config.Set("rocksdb.wal-dir", "/var/lib/wal")
		tc.Spec.TiKV.Config.Set("titan.dirname", "/var/lib/titan")
		clusterConfig := newTidbClusterConfig(e2econfig.TestConfig, ns, clusterName, "admin", utilimage.TiDBV4Version)
		clusterConfig.Resources["pd.replicas"] = "1"
		clusterConfig.Resources["tikv.replicas"] = "4"
		clusterConfig.Resources["tidb.replicas"] = "1"
		clusterConfig.Clustrer = tc

		log.Logf("deploying tidb cluster [%s/%s]", clusterConfig.Namespace, clusterConfig.ClusterName)
		oa.DeployTidbClusterOrDie(&clusterConfig)
		oa.CheckTidbClusterStatusOrDie(&clusterConfig)

		ginkgo.By("scale multiple pvc tidb cluster")
		clusterConfig.ScaleTiKV(3)
		oa.UpgradeTidbClusterOrDie(&clusterConfig)
		oa.CheckTidbClusterStatusOrDie(&clusterConfig)

		ginkgo.By("scale out multiple pvc tidb cluster")
		clusterConfig.ScaleTiKV(4)
		oa.UpgradeTidbClusterOrDie(&clusterConfig)
		oa.CheckTidbClusterStatusOrDie(&clusterConfig)
	})
})

func newTidbClusterConfig(cfg *tests.Config, ns, clusterName, password, tidbVersion string) tests.TidbClusterConfig {
	return tests.TidbClusterConfig{
		Namespace:        ns,
		ClusterName:      clusterName,
		EnablePVReclaim:  false,
		OperatorTag:      cfg.OperatorTag,
		PDImage:          fmt.Sprintf("pingcap/pd:%s", tidbVersion),
		TiKVImage:        fmt.Sprintf("pingcap/tikv:%s", tidbVersion),
		TiDBImage:        fmt.Sprintf("pingcap/tidb:%s", tidbVersion),
		PumpImage:        fmt.Sprintf("pingcap/tidb-binlog:%s", tidbVersion),
		StorageClassName: "local-storage",
		Password:         password,
		UserName:         "root",
		InitSecretName:   fmt.Sprintf("%s-set-secret", clusterName),
		BackupSecretName: fmt.Sprintf("%s-backup-secret", clusterName),
		BackupName:       "backup",
		Resources: map[string]string{
			"discovery.resources.limits.cpu":      "1000m",
			"discovery.resources.limits.memory":   "2Gi",
			"discovery.resources.requests.cpu":    "20m",
			"discovery.resources.requests.memory": "20Mi",
			"pd.resources.limits.cpu":             "1000m",
			"pd.resources.limits.memory":          "2Gi",
			"pd.resources.requests.cpu":           "20m",
			"pd.resources.requests.memory":        "20Mi",
			"tikv.resources.limits.cpu":           "2000m",
			"tikv.resources.limits.memory":        "4Gi",
			"tikv.resources.requests.cpu":         "20m",
			"tikv.resources.requests.memory":      "20Mi",
			"tidb.resources.limits.cpu":           "2000m",
			"tidb.resources.limits.memory":        "4Gi",
			"tidb.resources.requests.cpu":         "20m",
			"tidb.resources.requests.memory":      "20Mi",
			"tidb.initSql":                        strconv.Quote("create database e2e;"),
			"discovery.image":                     cfg.OperatorImage,
		},
		Args:    map[string]string{},
		Monitor: true,
		BlockWriteConfig: blockwriter.Config{
			TableNum:    1,
			Concurrency: 1,
			BatchSize:   1,
			RawSize:     1,
		},
		TopologyKey:            "rack",
		EnableConfigMapRollout: true,
		ClusterVersion:         tidbVersion,
	}
}
