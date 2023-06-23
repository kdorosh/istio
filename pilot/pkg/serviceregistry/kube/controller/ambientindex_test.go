// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	meshconfig "istio.io/api/mesh/v1alpha1"
	auth "istio.io/api/security/v1beta1"
	"istio.io/api/type/v1beta1"
	"istio.io/istio/pilot/pkg/config/kube/crd"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pilot/test/util"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/file"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/util/protomarshal"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/pkg/workloadapi"
	"istio.io/istio/pkg/workloadapi/security"
)

func TestAmbientIndex(t *testing.T) {
	test.SetForTest(t, &features.EnableAmbientControllers, true)
	cfg := memory.NewSyncController(memory.MakeSkipValidation(collections.PilotGatewayAPI()))
	controller, fx := NewFakeControllerWithOptions(t, FakeControllerOptions{
		ConfigController: cfg,
		MeshWatcher:      mesh.NewFixedWatcher(&meshconfig.MeshConfig{RootNamespace: "istio-system"}),
		ClusterID:        "cluster0",
	})
	controller.network = "testnetwork"
	pc := clienttest.Wrap(t, controller.podsClient)
	sc := clienttest.Wrap(t, controller.services)
	cfg.RegisterEventHandler(gvk.AuthorizationPolicy, controller.AuthorizationPolicyHandler)
	cfg.RegisterEventHandler(gvk.WorkloadEntry, controller.WorkloadEntryHandler)
	cfg.RegisterEventHandler(gvk.PeerAuthentication, controller.PeerAuthenticationHandler)
	go cfg.Run(test.NewStop(t))

	addPod(t, pc, "127.0.0.1", "name1", "sa1", map[string]string{"app": "a"}, nil)
	assertAddresses(t, controller, "", "name1")
	assertEvent(t, fx, "cluster0//Pod/ns1/name1")

	addPod(t, pc, "127.0.0.2", "name2", "sa1", map[string]string{"app": "a", "other": "label"}, nil)
	addPod(t, pc, "127.0.0.3", "name3", "sa1", map[string]string{"app": "other"}, nil)
	assertAddresses(t, controller, "", "name1", "name2", "name3")
	assertAddresses(t, controller, "testnetwork/127.0.0.1", "name1")
	assertAddresses(t, controller, "testnetwork/127.0.0.2", "name2")
	for _, key := range []string{"cluster0//Pod/ns1/name3", "testnetwork/127.0.0.3"} {
		assert.Equal(t, controller.ambientIndex.Lookup(key), []*model.AddressInfo{
			{
				Address: &workloadapi.Address{
					Type: &workloadapi.Address_Workload{
						Workload: &workloadapi.Workload{
							Name:              "name3",
							Namespace:         "ns1",
							Addresses:         [][]byte{netip.MustParseAddr("127.0.0.3").AsSlice()},
							Network:           "testnetwork",
							ServiceAccount:    "sa1",
							Uid:               "cluster0//Pod/ns1/name3",
							Node:              "node1",
							CanonicalName:     "other",
							CanonicalRevision: "latest",
							WorkloadType:      workloadapi.WorkloadType_POD,
							WorkloadName:      "name3",
							ClusterId:         "cluster0",
							Status:            workloadapi.WorkloadStatus_HEALTHY,
						},
					},
				},
			},
		})
	}
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	assertEvent(t, fx, "cluster0//Pod/ns1/name3")

	// Non-existent IP should have no response
	assertAddresses(t, controller, "testnetwork/10.0.0.1")
	fx.Clear()

	addService(t, sc, "svc1",
		map[string]string{},
		map[string]string{},
		[]int32{80}, map[string]string{"app": "a"}, "10.0.0.1")
	// Services should appear with workloads
	assertAddresses(t, controller, "", "name1", "name2", "name3", "svc1")
	assertAddresses(t, controller, "testnetwork/127.0.0.1", "name1")
	// Now we should be able to look up a VIP as well
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name1", "name2", "svc1")
	// We should get an event for the new Service and the two *Pod* IPs impacted
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "ns1/svc1.ns1.svc.company.com")

	// Add a new pod to the service, we should see it
	addPod(t, pc, "127.0.0.4", "name4", "sa1", map[string]string{"app": "a"}, nil)
	assertAddresses(t, controller, "", "name1", "name2", "name3", "name4", "svc1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name1", "name2", "name4", "svc1")
	assertEvent(t, fx, "cluster0//Pod/ns1/name4")

	// Delete it, should remove from the Service as well
	deletePod(t, pc, "name4")
	assertAddresses(t, controller, "", "name1", "name2", "name3", "svc1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name1", "name2", "svc1")
	assertAddresses(t, controller, "testnetwork/127.0.0.4") // Should not be accessible anymore
	assertAddresses(t, controller, "cluster0//Pod/ns1/name4")
	assertEvent(t, fx, "cluster0//Pod/ns1/name4")

	fx.Clear()
	// Update Service to have a more restrictive label selector
	addService(t, sc, "svc1",
		map[string]string{},
		map[string]string{},
		[]int32{80}, map[string]string{"app": "a", "other": "label"}, "10.0.0.1")
	assertAddresses(t, controller, "", "name1", "name2", "name3", "svc1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name2", "svc1")
	// Need to update the *old* workload only
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "ns1/svc1.ns1.svc.company.com")
	// assertEvent("cluster0//v1/pod/ns1/name1") TODO: This should be the event, but we are not efficient here.

	// Update an existing pod into the service
	addPod(t, pc, "127.0.0.3", "name3", "sa1", map[string]string{"app": "a", "other": "label"}, nil)
	assertAddresses(t, controller, "", "name1", "name2", "name3", "svc1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name2", "name3", "svc1")
	assertEvent(t, fx, "cluster0//Pod/ns1/name3")

	// And remove it again
	addPod(t, pc, "127.0.0.3", "name3", "sa1", map[string]string{"app": "a"}, nil)
	assertAddresses(t, controller, "", "name1", "name2", "name3", "svc1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name2", "svc1")
	assertEvent(t, fx, "cluster0//Pod/ns1/name3")

	// Delete the service entirely
	deleteService(t, sc, "svc1")
	assertAddresses(t, controller, "", "name1", "name2", "name3")
	assertAddresses(t, controller, "testnetwork/10.0.0.1")
	assertEvent(t, fx, "cluster0//Pod/ns1/name2", "ns1/svc1.ns1.svc.company.com")
	assert.Equal(t, len(controller.ambientIndex.byService), 0)

	// Add a waypoint proxy pod for namespace
	addPod(t, pc, "127.0.0.200", "waypoint-ns-pod", "namespace-wide",
		map[string]string{
			constants.ManagedGatewayLabel: constants.ManagedGatewayMeshControllerLabel,
			constants.GatewayNameLabel:    "namespace-wide",
		}, nil)
	assertAddresses(t, controller, "", "name1", "name2", "name3", "waypoint-ns-pod")
	assertEvent(t, fx, "cluster0//Pod/ns1/waypoint-ns-pod")
	// create the waypoint service
	addService(t, sc, "waypoint-ns",
		map[string]string{constants.ManagedGatewayLabel: constants.ManagedGatewayMeshControllerLabel},
		map[string]string{},
		[]int32{80}, map[string]string{constants.GatewayNameLabel: "namespace-wide"}, "10.0.0.2")
	assertAddresses(t, controller, "", "name1", "name2", "name3", "waypoint-ns", "waypoint-ns-pod")
	// All these workloads updated, so push them
	assertEvent(t, fx, "cluster0//Pod/ns1/name1",
		"cluster0//Pod/ns1/name2",
		"cluster0//Pod/ns1/name3",
		"cluster0//Pod/ns1/waypoint-ns-pod",
		"ns1/waypoint-ns.ns1.svc.company.com",
	)
	// We should now see the waypoint service IP
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.3")[0].Address.GetWorkload().Waypoint.GetAddress().Address,
		netip.MustParseAddr("10.0.0.2").AsSlice())

	// Lookup for service IP should return Workload and Service AddressInfo objects
	assert.Equal(t,
		len(controller.ambientIndex.Lookup("testnetwork/10.0.0.2")),
		2)
	for _, k := range controller.ambientIndex.Lookup("testnetwork/10.0.0.2") {
		switch k.Type.(type) {
		case *workloadapi.Address_Workload:
			assert.Equal(t, k.Address.GetWorkload().Name, "waypoint-ns-pod")
			assert.Equal(t, k.Address.GetWorkload().Waypoint, nil)
		case *workloadapi.Address_Service:
			assert.Equal(t, k.Address.GetService().Name, "waypoint-ns")
		}
	}
	// Lookup for service via namespace/hostname returns Service and Workload AddressInfo
	assert.Equal(t,
		len(controller.ambientIndex.Lookup("ns1/waypoint-ns.ns1.svc.company.com")), 2)
	for _, k := range controller.ambientIndex.Lookup("ns1/waypoint-ns.ns1.svc.company.com") {
		switch k.Type.(type) {
		case *workloadapi.Address_Workload:
			assert.Equal(t, k.Address.GetWorkload().Name, "waypoint-ns-pod")
			assert.Equal(t, k.Address.GetWorkload().Waypoint, nil)
		case *workloadapi.Address_Service:
			assert.Equal(t, k.Address.GetService().Hostname, "waypoint-ns.ns1.svc.company.com")
		}
	}

	// Add another waypoint pod, expect no updates for other pods since waypoint address refers to service IP
	addPod(t, pc, "127.0.0.201", "waypoint2-ns-pod", "namespace-wide",
		map[string]string{
			constants.ManagedGatewayLabel: constants.ManagedGatewayMeshControllerLabel,
			constants.GatewayNameLabel:    "namespace-wide",
		}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/waypoint2-ns-pod")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.3")[0].Address.GetWorkload().Waypoint.GetAddress().Address, netip.MustParseAddr("10.0.0.2").AsSlice())
	// Waypoints do not have waypoints
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.200")[0].Address.GetWorkload().Waypoint,
		nil)
	assert.Equal(t, len(controller.Waypoint(model.WaypointScope{Namespace: "ns1", ServiceAccount: "namespace-wide"})), 1)
	for _, k := range controller.Waypoint(model.WaypointScope{Namespace: "ns1", ServiceAccount: "namespace-wide"}) {
		assert.Equal(t, k.AsSlice(), netip.MustParseAddr("10.0.0.2").AsSlice())
	}
	addService(t, sc, "svc1",
		map[string]string{},
		map[string]string{},
		[]int32{80}, map[string]string{"app": "a"}, "10.0.0.1")
	assertAddresses(t, controller, "testnetwork/10.0.0.1", "name1", "name2", "name3", "svc1")
	// Send update for the workloads as well...
	assertEvent(t, fx, "cluster0//Pod/ns1/name1",
		"cluster0//Pod/ns1/name2",
		"cluster0//Pod/ns1/name3",
		"ns1/svc1.ns1.svc.company.com",
	)
	// Make sure Service sees waypoints as well
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/10.0.0.1")[0].Address.GetWorkload().Waypoint.GetAddress().Address, netip.MustParseAddr("10.0.0.2").AsSlice())

	// Delete a waypoint
	deletePod(t, pc, "waypoint2-ns-pod")
	assertEvent(t, fx, "cluster0//Pod/ns1/waypoint2-ns-pod")
	// Workload should not be updated since service has not changed
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.3")[0].Address.GetWorkload().Waypoint.GetAddress().Address,
		netip.MustParseAddr("10.0.0.2").AsSlice())
	// As should workload via Service
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/10.0.0.1")[0].Address.GetWorkload().Waypoint.GetAddress().Address,
		netip.MustParseAddr("10.0.0.2").AsSlice())

	addPod(t, pc, "127.0.0.201", "waypoint2-sa", "waypoint-sa",
		map[string]string{constants.ManagedGatewayLabel: constants.ManagedGatewayMeshControllerLabel},
		map[string]string{constants.WaypointServiceAccount: "sa2"})
	assertEvent(t, fx, "cluster0//Pod/ns1/waypoint2-sa")
	// Unrelated SA should not change anything
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.3")[0].Address.GetWorkload().Waypoint.GetAddress().Address,
		netip.MustParseAddr("10.0.0.2").AsSlice())

	// Adding a new pod should also see the waypoint
	addPod(t, pc, "127.0.0.6", "name6", "sa1", map[string]string{"app": "a"}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name6")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.6")[0].Address.GetWorkload().Waypoint.GetAddress().Address,
		netip.MustParseAddr("10.0.0.2").AsSlice())

	deletePod(t, pc, "name6")
	assertEvent(t, fx, "cluster0//Pod/ns1/name6")

	deletePod(t, pc, "name3")
	assertEvent(t, fx, "cluster0//Pod/ns1/name3")
	deletePod(t, pc, "name2")
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")

	deleteService(t, sc, "waypoint-ns")
	assertEvent(t, fx, "cluster0//Pod/ns1/name1",
		"cluster0//Pod/ns1/waypoint-ns-pod",
		"ns1/waypoint-ns.ns1.svc.company.com",
	)

	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/10.0.0.1")[0].Address.GetWorkload().Waypoint,
		nil)

	// Test that PeerAuthentications are added to the ambient index
	addPolicy(t, cfg, "global", "istio-system", nil, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
	})
	addPolicy(t, cfg, "namespace", "ns1", nil, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})

	// Should add the static policy to all pods in the ns1 namespace since the effective mode is STRICT
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/waypoint-ns-pod", "cluster0//Pod/ns1/waypoint2-sa")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})
	fx.Clear()

	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})
	// Expect no event since the effective policy doesn't change
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Change the workload policy to be permissive
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1") // Static policy should be removed since it isn't STRICT
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)

	// Add a port-level STRICT exception to the workload policy
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_STRICT,
			},
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1") // Selector policy should be added back since there is now a STRICT exception
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Pod not in selector policy, but namespace policy should take effect (hence static policy)
	addPod(t, pc, "127.0.0.2", "name2", "sa1", map[string]string{"app": "not-a"}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.2")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Add it to the policy by updating its selector
	addPod(t, pc, "127.0.0.2", "name2", "sa1", map[string]string{"app": "a"}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.2")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Add global selector policy; nothing should happen since PeerAuthentication doesn't support global mesh wide selectors
	addPolicy(t, cfg, "global-selector", "istio-system", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Delete global selector policy
	cfg.Delete(gvk.PeerAuthentication, "global-selector", "istio-system", nil)

	// Update workload policy to be PERMISSIVE
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
			},
		}
	})
	// There should be an event since effective policy moves to PERMISSIVE
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)

	// Change namespace policy to be PERMISSIVE
	addPolicy(t, cfg, "namespace", "ns1", nil, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
	})

	// All pods have an event (since we're only testing one namespace) but still no policies attached
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "cluster0//Pod/ns1/waypoint-ns-pod", "cluster0//Pod/ns1/waypoint2-sa")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)

	// Change workload policy to be STRICT and remove port-level overrides
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
		pol.PortLevelMtls = nil
	})

	// Selected pods receive an event
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)}) // Effective mode is STRICT so set policy

	// Add a permissive port-level override
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
			},
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching pods receive an event
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Set workload policy to be UNSET with a STRICT port-level override
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = nil // equivalent to UNSET
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_STRICT,
			},
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching pods receive an event
	// The policy should still be added since the effective policy is PERMISSIVE
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Change namespace policy back to STRICT
	addPolicy(t, cfg, "namespace", "ns1", nil, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})
	// All pods have an event (since we're only testing one namespace)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "cluster0//Pod/ns1/waypoint-ns-pod", "cluster0//Pod/ns1/waypoint2-sa")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)}) // Effective mode is STRICT so set static policy

	// Set workload policy to be UNSET with a PERMISSIVE port-level override
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = nil // equivalent to UNSET
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
			},
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching pods receive an event
	// The policy should still be added since the effective policy is STRICT
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName), fmt.Sprintf("ns1/%sselector", convertedPeerAuthenticationPrefix)})

	// Clear PeerAuthentication from workload
	cfg.Delete(gvk.PeerAuthentication, "selector", "ns1", nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")
	// Effective policy is still STRICT so the static policy should still be set
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Now remove the namespace and global policies along with the pods
	cfg.Delete(gvk.PeerAuthentication, "namespace", "ns1", nil)
	cfg.Delete(gvk.PeerAuthentication, "global", "istio-system", nil)
	deletePod(t, pc, "name2")
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	fx.Clear()

	// Test AuthorizationPolicies
	addPolicy(t, cfg, "global", "istio-system", nil, gvk.AuthorizationPolicy, nil)
	addPolicy(t, cfg, "namespace", "ns1", nil, gvk.AuthorizationPolicy, nil)
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)

	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.AuthorizationPolicy, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector"})

	// Pod not in policy
	addPod(t, pc, "127.0.0.2", "name2", "sa1", map[string]string{"app": "not-a"}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.2")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)

	// Add it to the policy by updating its selector
	addPod(t, pc, "127.0.0.2", "name2", "sa1", map[string]string{"app": "a"}, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.2")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector"})

	addPolicy(t, cfg, "global-selector", "istio-system", map[string]string{"app": "a"}, gvk.AuthorizationPolicy, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")

	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"istio-system/global-selector", "ns1/selector"})

	// Update selector to not select
	addPolicy(t, cfg, "global-selector", "istio-system", map[string]string{"app": "not-a"}, gvk.AuthorizationPolicy, nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")

	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector"})

	// Add STRICT global PeerAuthentication
	addPolicy(t, cfg, "strict", "istio-system", nil, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})
	// Every workload should receive an event
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "cluster0//Pod/ns1/waypoint-ns-pod", "cluster0//Pod/ns1/waypoint2-sa")
	// Static STRICT policy should be sent
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector", fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Now add a STRICT workload PeerAuthentication
	addPolicy(t, cfg, "selector-strict", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching workloads should receive an event
	// Effective policy is still STRICT so only static policy should be referenced
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector", fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Change the workload policy to PERMISSIVE
	addPolicy(t, cfg, "selector-strict", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching workloads should receive an event
	// Static STRICT policy should disappear
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector"})

	// Now make the workload policy STRICT but have a PERMISSIVE port-level override
	addPolicy(t, cfg, "selector-strict", "ns1", map[string]string{"app": "a"}, gvk.PeerAuthentication, func(c *config.Config) {
		pol := c.Spec.(*auth.PeerAuthentication)
		pol.Mtls = &auth.PeerAuthentication_MutualTLS{
			Mode: auth.PeerAuthentication_MutualTLS_STRICT,
		}
		pol.PortLevelMtls = map[uint32]*auth.PeerAuthentication_MutualTLS{
			9090: {
				Mode: auth.PeerAuthentication_MutualTLS_PERMISSIVE,
			},
		}
	})
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching workloads should receive an event
	// Workload policy should be added since there's a port level exclusion
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector", fmt.Sprintf("ns1/%sselector-strict", convertedPeerAuthenticationPrefix)})

	// Now add a rule allowing a specific source principal to the workload AuthorizationPolicy
	addPolicy(t, cfg, "selector", "ns1", map[string]string{"app": "a"}, gvk.AuthorizationPolicy, func(c *config.Config) {
		pol := c.Spec.(*auth.AuthorizationPolicy)
		pol.Rules = []*auth.Rule{
			{
				From: []*auth.Rule_From{{Source: &auth.Source{Principals: []string{"cluster.local/ns/ns1/sa/sa1"}}}},
			},
		}
	})
	// No event since workload policy should still be there (both workloads' policy references remain the same).
	// Since PeerAuthentications are translated into DENY policies we can safely apply them
	// alongside ALLOW authorization policies
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{"ns1/selector", fmt.Sprintf("ns1/%sselector-strict", convertedPeerAuthenticationPrefix)})

	cfg.Delete(gvk.AuthorizationPolicy, "selector", "ns1", nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2")
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("ns1/%sselector-strict", convertedPeerAuthenticationPrefix)})

	// Delete selector policy
	cfg.Delete(gvk.PeerAuthentication, "selector-strict", "ns1", nil)
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2") // Matching workloads should receive an event
	// Static STRICT policy should now be sent because of the global policy
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		[]string{fmt.Sprintf("istio-system/%s", staticStrictPolicyName)})

	// Delete global policy
	cfg.Delete(gvk.PeerAuthentication, "strict", "istio-system", nil)
	// Every workload should receive an event
	assertEvent(t, fx, "cluster0//Pod/ns1/name1", "cluster0//Pod/ns1/name2", "cluster0//Pod/ns1/waypoint-ns-pod", "cluster0//Pod/ns1/waypoint2-sa")
	// Now no policies are in effect
	assert.Equal(t,
		controller.ambientIndex.Lookup("testnetwork/127.0.0.1")[0].Address.GetWorkload().AuthorizationPolicies,
		nil)
}

