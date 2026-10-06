// modules/region.bicep — One region's complete worker setup:
//   • VNet + subnet
//   • NSG with conservative outbound rules
//   • Egress: per-VM Public IP (each VM gets a Standard SKU IPv4 address;
//     no NAT Gateway — see docs/AGENTS.md §1 for the rationale)
//   • VMSS with cloud-init and UAMI attached
//
// Invoked once per region by:
//   • fleet.bicep              — initial deploy of every requested region
//   • fleet-add-region.bicep   — single-region scale-out at runtime

targetScope = 'resourceGroup'

@description('Azure region (must match the location of the VMSS instances).')
param location string

@description('8-char run suffix from the Go controller.')
param runSuffix string

@description('VM SKU (F4s_v2 or F8s_v2).')
param vmSize string

@description('Number of VMs in this VMSS.')
@minValue(1)
@maxValue(200)
param capacity int

@description('Use spot pricing.')
param useSpot bool

@description('Admin username.')
param adminUsername string

@description('SSH public key.')
@secure()
param sshPublicKey string

@description('Base64-encoded cloud-init script (passed as customData).')
@secure()
param customDataBase64 string

@description('Full resource ID of the UAMI.')
param uamiResourceId string

@description('Client ID of the UAMI — useful for IMDS token requests in cloud-init.')
param uamiClientId string

@description('Tags applied to every resource in this region.')
param tags object

// ---------------------------------------------------------------------------
// NSG — restrictive outbound; default inbound deny.
// ---------------------------------------------------------------------------

resource nsg 'Microsoft.Network/networkSecurityGroups@2024-01-01' = {
  name: 'ddos-${runSuffix}-${location}-nsg'
  location: location
  tags: tags
  properties: {
    securityRules: [
      {
        name: 'AllowAzureMonitorOut'
        properties: {
          priority: 100
          direction: 'Outbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          sourceAddressPrefix: '*'
          destinationPortRange: '443'
          destinationAddressPrefix: 'AzureMonitor'
        }
      }
      {
        name: 'AllowStorageOut'
        properties: {
          priority: 110
          direction: 'Outbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          sourceAddressPrefix: '*'
          destinationPortRange: '443'
          destinationAddressPrefix: 'Storage'
        }
      }
      {
        name: 'AllowEntraOut'
        properties: {
          priority: 120
          direction: 'Outbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          sourceAddressPrefix: '*'
          destinationPortRange: '443'
          destinationAddressPrefix: 'AzureActiveDirectory'
        }
      }
      {
        name: 'AllowTargetsOut'
        properties: {
          priority: 200
          direction: 'Outbound'
          access: 'Allow'
          protocol: 'Tcp'
          sourcePortRange: '*'
          sourceAddressPrefix: '*'
          destinationPortRanges: [ '80', '443' ]
          destinationAddressPrefix: 'Internet'
        }
      }
      {
        name: 'DenyAllOtherOutbound'
        properties: {
          priority: 4000
          direction: 'Outbound'
          access: 'Deny'
          protocol: '*'
          sourcePortRange: '*'
          sourceAddressPrefix: '*'
          destinationPortRange: '*'
          destinationAddressPrefix: '*'
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// VNet + subnet (per-VM public IPs are attached at VMSS ipConfig level)
// ---------------------------------------------------------------------------

resource vnet 'Microsoft.Network/virtualNetworks@2024-01-01' = {
  name: 'ddos-${runSuffix}-${location}-vnet'
  location: location
  tags: tags
  properties: {
    addressSpace: {
      addressPrefixes: [ '10.42.0.0/16' ]
    }
    subnets: [
      {
        name: 'workers'
        properties: {
          addressPrefix: '10.42.1.0/24'
          networkSecurityGroup: {
            id: nsg.id
          }
        }
      }
    ]
  }
}

// ---------------------------------------------------------------------------
// VMSS
// ---------------------------------------------------------------------------

resource vmss 'Microsoft.Compute/virtualMachineScaleSets@2024-07-01' = {
  name: 'ddos-${runSuffix}-${location}-vmss'
  location: location
  tags: tags
  sku: {
    name: vmSize
    tier: 'Standard'
    capacity: capacity
  }
  identity: {
    type: 'UserAssigned'
    userAssignedIdentities: {
      '${uamiResourceId}': {}
    }
  }
  properties: {
    overprovision: false
    singlePlacementGroup: false
    platformFaultDomainCount: 1
    upgradePolicy: {
      mode: 'Manual'
    }
    orchestrationMode: 'Uniform'
    virtualMachineProfile: {
      priority: useSpot ? 'Spot' : 'Regular'
      evictionPolicy: useSpot ? 'Delete' : null
      billingProfile: useSpot ? {
        maxPrice: -1 // cap at on-demand, never auto-evict due to price
      } : null
      osProfile: {
        computerNamePrefix: take('ddos-${runSuffix}-${location}', 9)
        adminUsername: adminUsername
        linuxConfiguration: {
          disablePasswordAuthentication: true
          ssh: {
            publicKeys: [
              {
                path: '/home/${adminUsername}/.ssh/authorized_keys'
                keyData: sshPublicKey
              }
            ]
          }
          provisionVMAgent: true
        }
        customData: customDataBase64
      }
      storageProfile: {
        imageReference: {
          publisher: 'Canonical'
          offer: 'ubuntu-24_04-lts'
          sku: 'server'
          version: 'latest'
        }
        osDisk: {
          createOption: 'FromImage'
          caching: 'ReadOnly'
          diffDiskSettings: {
            option: 'Local'
            placement: 'CacheDisk'
          }
        }
      }
      networkProfile: {
        networkInterfaceConfigurations: [
          {
            name: 'nic0'
            properties: {
              primary: true
              enableAcceleratedNetworking: true
              networkSecurityGroup: {
                id: nsg.id
              }
              ipConfigurations: [
                {
                  name: 'ipcfg0'
                  properties: {
                    subnet: {
                      id: '${vnet.id}/subnets/workers'
                    }
                    privateIPAddressVersion: 'IPv4'
                    publicIPAddressConfiguration: {
                      name: 'pip'
                      properties: {
                        idleTimeoutInMinutes: 4
                        publicIPAddressVersion: 'IPv4'
                      }
                      sku: {
                        name: 'Standard'
                        tier: 'Regional'
                      }
                    }
                  }
                }
              ]
            }
          }
        ]
      }
      diagnosticsProfile: {
        bootDiagnostics: {
          enabled: true
        }
      }
      // Surface UAMI client ID to cloud-init via an instance metadata tag so
      // the IMDS token-refresh sidecar knows which identity to request.
      userData: base64('UAMI_CLIENT_ID=${uamiClientId}')
    }
  }
}

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------

output vmssId string = vmss.id
output vmssName string = vmss.name
output vnetId string = vnet.id
output nsgId string = nsg.id
