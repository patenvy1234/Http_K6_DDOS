// observability.bicep — subscription-scoped deployment that creates the persistent
// monitoring stack used by every `ddos run`. Provisioned once via
// `ddos init-observability`.
//
// Resources:
//   • RG (ddos-observability)
//   • Azure Monitor Workspace (managed Prometheus)
//   • Data Collection Endpoint (DCE) — workers POST metrics here
//   • Data Collection Rule (DCR) — routes Microsoft-PrometheusMetrics → workspace
//   • Azure Managed Grafana (Standard SKU) with workspace as datasource
//   • Role assignments:
//       – Grafana managed identity → Monitoring Reader on the workspace
//       – Operator principal → Grafana Admin on the Grafana resource
//
// Workers use *their own* managed identity (granted "Monitoring Metrics Publisher"
// on the DCR at run-RG deploy time) to POST to the DCE.

targetScope = 'subscription'

@description('Location for all observability resources.')
param location string = 'eastus'

@description('Name of the persistent observability RG.')
param resourceGroupName string = 'ddos-observability'

@description('Short prefix used for resource naming (lowercase, alphanumeric).')
@minLength(3)
@maxLength(12)
param namePrefix string = 'ddos'

@description('Object ID of the user that should receive Grafana Admin. Run `az ad signed-in-user show --query id -o tsv` to fetch.')
param operatorObjectId string

@description('Optional tags applied to every resource.')
param tags object = {
  purpose: 'ddos-loadgen'
  component: 'observability'
  managedBy: 'ddos-cli'
}

resource rg 'Microsoft.Resources/resourceGroups@2024-03-01' = {
  name: resourceGroupName
  location: location
  tags: tags
}

module obs 'modules/observability_inner.bicep' = {
  scope: rg
  name: 'obs-inner'
  params: {
    location: location
    namePrefix: namePrefix
    operatorObjectId: operatorObjectId
    tags: tags
  }
}

@description('Resource ID of the Azure Monitor Workspace.')
output workspaceResourceId string = obs.outputs.workspaceResourceId

@description('Short name of the Azure Monitor Workspace.')
output workspaceName string = obs.outputs.workspaceName

@description('Ingestion endpoint URL of the DCE (workers POST metrics here).')
output dceUrl string = obs.outputs.dceUrl

@description('Resource ID of the Data Collection Endpoint.')
output dceResourceId string = obs.outputs.dceResourceId

@description('Resource ID of the Data Collection Rule.')
output dcrResourceId string = obs.outputs.dcrResourceId

@description('Immutable ID of the DCR — needed in the worker remote_write URL.')
output dcrImmutableId string = obs.outputs.dcrImmutableId

@description('Public URL of the Azure Managed Grafana instance.')
output grafanaUrl string = obs.outputs.grafanaUrl

@description('Resource ID of the Grafana instance.')
output grafanaResourceId string = obs.outputs.grafanaResourceId

@description('Short name of the Azure Managed Grafana resource.')
output grafanaName string = obs.outputs.grafanaName

@description('Prometheus-compatible query endpoint of the workspace.')
output prometheusQueryUrl string = obs.outputs.prometheusQueryUrl

@description('Name of the observability resource group.')
output resourceGroupName string = rg.name
