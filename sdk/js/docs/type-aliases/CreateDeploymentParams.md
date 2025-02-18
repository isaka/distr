[**@glasskube/distr-sdk**](../README.md)

---

[@glasskube/distr-sdk](../README.md) / CreateDeploymentParams

# Type Alias: CreateDeploymentParams

> **CreateDeploymentParams**: `object`

## Type declaration

### application

> **application**: `object`

#### application.id?

> `optional` **id**: `string`

#### application.versionId?

> `optional` **versionId**: `string`

### kubernetesDeployment?

> `optional` **kubernetesDeployment**: `object`

#### kubernetesDeployment.releaseName

> **releaseName**: `string`

#### kubernetesDeployment.valuesYaml?

> `optional` **valuesYaml**: `string`

### target

> **target**: `object`

#### target.kubernetes?

> `optional` **kubernetes**: `object`

#### target.kubernetes.namespace

> **namespace**: `string`

#### target.kubernetes.scope

> **scope**: [`DeploymentTargetScope`](DeploymentTargetScope.md)

#### target.name

> **name**: `string`

#### target.type

> **type**: [`DeploymentType`](DeploymentType.md)
