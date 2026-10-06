// fleet.bicep — Phase 2 of the two-phase per-run deployment.
//
// Deploys one VMSS per region. Called after foundation.bicep so the Go
// controller can embed the storage account name into cloud-init before this
// deployment runs.
//
// Inputs: uamiResourceId, uamiClientId (from foundation outputs), plus
//         all fleet topology parameters and the pre-rendered customDataBase64.

targetScope = 'resourceGroup'

// ---------------------------------------------------------------------------
// Parameters
// ---------------------------------------------------------------------------

@description('Short run identifier.')
@minLength(3)
@maxLength(48)
param runName string

@description('8-character suffix.')
@minLength(6)
@maxLength(12)
param runSuffix string

@description('Regions to deploy a VMSS into.')
param regions array

@description('VM SKU (F4s_v2 or F8s_v2).')
@allowed([
  'Standard_F4s_v2'
  'Standard_F8s_v2'
])
param vmSize string = 'Standard_F8s_v2'

@description('Initial VMSS capacity per region.')
@minValue(1)
@maxValue(200)
param initialVMCount int

@description('Maximum VMSS capacity per region (stored as tag).')
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
// Tags (must match foundation.bicep tags for deadman consistency)
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
// Per-region modules
// ---------------------------------------------------------------------------

module regionDeploy 'modules/region.bicep' = [for (region, i) in regions: {
  name: 'region-${i}-${region}'
  params: {
    location: region
    runSuffix: runSuffix
    vmSize: vmSize
    capacity: initialVMCount
    useSpot: useSpot
    adminUsername: adminUsername
    sshPublicKey: sshPublicKey
    customDataBase64: customDataBase64
    uamiResourceId: uamiResourceId
    uamiClientId: uamiClientId
    tags: tags
  }
}]

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------

output vmssIds   array = [for (region, i) in regions: regionDeploy[i].outputs.vmssId]
output vmssNames array = [for (region, i) in regions: regionDeploy[i].outputs.vmssName]
