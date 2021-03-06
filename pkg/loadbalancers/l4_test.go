package loadbalancers

/*
Copyright 2020 The Kubernetes Authors.

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

import (
	"google.golang.org/api/compute/v1"
	"k8s.io/ingress-gce/pkg/backends"
	"k8s.io/ingress-gce/pkg/firewalls"
	"k8s.io/ingress-gce/pkg/metrics"
	"k8s.io/ingress-gce/pkg/utils"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/mock"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/healthchecks"
	"k8s.io/ingress-gce/pkg/test"
	namer_util "k8s.io/ingress-gce/pkg/utils/namer"
	"k8s.io/legacy-cloud-providers/gce"
)

const (
	clusterUID = "aaaaa"
	// TODO Uncomment after https://github.com/kubernetes/kubernetes/pull/87667 is available in vendor.
	// eventMsgFirewallChange = "XPN Firewall change required by network admin"
)

var (
	fakeMetricsCollector = metrics.NewControllerMetrics()
)

func getFakeGCECloud(vals gce.TestClusterValues) *gce.Cloud {
	fakeGCE := gce.NewFakeGCECloud(vals)
	// InsertHook required to assign an IP Address for forwarding rule
	(fakeGCE.Compute().(*cloud.MockGCE)).MockAddresses.InsertHook = mock.InsertAddressHook
	(fakeGCE.Compute().(*cloud.MockGCE)).MockAlphaAddresses.X = mock.AddressAttributes{}
	(fakeGCE.Compute().(*cloud.MockGCE)).MockAddresses.X = mock.AddressAttributes{}
	(fakeGCE.Compute().(*cloud.MockGCE)).MockForwardingRules.InsertHook = mock.InsertFwdRuleHook

	(fakeGCE.Compute().(*cloud.MockGCE)).MockRegionBackendServices.UpdateHook = mock.UpdateRegionBackendServiceHook
	(fakeGCE.Compute().(*cloud.MockGCE)).MockHealthChecks.UpdateHook = mock.UpdateHealthCheckHook
	(fakeGCE.Compute().(*cloud.MockGCE)).MockFirewalls.UpdateHook = mock.UpdateFirewallHook
	return fakeGCE
}

func TestEnsureInternalBackendServiceUpdates(t *testing.T) {
	t.Parallel()
	fakeGCE := getFakeGCECloud(gce.DefaultTestClusterValues())
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	bsName := l.namer.VMIPNEG(l.Service.Namespace, l.Service.Name)
	_, err := l.backendPool.EnsureL4BackendService(bsName, "", "TCP", string(svc.Spec.SessionAffinity), string(cloud.SchemeInternal), l.NamespacedName, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to ensure backend service  %s - err %v", bsName, err)
	}

	// Update the Internal Backend Service with a new ServiceAffinity
	_, err = l.backendPool.EnsureL4BackendService(bsName, "", "TCP", string(v1.ServiceAffinityNone), string(cloud.SchemeInternal), l.NamespacedName, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to ensure backend service  %s - err %v", bsName, err)
	}
	key := meta.RegionalKey(bsName, l.cloud.Region())
	bs, err := composite.GetBackendService(l.cloud, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to get backend service  %s - err %v", bsName, err)
	}
	if bs.SessionAffinity != strings.ToUpper(string(v1.ServiceAffinityNone)) {
		t.Errorf("Expected session affinity '%s' in %+v, Got '%s'", strings.ToUpper(string(v1.ServiceAffinityNone)), bs, bs.SessionAffinity)
	}
	// Change the Connection Draining timeout to a different value manually. Also update session Affinity to trigger
	// an update in the Ensure method. The timeout value should not be reconciled.
	newTimeout := int64(backends.DefaultConnectionDrainingTimeoutSeconds * 2)
	bs.ConnectionDraining.DrainingTimeoutSec = newTimeout
	bs.SessionAffinity = strings.ToUpper(string(v1.ServiceAffinityClientIP))
	err = composite.UpdateBackendService(l.cloud, key, bs)
	if err != nil {
		t.Errorf("Failed to update backend service with new connection draining timeout - err %v", err)
	}
	bs, err = l.backendPool.EnsureL4BackendService(bsName, "", "TCP", string(v1.ServiceAffinityNone), string(cloud.SchemeInternal), l.NamespacedName, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to ensure backend service  %s - err %v", bsName, err)
	}
	if bs.SessionAffinity != strings.ToUpper(string(v1.ServiceAffinityNone)) {
		t.Errorf("Backend service did not get updated.")
	}
	if bs.ConnectionDraining.DrainingTimeoutSec != newTimeout {
		t.Errorf("Connection Draining timeout got reconciled to %d, expected %d", bs.ConnectionDraining.DrainingTimeoutSec, newTimeout)
	}
}

func TestEnsureInternalLoadBalancer(t *testing.T) {
	t.Parallel()
	nodeNames := []string{"test-node-1"}
	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)

	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}

	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)
}

func TestEnsureInternalLoadBalancerTypeChange(t *testing.T) {
	t.Parallel()
	nodeNames := []string{"test-node-1"}
	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)

	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	// Now add the latest annotation and change scheme to external
	svc.Annotations[gce.ServiceAnnotationLoadBalancerType] = ""
	// This will be invoked by service_controller
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalLoadBalancerWithExistingResources(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	nodeNames := []string{"test-node-1"}

	fakeGCE := getFakeGCECloud(vals)
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}

	lbName := l.namer.VMIPNEG(svc.Namespace, svc.Name)

	// Create the expected resources necessary for an Internal Load Balancer
	sharedHC := !servicehelper.RequestsOnlyLocalTraffic(svc)
	hcName, _ := healthchecks.HealthCheckName(sharedHC, l.namer.UID(), lbName)
	hcPath, hcPort := gce.GetNodesHealthCheckPath(), gce.GetNodesHealthCheckPort()
	_, hcLink, err := healthchecks.EnsureL4HealthCheck(l.cloud, hcName, l.NamespacedName, sharedHC, hcPath, hcPort)
	if err != nil {
		t.Errorf("Failed to create healthcheck, err %v", err)
	}
	_, err = l.backendPool.EnsureL4BackendService(lbName, hcLink, "TCP", string(l.Service.Spec.SessionAffinity),
		string(cloud.SchemeInternal), l.NamespacedName, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to create backendservice, err %v", err)
	}
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)
}

// TestEnsureInternalLoadBalancerClearPreviousResources creates ILB resources with incomplete configuration and verifies
// that they are updated when the controller processes the load balancer service.
func TestEnsureInternalLoadBalancerClearPreviousResources(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	nodeNames := []string{"test-node-1"}

	fakeGCE := getFakeGCECloud(vals)
	svc := test.NewL4ILBService(true, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}

	lbName := l.namer.VMIPNEG(svc.Namespace, svc.Name)
	frName := l.GetFRName()
	key, err := composite.CreateKey(l.cloud, frName, meta.Regional)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}

	// Create a ForwardingRule that's missing an IP address
	existingFwdRule := &composite.ForwardingRule{
		Name:                frName,
		IPAddress:           "",
		Ports:               []string{"123"},
		IPProtocol:          "TCP",
		LoadBalancingScheme: string(cloud.SchemeInternal),
	}
	if err = composite.CreateForwardingRule(l.cloud, key, existingFwdRule); err != nil {
		t.Errorf("Failed to create fake forwarding rule %s, err %v", lbName, err)
	}
	key.Name = lbName
	// Create a Firewall that's missing a Description
	existingFirewall := &compute.Firewall{
		Name:    lbName,
		Network: fakeGCE.NetworkURL(),
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"123"},
			},
		},
	}
	fakeGCE.CreateFirewall(existingFirewall)

	sharedHealthCheck := !servicehelper.RequestsOnlyLocalTraffic(svc)
	hcName, _ := healthchecks.HealthCheckName(sharedHealthCheck, l.namer.UID(), lbName)

	// Create a healthcheck with an incomplete fields
	existingHC := &composite.HealthCheck{Name: hcName}
	// hcName will be same as lbName since this service uses trafficPolicy Local. So the same key can be used.
	if err = composite.CreateHealthCheck(fakeGCE, key, existingHC); err != nil {
		t.Errorf("Failed to create fake healthcheck %s, err %v", hcName, err)
	}

	// Create a backend Service that's missing Description and Backends
	existingBS := &composite.BackendService{
		Name:                lbName,
		Protocol:            "TCP",
		LoadBalancingScheme: string(cloud.SchemeInternal),
	}

	if err = composite.CreateBackendService(fakeGCE, key, existingBS); err != nil {
		t.Errorf("Failed to create fake backend service %s, err %v", lbName, err)
	}
	existingFwdRule.BackendService = existingBS.Name

	if _, err = l.EnsureInternalLoadBalancer(nodeNames, svc); err != nil {
		t.Errorf("Failed to ensure loadBalancer %s, err %v", lbName, err)
	}
	key.Name = frName
	// Expect new resources with the correct attributes to be created
	newFwdRule, err := composite.GetForwardingRule(fakeGCE, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to lookup forwarding rule %s, err %v", lbName, err)
	}
	if newFwdRule == existingFwdRule {
		t.Errorf("Expected incomplete forwarding rule to be updated")
	}

	newFirewall, err := fakeGCE.GetFirewall(lbName)
	if err != nil {
		t.Errorf("Failed to lookup firewall rule %s, err %v", lbName, err)
	}
	if newFirewall == existingFirewall {
		t.Errorf("Expected incomplete firewall rule to be updated")
	}

	key.Name = lbName
	newHC, err := composite.GetHealthCheck(fakeGCE, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to lookup healthcheck %s, err %v", lbName, err)
	}
	if newHC == existingHC || newHC.SelfLink == "" {
		t.Errorf("Expected incomplete healthcheck to be updated")
	}

	newBS, err := composite.GetBackendService(fakeGCE, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to lookup backend service %s, err %v", lbName, err)
	}
	if newBS == existingBS {
		t.Errorf("Expected incomplete backend service to be updated")
	}
}

// TestUpdateResourceLinks verifies that an existing backend service created with different healthchecks is reconciled
// upon load balancer sync. The other healthchecks are not deleted.
func TestUpdateResourceLinks(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	nodeNames := []string{"test-node-1"}

	fakeGCE := getFakeGCECloud(vals)
	svc := test.NewL4ILBService(true, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}

	lbName := l.namer.VMIPNEG(svc.Namespace, svc.Name)
	key, err := composite.CreateKey(l.cloud, lbName, meta.Regional)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}
	key.Name = "hc1"
	hc1 := &composite.HealthCheck{Name: "hc1"}
	if err = composite.CreateHealthCheck(fakeGCE, key, hc1); err != nil {
		t.Errorf("Failed to create fake healthcheck hc1 , err %v", err)
	}

	key.Name = "hc2"
	hc2 := &composite.HealthCheck{Name: "hc2"}
	if err = composite.CreateHealthCheck(fakeGCE, key, hc2); err != nil {
		t.Errorf("Failed to create fake healthcheck hc2, err %v", err)
	}

	key.Name = lbName
	// Create a backend Service that's missing Description and Backends
	existingBS := &composite.BackendService{
		Name:                lbName,
		Protocol:            "TCP",
		LoadBalancingScheme: string(cloud.SchemeInternal),
		HealthChecks:        []string{"hc1", "hc2"},
	}

	if err = composite.CreateBackendService(fakeGCE, key, existingBS); err != nil {
		t.Errorf("Failed to create fake backend service %s, err %v", lbName, err)
	}
	bs, err := composite.GetBackendService(fakeGCE, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to lookup backend service")
	}
	if !reflect.DeepEqual(bs.HealthChecks, []string{"hc1", "hc2"}) {
		t.Errorf("Unexpected healthchecks in backend service - %v", bs.HealthChecks)
	}
	if _, err = l.EnsureInternalLoadBalancer(nodeNames, svc); err != nil {
		t.Errorf("Failed to ensure loadBalancer %s, err %v", lbName, err)
	}
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	// verifies that the right healthcheck is present
	assertInternalLbResources(t, svc, l, nodeNames)

	// ensure that the other healthchecks still exist.
	key.Name = "hc1"
	if hc1, err = composite.GetHealthCheck(fakeGCE, key, meta.VersionGA); err != nil {
		t.Errorf("Failed to lookup healthcheck - hc1")
	}
	if hc1 == nil {
		t.Errorf("Got nil healthcheck")
	}
	key.Name = "hc2"
	if hc2, err = composite.GetHealthCheck(fakeGCE, key, meta.VersionGA); err != nil {
		t.Errorf("Failed to lookup healthcheck - hc1")
	}
	if hc2 == nil {
		t.Errorf("Got nil healthcheck")
	}
}

func TestEnsureInternalLoadBalancerHealthCheckConfigurable(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	nodeNames := []string{"test-node-1"}

	fakeGCE := getFakeGCECloud(vals)
	svc := test.NewL4ILBService(true, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	lbName := l.namer.VMIPNEG(svc.Namespace, svc.Name)
	key, err := composite.CreateKey(l.cloud, lbName, meta.Regional)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}
	sharedHealthCheck := !servicehelper.RequestsOnlyLocalTraffic(svc)
	hcName, _ := healthchecks.HealthCheckName(sharedHealthCheck, l.namer.UID(), lbName)

	// Create a healthcheck with an incorrect threshold, default value is 8s.
	existingHC := &composite.HealthCheck{Name: hcName, CheckIntervalSec: 6000}
	if err = composite.CreateHealthCheck(fakeGCE, key, existingHC); err != nil {
		t.Errorf("Failed to create fake healthcheck %s, err %v", hcName, err)
	}

	if _, err = l.EnsureInternalLoadBalancer(nodeNames, svc); err != nil {
		t.Errorf("Failed to ensure loadBalancer %s, err %v", lbName, err)
	}

	newHC, err := composite.GetHealthCheck(fakeGCE, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to lookup healthcheck %s, err %v", lbName, err)
	}
	if newHC.CheckIntervalSec != existingHC.CheckIntervalSec {
		t.Errorf("Check interval got incorrectly reconciled")
	}
}

func TestEnsureInternalLoadBalancerDeleted(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)
	nodeNames := []string{"test-node-1"}
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	// Delete the loadbalancer
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalLoadBalancerDeletedTwiceDoesNotError(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)
	nodeNames := []string{"test-node-1"}
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	// Delete the loadbalancer
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)

	// Deleting the loadbalancer and resources again should not cause an error.
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalLoadBalancerWithSpecialHealthCheck(t *testing.T) {
	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)
	nodeNames := []string{"test-node-1"}
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}

	healthCheckNodePort := int32(10101)
	svc.Spec.HealthCheckNodePort = healthCheckNodePort
	svc.Spec.Type = v1.ServiceTypeLoadBalancer
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal

	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	lbName := l.namer.VMIPNEG(svc.Namespace, svc.Name)
	key, err := composite.CreateKey(l.cloud, lbName, meta.Global)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}
	hc, err := composite.GetHealthCheck(l.cloud, key, meta.VersionGA)
	if err != nil || hc == nil {
		t.Errorf("Failed to get healthcheck, err %v", err)
	}
	if hc.HttpHealthCheck.Port != int64(healthCheckNodePort) {
		t.Errorf("Unexpected port in healthcheck, expected %d, Got %d", healthCheckNodePort, hc.HttpHealthCheck.Port)
	}
}

type EnsureILBParams struct {
	clusterName     string
	clusterID       string
	service         *v1.Service
	existingFwdRule *composite.ForwardingRule
}

// newEnsureILBParams is the constructor of EnsureILBParams.
func newEnsureILBParams() *EnsureILBParams {
	vals := gce.DefaultTestClusterValues()
	return &EnsureILBParams{
		vals.ClusterName,
		vals.ClusterID,
		test.NewL4ILBService(false, 8080),
		nil,
	}
}

// TestEnsureInternalLoadBalancerErrors tests the function
// EnsureInternalLoadBalancer, making sure the system won't panic when
// exceptions raised by gce.
func TestEnsureInternalLoadBalancerErrors(t *testing.T) {
	vals := gce.DefaultTestClusterValues()
	var params *EnsureILBParams

	for desc, tc := range map[string]struct {
		adjustParams func(*EnsureILBParams)
		injectMock   func(*cloud.MockGCE)
	}{
		"EnsureInternalBackendService failed": {
			injectMock: func(c *cloud.MockGCE) {
				c.MockRegionBackendServices.GetHook = mock.GetRegionBackendServicesErrHook
			},
		},
		"Create internal health check failed": {
			injectMock: func(c *cloud.MockGCE) {
				c.MockHealthChecks.GetHook = mock.GetHealthChecksInternalErrHook
			},
		},
		"Create firewall failed": {
			injectMock: func(c *cloud.MockGCE) {
				c.MockFirewalls.InsertHook = mock.InsertFirewallsUnauthorizedErrHook
			},
		},
		"Create region forwarding rule failed": {
			injectMock: func(c *cloud.MockGCE) {
				c.MockForwardingRules.InsertHook = mock.InsertForwardingRulesInternalErrHook
			},
		},
		"Get region forwarding rule failed": {
			injectMock: func(c *cloud.MockGCE) {
				c.MockForwardingRules.GetHook = mock.GetForwardingRulesInternalErrHook
			},
		},
		"Delete region forwarding rule failed": {
			adjustParams: func(params *EnsureILBParams) {
				params.existingFwdRule = &composite.ForwardingRule{BackendService: "badBackendService"}
			},
			injectMock: func(c *cloud.MockGCE) {
				c.MockForwardingRules.DeleteHook = mock.DeleteForwardingRuleErrHook
			},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			fakeGCE := getFakeGCECloud(gce.DefaultTestClusterValues())
			nodeNames := []string{"test-node-1"}
			params = newEnsureILBParams()
			if tc.adjustParams != nil {
				tc.adjustParams(params)
			}
			namer := namer_util.NewNamer(clusterUID, "")
			l := NewL4Handler(params.service, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
			//lbName := l.namer.VMIPNEG(params.service.Namespace, params.service.Name)
			frName := l.GetFRName()
			key, err := composite.CreateKey(l.cloud, frName, meta.Regional)
			if err != nil {
				t.Errorf("Unexpected error when creating key - %v", err)
			}
			_, err = test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
			if err != nil {
				t.Errorf("Unexpected error when adding nodes %v", err)
			}
			// Create a dummy forwarding rule in order to trigger a delete in the EnsureInternalLoadBalancer function.
			if err = composite.CreateForwardingRule(l.cloud, key, &composite.ForwardingRule{Name: frName}); err != nil {
				t.Errorf("Failed to create fake forwarding rule %s, err %v", frName, err)
			}
			// Inject error hooks after creating the forwarding rule
			if tc.injectMock != nil {
				tc.injectMock(fakeGCE.Compute().(*cloud.MockGCE))
			}
			status, err := l.EnsureInternalLoadBalancer(nodeNames, params.service)
			if err == nil {
				t.Errorf("Expected error when %s", desc)
			}
			if status != nil {
				t.Errorf("Expected empty status when %s, Got %v", desc, status)
			}
		})
	}
}

/* TODO uncomment after https://github.com/kubernetes/kubernetes/pull/87667 is available in vendor
func TestEnsureLoadBalancerDeletedSucceedsOnXPN(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	vals.OnXPN = true
	fakeGCE := gce.NewFakeGCECloud(vals)
	c := fakeGCE.Compute().(*cloud.MockGCE)
	svc := test.NewL4ILBService(false, 8080)
	nodeNames := []string{"test-node-1"}
	namer := namer_util.NewNamer(clusterUID, "")
	recorder := record.NewFakeRecorder(100)
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, recorder, &sync.Mutex{}))
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	fwName := l.namer.VMIPNEG(svc.Namespace, svc.Name)
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	c.MockFirewalls.DeleteHook = mock.DeleteFirewallsUnauthorizedErrHook

	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Failed to delete loadBalancer, err %v", err)
	}
	gcloudcmd := gce.FirewallToGCloudDeleteCmd(fwName, fakeGCE.ProjectID())
	XPNErrMsg := fmt.Sprintf("%s %s: `%v`", v1.EventTypeNormal, eventMsgFirewallChange, gcloudcmd)
	err = test.CheckEvent(recorder, XPNErrMsg, true)
	if err != nil {
		t.Errorf("Failed to check event, err %v", err)
	}
}
*/

