// modules/role_dcr.bicep — Cross-RG role assignment.
//
// Invoked by foundation.bicep with `scope: resourceGroup(observabilityResourceGroup)`
// so the role assignment lands on the persistent DCR. The principal is the
// run RG's UAMI.

targetScope = 'resourceGroup'

@description('Name of the DCR in this RG (typically "ddos-dcr-prom").')
param dcrName string

@description('Principal (object) ID of the UAMI from the run RG.')
param principalId string

@description('Run suffix — used only to make the role assignment GUID stable per run.')
param runSuffix string

// Built-in: Monitoring Metrics Publisher
var roleMonitoringMetricsPublisher = subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '3913510d-42f4-4e42-8a64-420c390055eb')

resource dcr 'Microsoft.Insights/dataCollectionRules@2023-03-11' existing = {
  name: dcrName
}

resource role 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: dcr
  name: guid(dcr.id, principalId, runSuffix, 'monitoringMetricsPublisher')
  properties: {
    roleDefinitionId: roleMonitoringMetricsPublisher
    principalId: principalId
    principalType: 'ServicePrincipal'
  }
}

output roleAssignmentId string = role.id