func TestPodLifecycleWorkloadGates(t *testing.T) {
	test.SetForTest(t, &features.EnableAmbientControllers, true)
	cfg := memory.NewSyncController(memory.MakeSkipValidation(collections.PilotGatewayAPI()))
	controller, fx := NewFakeControllerWithOptions(t, FakeControllerOptions{
		ConfigController: cfg,
		MeshWatcher:      mesh.NewFixedWatcher(&meshconfig.MeshConfig{RootNamespace: "istio-system"}),
	})
	pc := clienttest.Wrap(t, controller.podsClient)
	cfg.RegisterEventHandler(gvk.AuthorizationPolicy, controller.AuthorizationPolicyHandler)
	go cfg.Run(test.NewStop(t))
	assertWorkloads := func(lookup string, state workloadapi.WorkloadStatus, names ...string) {
		t.Helper()
		want := sets.New(names...)
		assert.EventuallyEqual(t, func() sets.String {
			var workloads []*model.AddressInfo
			if lookup == "" {
				workloads = controller.ambientIndex.All()
			} else {
				workloads = controller.ambientIndex.Lookup(lookup)
			}
			have := sets.New[string]()
			for _, wl := range workloads {
				switch addr := wl.Address.Type.(type) {
				case *workloadapi.Address_Workload:
					if addr.Workload.Status == state {
						have.Insert(addr.Workload.Name)
					}
				}
			}
			return have
		}, want, retry.Timeout(time.Second*3))
	}
	addPods := func(ip string, name, sa string, labels map[string]string, markReady bool, phase corev1.PodPhase) {
		t.Helper()
		pod := generatePod(ip, name, "ns1", sa, "node1", labels, nil)

		p := pc.Get(name, pod.Namespace)
		if p == nil {
			// Apiserver doesn't allow Create to modify the pod status; in real world its a 2 part process
			pod.Status = corev1.PodStatus{}
			newPod := pc.Create(pod)
			if markReady {
				setPodReady(newPod)
			}
			newPod.Status.PodIP = ip
			newPod.Status.Phase = phase
			newPod.Status.PodIPs = []corev1.PodIP{
				{
					IP: ip,
				},
			}
			pc.UpdateStatus(newPod)
		} else {
			pc.Update(pod)
		}
	}

	addPods("127.0.0.1", "name1", "sa1", map[string]string{"app": "a"}, true, corev1.PodRunning)
	assertEvent(t, fx, "//Pod/ns1/name1")
	assertWorkloads("", workloadapi.WorkloadStatus_HEALTHY, "name1")

	addPods("127.0.0.2", "name2", "sa1", map[string]string{"app": "a", "other": "label"}, false, corev1.PodRunning)
	addPods("127.0.0.3", "name3", "sa1", map[string]string{"app": "other"}, false, corev1.PodPending)
	assertEvent(t, fx, "//Pod/ns1/name2")
	// Still healthy
	assertWorkloads("", workloadapi.WorkloadStatus_HEALTHY, "name1")
	// Unhealthy
	assertWorkloads("", workloadapi.WorkloadStatus_UNHEALTHY, "name2")
	// name3 isn't running at all
}