func TestEnsureInternalLoadBalancerEnableGlobalAccess(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)

	nodeNames := []string{"test-node-1"}
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	frName := l.GetFRName()
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)

	// Change service to include the global access annotation
	svc.Annotations[gce.ServiceAnnotationILBAllowGlobalAccess] = "true"
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)
	betaRuleDescString, err := utils.MakeL4ILBServiceDescription(utils.ServiceKeyFunc(svc.Namespace, svc.Name), "1.2.3.0", meta.VersionBeta)
	if err != nil {
		t.Errorf("Unexpected error when creating description - %v", err)
	}
	key, err := composite.CreateKey(l.cloud, frName, meta.Regional)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}
	fwdRule, err := composite.GetForwardingRule(l.cloud, key, meta.VersionBeta)
	if err != nil {
		t.Errorf("Unexpected error when looking up forwarding rule - %v", err)
	}
	if !fwdRule.AllowGlobalAccess {
		t.Errorf("Unexpected false value for AllowGlobalAccess")
	}
	if fwdRule.Description != betaRuleDescString {
		t.Errorf("Expected description %s, Got %s", betaRuleDescString, fwdRule.Description)
	}
	// remove the annotation and disable global access.
	delete(svc.Annotations, gce.ServiceAnnotationILBAllowGlobalAccess)
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	gaRuleDescString, err := utils.MakeL4ILBServiceDescription(utils.ServiceKeyFunc(svc.Namespace, svc.Name), "1.2.3.0", meta.VersionGA)
	if err != nil {
		t.Errorf("Unexpected error when creating description - %v", err)
	}
	// Fetch the beta version of the rule to make sure GlobalAccess field is off. Calling the GA API will always show
	// this field as false.
	fwdRule, err = composite.GetForwardingRule(l.cloud, key, meta.VersionBeta)
	if err != nil {
		t.Errorf("Unexpected error when looking up forwarding rule - %v", err)
	}

	if fwdRule.AllowGlobalAccess {
		t.Errorf("Unexpected true value for AllowGlobalAccess")
	}
	if fwdRule.Description != gaRuleDescString {
		t.Errorf("Expected description %s, Got %s", gaRuleDescString, fwdRule.Description)
	}
	// Delete the service
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalLoadBalancerDisableGlobalAccess(t *testing.T) {
	t.Parallel()

	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)

	nodeNames := []string{"test-node-1"}
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	svc.Annotations[gce.ServiceAnnotationILBAllowGlobalAccess] = "true"
	frName := l.GetFRName()
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	assertInternalLbResources(t, svc, l, nodeNames)
	key, err := composite.CreateKey(l.cloud, frName, meta.Regional)
	if err != nil {
		t.Errorf("Unexpected error when creating key - %v", err)
	}

	fwdRule, err := composite.GetForwardingRule(l.cloud, key, meta.VersionBeta)
	if err != nil {
		t.Errorf("Unexpected error when looking up forwarding rule - %v", err)
	}
	if !fwdRule.AllowGlobalAccess {
		t.Errorf("Unexpected false value for AllowGlobalAccess")
	}

	// disable global access - setting the annotation to false or removing annotation will disable it
	svc.Annotations[gce.ServiceAnnotationILBAllowGlobalAccess] = "false"
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	fwdRule, err = composite.GetForwardingRule(l.cloud, key, meta.VersionBeta)
	if err != nil {
		t.Errorf("Unexpected error when looking up forwarding rule - %v", err)
	}
	if fwdRule.AllowGlobalAccess {
		t.Errorf("Unexpected 'true' value for AllowGlobalAccess")
	}

	// Delete the service
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalLoadBalancerCustomSubnet(t *testing.T) {
	t.Parallel()
	nodeNames := []string{"test-node-1"}
	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)
	fakeGCE.AlphaFeatureGate = gce.NewAlphaFeatureGate([]string{gce.AlphaFeatureILBCustomSubnet})

	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	status, err := l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}

	frName := l.GetFRName()
	fwdRule, err := composite.GetForwardingRule(l.cloud, meta.RegionalKey(frName, l.cloud.Region()), meta.VersionGA)
	if err != nil || fwdRule == nil {
		t.Errorf("Unexpected error looking up forwarding rule - err %v", err)
	}
	if fwdRule.Subnetwork != "" {
		t.Errorf("Unexpected subnet value %s in ILB ForwardingRule", fwdRule.Subnetwork)
	}

	// Change service to include the global access annotation and request static ip
	requestedIP := "4.5.6.7"
	svc.Annotations[gce.ServiceAnnotationILBSubnet] = "test-subnet"
	svc.Spec.LoadBalancerIP = requestedIP
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	if status.Ingress[0].IP != requestedIP {
		t.Fatalf("Reserved IP %s not propagated, Got '%s'", requestedIP, status.Ingress[0].IP)
	}
	fwdRule, err = composite.GetForwardingRule(l.cloud, meta.RegionalKey(frName, l.cloud.Region()), meta.VersionGA)
	if err != nil || fwdRule == nil {
		t.Errorf("Unexpected error looking up forwarding rule - err %v", err)
	}
	if !strings.HasSuffix(fwdRule.Subnetwork, "test-subnet") {
		t.Errorf("Unexpected subnet value '%s' in ILB ForwardingRule, expected 'test-subnet'", fwdRule.Subnetwork)
	}

	// Change to a different subnet
	svc.Annotations[gce.ServiceAnnotationILBSubnet] = "another-subnet"
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	if status.Ingress[0].IP != requestedIP {
		t.Errorf("Reserved IP %s not propagated, Got %s", requestedIP, status.Ingress[0].IP)
	}
	fwdRule, err = composite.GetForwardingRule(l.cloud, meta.RegionalKey(frName, l.cloud.Region()), meta.VersionGA)
	if err != nil || fwdRule == nil {
		t.Errorf("Unexpected error looking up forwarding rule - err %v", err)
	}
	if !strings.HasSuffix(fwdRule.Subnetwork, "another-subnet") {
		t.Errorf("Unexpected subnet value' %s' in ILB ForwardingRule, expected 'another-subnet'", fwdRule.Subnetwork)
	}
	// remove the annotation - ILB should revert to default subnet.
	delete(svc.Annotations, gce.ServiceAnnotationILBSubnet)
	status, err = l.EnsureInternalLoadBalancer(nodeNames, svc)
	if err != nil {
		t.Errorf("Failed to ensure loadBalancer, err %v", err)
	}
	if len(status.Ingress) == 0 {
		t.Errorf("Got empty loadBalancer status using handler %v", l)
	}
	fwdRule, err = composite.GetForwardingRule(l.cloud, meta.RegionalKey(frName, l.cloud.Region()), meta.VersionGA)
	if err != nil || fwdRule == nil {
		t.Errorf("Unexpected error %v", err)
	}
	if fwdRule.Subnetwork != "" {
		t.Errorf("Unexpected subnet value '%s' in ILB ForwardingRule.", fwdRule.Subnetwork)
	}
	// Delete the loadbalancer
	err = l.EnsureInternalLoadBalancerDeleted(svc)
	if err != nil {
		t.Errorf("Unexpected error deleting loadbalancer - err %v", err)
	}
	assertInternalLbResourcesDeleted(t, svc, true, l)
}

