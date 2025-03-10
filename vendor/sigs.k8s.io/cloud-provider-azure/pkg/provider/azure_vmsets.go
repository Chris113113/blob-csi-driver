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

package provider

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-02-01/network"
	"github.com/Azure/go-autorest/autorest/azure"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

// VMSet defines functions all vmsets (including scale set and availability
// set) should be implemented.
// Don't forget to run the following command to generate the mock client:
// mockgen -source=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/pkg/provider/azure_vmsets.go -package=provider VMSet > $GOPATH/src/sigs.k8s.io/cloud-provider-azure/pkg/provider/azure_mock_vmsets_test.go
type VMSet interface {
	// GetInstanceIDByNodeName gets the cloud provider ID by node name.
	// It must return ("", cloudprovider.InstanceNotFound) if the instance does
	// not exist or is no longer running.
	GetInstanceIDByNodeName(name string) (string, error)
	// GetInstanceTypeByNodeName gets the instance type by node name.
	GetInstanceTypeByNodeName(name string) (string, error)
	// GetIPByNodeName gets machine private IP and public IP by node name.
	GetIPByNodeName(name string) (string, string, error)
	// GetPrimaryInterface gets machine primary network interface by node name.
	GetPrimaryInterface(nodeName string) (network.Interface, error)
	// GetNodeNameByProviderID gets the node name by provider ID.
	GetNodeNameByProviderID(providerID string) (types.NodeName, error)

	// GetZoneByNodeName gets cloudprovider.Zone by node name.
	GetZoneByNodeName(name string) (cloudprovider.Zone, error)

	// GetPrimaryVMSetName returns the VM set name depending on the configured vmType.
	// It returns config.PrimaryScaleSetName for vmss and config.PrimaryAvailabilitySetName for standard vmType.
	GetPrimaryVMSetName() string
	// GetVMSetNames selects all possible availability sets or scale sets
	// (depending vmType configured) for service load balancer, if the service has
	// no loadbalancer mode annotation returns the primary VMSet. If service annotation
	// for loadbalancer exists then return the eligible VMSet.
	GetVMSetNames(service *v1.Service, nodes []*v1.Node) (availabilitySetNames *[]string, err error)
	// EnsureHostsInPool ensures the given Node's primary IP configurations are
	// participating in the specified LoadBalancer Backend Pool.
	EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetName string, isInternal bool) error
	// EnsureHostInPool ensures the given VM's Primary NIC's Primary IP Configuration is
	// participating in the specified LoadBalancer Backend Pool.
	EnsureHostInPool(service *v1.Service, nodeName types.NodeName, backendPoolID string, vmSetName string, isInternal bool) (string, string, string, *compute.VirtualMachineScaleSetVM, error)
	// EnsureBackendPoolDeleted ensures the loadBalancer backendAddressPools deleted from the specified nodes.
	EnsureBackendPoolDeleted(service *v1.Service, backendPoolID, vmSetName string, backendAddressPools *[]network.BackendAddressPool) error
	//EnsureBackendPoolDeletedFromVMSets ensures the loadBalancer backendAddressPools deleted from the specified VMSS/VMAS
	EnsureBackendPoolDeletedFromVMSets(vmSetNamesMap map[string]bool, backendPoolID string) error

	// AttachDisk attaches a disk to vm
	AttachDisk(nodeName types.NodeName, diskMap map[string]*AttachDiskOptions) (*azure.Future, error)
	// DetachDisk detaches a disk from vm
	DetachDisk(nodeName types.NodeName, diskMap map[string]string) error
	// WaitForUpdateResult waits for the response of the update request
	WaitForUpdateResult(ctx context.Context, future *azure.Future, resourceGroupName, source string) error

	// GetDataDisks gets a list of data disks attached to the node.
	GetDataDisks(nodeName types.NodeName, crt azcache.AzureCacheReadType) ([]compute.DataDisk, *string, error)

	// UpdateVM updates a vm
	UpdateVM(nodeName types.NodeName) error

	// GetPowerStatusByNodeName returns the powerState for the specified node.
	GetPowerStatusByNodeName(name string) (string, error)

	// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
	GetProvisioningStateByNodeName(name string) (string, error)

	// GetPrivateIPsByNodeName returns a slice of all private ips assigned to node (ipv6 and ipv4)
	GetPrivateIPsByNodeName(name string) ([]string, error)

	// GetNodeNameByIPConfigurationID gets the nodeName and vmSetName by IP configuration ID.
	GetNodeNameByIPConfigurationID(ipConfigurationID string) (string, string, error)

	// GetNodeCIDRMasksByProviderID returns the node CIDR subnet mask by provider ID.
	GetNodeCIDRMasksByProviderID(providerID string) (int, int, error)

	// GetAgentPoolVMSetNames returns all vmSet names according to the nodes
	GetAgentPoolVMSetNames(nodes []*v1.Node) (*[]string, error)
}
