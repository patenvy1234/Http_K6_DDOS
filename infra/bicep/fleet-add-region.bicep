// fleet-add-region.bicep — Single-region VMSS addition used by the autoscaler.
//
// fleet.bicep deploys the *initial* fleet (one VMSS per region in the user's
// `regions` list). When the runtime scaler decides to add capacity, it
// prefers spinning up a brand-new VMSS in a region that doesn't yet have one
// (better SNAT pool, better spot availability, no per-VMSS cap concerns).
// This template owns that single-region deploy without touching existing
// VMSS resources (which would otherwise be reset to their initial capacity
// by an incremental fleet.bicep redeploy).

targetScope = 'resourceGroup'

@description('Short run identifier (matches fleet.bicep).')
@minLength(3)
@maxLength(48)
param runName string

@description('8-character run suffix.')
@minLength(6)
@maxLength(12)
param runSuffix string

@description('The Azure region for this VMSS (must not collide with an existing region in the same RG).')
param region string

@description('VM SKU (must match the rest of the fleet for uniform per-VM RPS sizing).')
@allowed([
  'Standard_F4s_v2'
  'Standard_F8s_v2'
])
param vmSize string

@description('Initial VMSS capacity for this new region.')
@minValue(1)
@maxValue(200)
param capacity int

@description('Total fleet maxVMCount — stored as a tag so the deadman/cleanup logic stays consistent.')
@minValue(1)
@maxValue(500)
param maxVMCount int

@description('Use spot pricing.')
param useSpot bool = true

@description('SSH admin username.')
param adminUsername string = 'ddos'

@description('SSH public key for the admin user.')
@secure()
param sshPublicKey string

@description('Base64-encoded cloud-init (already rendered with storage info).')
@secure()
param customDataBase64 string

@description('RFC3339 auto-delete-at timestamp (for tagging).')
param autoDeleteAt string

@description('Owner identifier.')
param owner string = ''

@description('Full resource ID of the UAMI from foundation.bicep.')
param uamiResourceId string

@description('Client ID of the UAMI from foundation.bicep.')
param uamiClientId string

// ---------------------------------------------------------------------------
// Tags (must match fleet.bicep tags for deadman consistency)
// ---------------------------------------------------------------------------

var tags = {
  purpose: 'ddos-loadgen'
  runId: runSuffix
  runName: runName
  'auto-delete-at': autoDeleteAt
  owner: owner
  managedBy: 'ddos-cli'
  maxVMCount: string(maxVMCount)
}

// ---------------------------------------------------------------------------
// Single-region VMSS via the same module fleet.bicep uses
// ---------------------------------------------------------------------------

module regionDeploy 'modules/region.bicep' = {
  name: 'region-add-${region}'
  params: {
    location: region
    runSuffix: runSuffix
    vmSize: vmSize
    capacity: capacity
    useSpot: useSpot
    adminUsername: adminUsername
    sshPublicKey: sshPublicKey
    customDataBase64: customDataBase64
    uamiResourceId: uamiResourceId
    uamiClientId: uamiClientId
    tags: tags
  }
}

output vmssId   string = regionDeploy.outputs.vmssId
output vmssName string = regionDeploy.outputs.vmssName