func TestEnsureInternalFirewallPortRanges(t *testing.T) {
	vals := gce.DefaultTestClusterValues()
	fakeGCE := getFakeGCECloud(vals)
	svc := test.NewL4ILBService(false, 8080)
	namer := namer_util.NewNamer(clusterUID, "")
	l := NewL4Handler(svc, fakeGCE, meta.Regional, namer, record.NewFakeRecorder(100), &sync.Mutex{}, fakeMetricsCollector)
	fwName := l.namer.VMIPNEG(l.Service.Namespace, l.Service.Name)
	tc := struct {
		Input  []int
		Result []string
	}{
		Input: []int{15, 37, 900, 2002, 2003, 2003, 2004, 2004}, Result: []string{"15", "37", "900", "2002-2004"},
	}
	c := fakeGCE.Compute().(*cloud.MockGCE)
	c.MockFirewalls.InsertHook = nil
	c.MockFirewalls.UpdateHook = nil

	nodeNames := []string{"test-node-1"}
	_, err := test.CreateAndInsertNodes(l.cloud, nodeNames, vals.ZoneName)
	if err != nil {
		t.Errorf("Unexpected error when adding nodes %v", err)
	}
	c.MockFirewalls.InsertHook = nil
	c.MockFirewalls.UpdateHook = nil

	sourceRange := []string{"10.0.0.0/20"}
	firewalls.EnsureL4InternalFirewallRule(
		l.cloud,
		fwName,
		"1.2.3.4",
		utils.ServiceKeyFunc(svc.Namespace, svc.Name),
		sourceRange,
		utils.GetPortRanges(tc.Input),
		nodeNames,
		string(v1.ProtocolTCP))
	if err != nil {
		t.Errorf("Unexpected error %v when ensuring firewall rule %s for svc %+v", err, fwName, svc)
	}
	existingFirewall, err := l.cloud.GetFirewall(fwName)
	if err != nil || existingFirewall == nil || len(existingFirewall.Allowed) == 0 {
		t.Errorf("Unexpected error %v when looking up firewall %s, Got firewall %+v", err, fwName, existingFirewall)
	}
	existingPorts := existingFirewall.Allowed[0].Ports
	if !reflect.DeepEqual(existingPorts, tc.Result) {
		t.Errorf("Expected firewall rule with ports %v,got %v", tc.Result, existingPorts)
	}
}