func TestRBACConvert(t *testing.T) {
	files := file.ReadDirOrFail(t, "testdata")
	if len(files) == 0 {
		// Just in case
		t.Fatal("expected test cases")
	}
	for _, f := range files {
		name := filepath.Base(f)
		if !strings.Contains(name, "-in.yaml") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			pol, _, err := crd.ParseInputs(file.AsStringOrFail(t, f))
			assert.NoError(t, err)
			var o *security.Authorization
			switch pol[0].GroupVersionKind {
			case gvk.AuthorizationPolicy:
				o = convertAuthorizationPolicy("istio-system", pol[0])
			case gvk.PeerAuthentication:
				o = convertPeerAuthentication("istio-system", pol[0])
			default:
				t.Fatalf("unknown kind %v", pol[0].GroupVersionKind)
			}
			msg := ""
			if o != nil {
				msg, err = protomarshal.ToYAML(o)
				assert.NoError(t, err)
			}
			golden := filepath.Join("testdata", strings.ReplaceAll(name, "-in", ""))
			util.CompareContent(t, []byte(msg), golden)
		})
	}
}

func addPolicy(t *testing.T, cfg *memory.Controller, name, ns string, selector map[string]string, kind config.GroupVersionKind, modify func(*config.Config)) {
	t.Helper()
	var sel *v1beta1.WorkloadSelector
	if selector != nil {
		sel = &v1beta1.WorkloadSelector{
			MatchLabels: selector,
		}
	}
	p := config.Config{
		Meta: config.Meta{
			GroupVersionKind: kind,
			Name:             name,
			Namespace:        ns,
		},
	}
	switch kind {
	case gvk.AuthorizationPolicy:
		p.Spec = &auth.AuthorizationPolicy{
			Selector: sel,
		}
	case gvk.PeerAuthentication:
		p.Spec = &auth.PeerAuthentication{
			Selector: sel,
		}
	}

	if modify != nil {
		modify(&p)
	}

	_, err := cfg.Create(p)
	if err != nil && strings.Contains(err.Error(), "item already exists") {
		_, err = cfg.Update(p)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertAddresses(t *testing.T, controller *FakeController, lookup string, names ...string) {
	t.Helper()
	want := sets.New(names...)
	assert.EventuallyEqual(t, func() sets.String {
		var addresses []*model.AddressInfo
		if lookup == "" {
			addresses = controller.ambientIndex.All()
		} else {
			addresses = controller.ambientIndex.Lookup(lookup)
		}
		have := sets.New[string]()
		for _, address := range addresses {
			switch addr := address.Address.Type.(type) {
			case *workloadapi.Address_Workload:
				have.Insert(addr.Workload.Name)
			case *workloadapi.Address_Service:
				have.Insert(addr.Service.Name)
			}
		}
		return have
	}, want, retry.Timeout(time.Second*3))
}

func deletePod(t *testing.T, pc clienttest.TestClient[*corev1.Pod], name string) {
	t.Helper()
	pc.Delete(name, "ns1")
}

func assertEvent(t *testing.T, fx *xdsfake.Updater, ip ...string) {
	t.Helper()
	want := strings.Join(ip, ",")
	fx.MatchOrFail(t, xdsfake.Event{Type: "xds", ID: want})
}

func deleteService(t *testing.T, sc clienttest.TestClient[*corev1.Service], name string) {
	t.Helper()
	sc.Delete(name, "ns1")
}

func addService(t *testing.T, sc clienttest.TestClient[*corev1.Service], name string, labels, annotations map[string]string,
	ports []int32, selector map[string]string, ip string,
) {
	t.Helper()
	service := generateService(name, "ns1", labels, annotations, ports, selector, ip)
	sc.CreateOrUpdate(service)
}

func assertWorkloads(t *testing.T, controller *FakeController, lookup string, state workloadapi.WorkloadStatus, names ...string) {
	t.Helper()
	want := sets.New(names...)
	assert.EventuallyEqual(t, func() sets.String {
		var workloads []*model.AddressInfo
		if lookup == "" {
			workloads = controller.ambientIndex.All()
		} else {
			workloads = controller.ambientIndex.Lookup(lookup)
		}
		have := sets.New[string]()
		for _, wl := range workloads {
			switch addr := wl.Address.Type.(type) {
			case *workloadapi.Address_Workload:
				if addr.Workload.Status == state {
					have.Insert(addr.Workload.Name)
				}
			}
		}
		return have
	}, want, retry.Timeout(time.Second*3))
}

func addPod(t *testing.T, pc clienttest.TestClient[*corev1.Pod], ip string, name, sa string, labels map[string]string, annotations map[string]string) {
	t.Helper()
	pod := generatePod(ip, name, "ns1", sa, "node1", labels, annotations)

	p := pc.Get(name, pod.Namespace)
	if p == nil {
		// Apiserver doesn't allow Create to modify the pod status; in real world its a 2 part process
		pod.Status = corev1.PodStatus{}
		newPod := pc.Create(pod)
		setPodReady(newPod)
		newPod.Status.PodIP = ip
		newPod.Status.PodIPs = []corev1.PodIP{
			{
				IP: ip,
			},
		}
		newPod.Status.Phase = corev1.PodRunning
		pc.UpdateStatus(newPod)
	} else {
		pc.Update(pod)
	}
}
