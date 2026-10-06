// observability_inner.bicep — RG-scoped child of observability.bicep.
//
// Splits the inner resources out so the parent template can create the RG and
// then deploy this module into it in a single subscription-scoped run.

targetScope = 'resourceGroup'

@description('Azure region.')
param location string

@description('Resource-name prefix (3-12 lowercase chars).')
param namePrefix string

@description('Object ID of the operator user that should get Grafana Admin.')
param operatorObjectId string

@description('Tags applied to every resource.')
param tags object

// ----------------------------------------------------------------------------
// Resource names
// ----------------------------------------------------------------------------

var workspaceName = '${namePrefix}-amw'
var dceName       = '${namePrefix}-dce'
var dcrName       = '${namePrefix}-dcr-prom'
var grafanaName   = '${namePrefix}-grafana'

// ----------------------------------------------------------------------------
// Azure Monitor Workspace (managed Prometheus)
// ----------------------------------------------------------------------------

resource amw 'Microsoft.Monitor/accounts@2023-04-03' = {
  name: workspaceName
  location: location
  tags: tags
  properties: {}
}

// ----------------------------------------------------------------------------
// Data Collection Endpoint (DCE) — workers POST metrics here
// ----------------------------------------------------------------------------

resource dce 'Microsoft.Insights/dataCollectionEndpoints@2023-03-11' = {
  name: dceName
  location: location
  tags: tags
  kind: 'Linux'
  properties: {
    networkAcls: {
      publicNetworkAccess: 'Enabled'
    }
  }
}

// ----------------------------------------------------------------------------
// Data Collection Rule (DCR) — routes Microsoft-PrometheusMetrics → workspace
// ----------------------------------------------------------------------------

resource dcr 'Microsoft.Insights/dataCollectionRules@2023-03-11' = {
  name: dcrName
  location: location
  tags: tags
  kind: 'Linux'
  properties: {
    dataCollectionEndpointId: dce.id
    dataSources: {
      prometheusForwarder: [
        {
          name: 'PrometheusDataSource'
          streams: [ 'Microsoft-PrometheusMetrics' ]
          labelIncludeFilter: {}
        }
      ]
    }
    destinations: {
      monitoringAccounts: [
        {
          accountResourceId: amw.id
          name: 'MonitoringAccount1'
        }
      ]
    }
    dataFlows: [
      {
        streams: [ 'Microsoft-PrometheusMetrics' ]
        destinations: [ 'MonitoringAccount1' ]
      }
    ]
  }
}

// ----------------------------------------------------------------------------
// Azure Managed Grafana (Standard SKU)
// ----------------------------------------------------------------------------

resource grafana 'Microsoft.Dashboard/grafana@2023-09-01' = {
  name: grafanaName
  location: location
  tags: tags
  sku: {
    name: 'Standard'
  }
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    publicNetworkAccess: 'Enabled'
    grafanaIntegrations: {
      azureMonitorWorkspaceIntegrations: [
        {
          azureMonitorWorkspaceResourceId: amw.id
        }
      ]
    }
  }
}

// ----------------------------------------------------------------------------
// Role assignments
// ----------------------------------------------------------------------------

// Built-in role definition IDs
var roleMonitoringDataReader = '/subscriptions/${subscription().subscriptionId}/providers/Microsoft.Authorization/roleDefinitions/b0d8363b-8ddd-447d-831f-62ca05bff136'
var roleGrafanaAdmin         = '/subscriptions/${subscription().subscriptionId}/providers/Microsoft.Authorization/roleDefinitions/22926164-76b3-42b3-bc55-97df8dab3e41'

// Grafana MI → Monitoring Data Reader on the workspace (so Grafana can query metrics).
resource grafanaReader 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: amw
  name: guid(amw.id, grafana.id, 'monitoringDataReader')
  properties: {
    roleDefinitionId: roleMonitoringDataReader
    principalId: grafana.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

// Operator → Grafana Admin on the Grafana resource.
resource operatorAdmin 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: grafana
  name: guid(grafana.id, operatorObjectId, 'grafanaAdmin')
  properties: {
    roleDefinitionId: roleGrafanaAdmin
    principalId: operatorObjectId
    principalType: 'User'
  }
}

// ----------------------------------------------------------------------------
// Outputs
// ----------------------------------------------------------------------------

output workspaceResourceId string = amw.id
output workspaceName string = amw.name
output dceUrl string = dce.properties.metricsIngestion.endpoint
output dceResourceId string = dce.id
output dcrResourceId string = dcr.id
output dcrImmutableId string = dcr.properties.immutableId
output grafanaUrl string = grafana.properties.endpoint
output grafanaResourceId string = grafana.id
output grafanaName string = grafana.name
output prometheusQueryUrl string = amw.properties.metrics.prometheusQueryEndpoint