func assertInternalLbResources(t *testing.T, apiService *v1.Service, l *L4, nodeNames []string) {
	// Check that Firewalls are created for the LoadBalancer and the HealthCheck
	sharedHC := !servicehelper.RequestsOnlyLocalTraffic(apiService)
	resourceName := l.namer.VMIPNEG(l.Service.Namespace, l.Service.Name)
	resourceDesc, err := utils.MakeL4ILBServiceDescription(utils.ServiceKeyFunc(apiService.Namespace, apiService.Name), "", meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to create description for resources, err %v", err)
	}
	hcName, hcFwName := healthchecks.HealthCheckName(sharedHC, l.namer.UID(), resourceName)
	fwNames := []string{
		resourceName,
		hcFwName,
	}

	for _, fwName := range fwNames {
		firewall, err := l.cloud.GetFirewall(fwName)
		if err != nil {
			t.Fatalf("Failed to fetch firewall rule %s - err %v", fwName, err)
		}
		if !utils.EqualStringSets(nodeNames, firewall.TargetTags) {
			t.Fatalf("Expected firewall rule target tags '%v', Got '%v'", nodeNames, firewall.TargetTags)
		}
		if len(firewall.SourceRanges) == 0 {
			t.Fatalf("Unexpected empty source range for firewall rule %v", firewall)
		}
		if firewall.Description != resourceDesc {
			t.Errorf("Unexpected description in firewall - Expected %s, Got %s", firewall.Description, resourceDesc)
		}
	}

	// Check that HealthCheck is created
	healthcheck, err := composite.GetHealthCheck(l.cloud, meta.GlobalKey(hcName), meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to fetch healthcheck %s - err %v", hcName, err)
	}
	if healthcheck.Name != hcName {
		t.Errorf("Unexpected name for healthcheck '%s' - expected '%s'", healthcheck.Name, hcName)
	}
	// Only non-shared Healthchecks get a description.
	if !sharedHC && healthcheck.Description != resourceDesc {
		t.Errorf("Unexpected description in healthcheck - Expected %s, Got %s", healthcheck.Description, resourceDesc)
	}

	// Check that BackendService exists
	backendServiceName := resourceName
	key := meta.RegionalKey(backendServiceName, l.cloud.Region())
	backendServiceLink := cloud.SelfLink(meta.VersionGA, l.cloud.ProjectID(), "backendServices", key)
	bs, err := composite.GetBackendService(l.cloud, key, meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to fetch backend service %s - err %v", backendServiceName, err)
	}
	if bs.Protocol != "TCP" {
		t.Errorf("Unexpected protocol '%s' for backend service %v", bs.Protocol, bs)
	}
	if bs.SelfLink != backendServiceLink {
		t.Errorf("Unexpected self link in backend service - Expected %s, Got %s", bs.SelfLink, backendServiceLink)
	}
	if bs.Description != resourceDesc {
		t.Errorf("Unexpected description in backend service - Expected %s, Got %s", bs.Description, resourceDesc)
	}
	if !utils.EqualStringSets(bs.HealthChecks, []string{healthcheck.SelfLink}) {
		t.Errorf("Unexpected healthcheck reference '%v' in backend service, expected '%s'", bs.HealthChecks,
			healthcheck.SelfLink)
	}
	// Check that ForwardingRule is created
	frName := l.GetFRName()
	fwdRule, err := composite.GetForwardingRule(l.cloud, meta.RegionalKey(frName, l.cloud.Region()), meta.VersionGA)
	if err != nil {
		t.Errorf("Failed to fetch forwarding rule %s - err %v", frName, err)

	}
	if fwdRule.Name != frName {
		t.Errorf("Unexpected name for forwarding rule '%s' - expected '%s'", fwdRule.Name, frName)
	}
	if fwdRule.IPProtocol != "TCP" {
		t.Errorf("Unexpected protocol '%s' for forwarding rule %v", fwdRule.IPProtocol, fwdRule)
	}
	if fwdRule.BackendService != backendServiceLink {
		t.Errorf("Unexpected backend service link '%s' for forwarding rule, expected '%s'", fwdRule.BackendService, backendServiceLink)
	}
	if fwdRule.Subnetwork != l.cloud.NetworkURL() {
		t.Errorf("Unexpected subnetwork '%s' in forwarding rule, expected '%s'",
			fwdRule.Subnetwork, l.cloud.NetworkURL())
	}
}

