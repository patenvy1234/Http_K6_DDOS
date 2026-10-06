// cleanup-function.bicep — Deploys the subscription-level safety-net Azure Function
// that deletes expired ddos load-gen resource groups.
//
// Uses Flex Consumption (FC1) — the only modern serverless plan that fully
// supports identity-based storage with no Azure Files share. Required because
// the management group enforces "Storage accounts should prevent shared key
// access" (Deny effect).
//
// Architecture:
//   • Storage account: shared keys disabled; MI gets Storage Blob Data Owner
//   • Two blob containers: "deployment" (zip) + "host" (Functions host metadata)
//   • Function App (FC1): Python 3.11, system-assigned MI
//   • Subscription Contributor role → MI (via role_subscription.bicep module)
//
// targetScope is resourceGroup — caller must pre-create the RG.

targetScope = 'resourceGroup'

@description('Azure region. Must be a Flex Consumption-supported region.')
param location string = resourceGroup().location

@description('Azure subscription ID (used to scope the Contributor role).')
param subscriptionId string = subscription().subscriptionId

@description('Short name prefix for resource naming.')
@minLength(3)
@maxLength(12)
param namePrefix string = 'ddos'

@description('Tags applied to every resource.')
param tags object = {
  purpose: 'ddos-safety-net'
  managedBy: 'ddos-cli'
}

// ---------------------------------------------------------------------------
// Storage account — identity-only (shared keys disabled by policy)
// ---------------------------------------------------------------------------

var storageAccountName = take('${namePrefix}sn${uniqueString(resourceGroup().id)}', 24)

resource sa 'Microsoft.Storage/storageAccounts@2023-04-01' = {
  name: storageAccountName
  location: location
  tags: tags
  sku: { name: 'Standard_LRS' }
  kind: 'StorageV2'
  properties: {
    minimumTlsVersion: 'TLS1_2'
    allowBlobPublicAccess: false
    allowSharedKeyAccess: false
    supportsHttpsTrafficOnly: true
    defaultToOAuthAuthentication: true
  }
}

resource blobSvc 'Microsoft.Storage/storageAccounts/blobServices@2023-04-01' = {
  parent: sa
  name: 'default'
}

// Flex Consumption needs a blob container to hold the deployment zip.
resource deploymentContainer 'Microsoft.Storage/storageAccounts/blobServices/containers@2023-04-01' = {
  parent: blobSvc
  name: 'deployment'
}

// ---------------------------------------------------------------------------
// Application Insights (lightweight, helpful for log streaming)
// ---------------------------------------------------------------------------

resource appInsights 'Microsoft.Insights/components@2020-02-02' = {
  name: '${namePrefix}-sn-ai'
  location: location
  tags: tags
  kind: 'web'
  properties: {
    Application_Type: 'web'
    RetentionInDays: 30
  }
}

// ---------------------------------------------------------------------------
// Flex Consumption App Service Plan (FC1 — serverless, identity-friendly)
// ---------------------------------------------------------------------------

resource plan 'Microsoft.Web/serverfarms@2023-12-01' = {
  name: '${namePrefix}-sn-plan'
  location: location
  tags: tags
  sku: {
    name: 'FC1'
    tier: 'FlexConsumption'
  }
  properties: {
    reserved: true
  }
}

// ---------------------------------------------------------------------------
// Function App (Flex Consumption — Python 3.11, MI-based storage)
// ---------------------------------------------------------------------------

resource fnApp 'Microsoft.Web/sites@2023-12-01' = {
  name: '${namePrefix}-cleanup-fn'
  location: location
  tags: tags
  kind: 'functionapp,linux'
  identity: {
    type: 'SystemAssigned'
  }
  properties: {
    serverFarmId: plan.id
    httpsOnly: true
    siteConfig: {
      appSettings: [
        { name: 'AzureWebJobsStorage__accountName',      value: sa.name }
        { name: 'AzureWebJobsStorage__credential',       value: 'managedidentity' }
        { name: 'APPLICATIONINSIGHTS_CONNECTION_STRING', value: appInsights.properties.ConnectionString }
        { name: 'AZURE_SUBSCRIPTION_ID',                 value: subscriptionId }
      ]
      ftpsState: 'Disabled'
      minTlsVersion: '1.2'
    }
    functionAppConfig: {
      deployment: {
        storage: {
          type: 'blobContainer'
          value: '${sa.properties.primaryEndpoints.blob}${deploymentContainer.name}'
          authentication: {
            type: 'SystemAssignedIdentity'
          }
        }
      }
      runtime: {
        name: 'python'
        version: '3.11'
      }
      scaleAndConcurrency: {
        maximumInstanceCount: 40
        instanceMemoryMB: 2048
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Role assignment: Function MI → Storage Blob Data Owner on its own storage
// (required so the Functions host can read/write its identity-based blobs
//  and so it can deploy code from the "deployment" container).
// ---------------------------------------------------------------------------

resource saBlobOwner 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  scope: sa
  name: guid(sa.id, fnApp.id, 'storageBlobDataOwner')
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b7e6dc6d-f1e8-4753-8033-0f276bb0955b')
    principalId: fnApp.identity.principalId
    principalType: 'ServicePrincipal'
  }
}

// ---------------------------------------------------------------------------
// Role assignment: Function MI → Contributor on the subscription
// (needed to call resource_groups.begin_delete across the subscription).
// Delegated to a subscription-scoped helper module.
// ---------------------------------------------------------------------------

module contributorRole 'modules/role_subscription.bicep' = {
  name: 'cleanup-fn-contributor-role'
  scope: subscription()
  params: {
    roleDefinitionId: 'b24988ac-6180-42a0-ab88-20f7382dd24c'  // Contributor
    principalId: fnApp.identity.principalId
    seed: 'cleanup-fn-contributor'
  }
}

// ---------------------------------------------------------------------------
// Outputs
// ---------------------------------------------------------------------------

output functionAppName string = fnApp.name
output functionAppResourceId string = fnApp.id
output principalId string = fnApp.identity.principalId
output storageAccountName string = sa.name
output deploymentContainerName string = deploymentContainer.name
