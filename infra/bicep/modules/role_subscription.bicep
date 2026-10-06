// role_subscription.bicep — Assigns a role at subscription scope.
// Must be called as a module with scope: subscription() from a parent template.

targetScope = 'subscription'

@description('GUID of the role definition to assign.')
param roleDefinitionId string

@description('Principal ID receiving the role.')
param principalId string

@description('Unique seed string for the assignment GUID.')
param seed string

resource assignment 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(subscription().id, principalId, seed)
  properties: {
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', roleDefinitionId)
    principalId: principalId
    principalType: 'ServicePrincipal'
  }
}