func assertInternalLbResourcesDeleted(t *testing.T, apiService *v1.Service, firewallsDeleted bool, l *L4) {
	frName := l.GetFRName()
	sharedHC := !servicehelper.RequestsOnlyLocalTraffic(apiService)
	resourceName := l.namer.VMIPNEG(l.Service.Namespace, l.Service.Name)
	hcName, hcFwName := healthchecks.HealthCheckName(sharedHC, l.namer.UID(), resourceName)

	if firewallsDeleted {
		// Check that Firewalls are deleted for the LoadBalancer and the HealthCheck
		fwNames := []string{
			resourceName,
			hcFwName,
		}

		for _, fwName := range fwNames {
			firewall, err := l.cloud.GetFirewall(fwName)
			if err == nil || firewall != nil {
				t.Errorf("Expected error when looking up firewall rule after deletion")
			}
		}
	}

	// Check forwarding rule is deleted
	fwdRule, err := l.cloud.GetRegionForwardingRule(frName, l.cloud.Region())
	if err == nil || fwdRule != nil {
		t.Errorf("Expected error when looking up forwarding rule after deletion")
	}

	// Check that HealthCheck is deleted
	healthcheck, err := l.cloud.GetHealthCheck(hcName)
	if err == nil || healthcheck != nil {
		t.Errorf("Expected error when looking up healthcheck after deletion")
	}
	bs, err := l.cloud.GetRegionBackendService(resourceName, l.cloud.Region())
	if err == nil || bs != nil {
		t.Errorf("Expected error when looking up backend service after deletion")
	}
}
