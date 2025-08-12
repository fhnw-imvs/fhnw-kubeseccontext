# Orakel of Funk

<h1 align="left" style="border-bottom: none">
    <img alt="OrakelOfFunk" src="docs/images/orakel-of-funk-logo-small.png" width="250">
</h1>


Keep your Kubernetes workloads in tune — secure and functional.

**WIP: This project is part of a Bachelor thesis at FHNW called _Automated Kubernetes Workload hardening using a Functionality Oracle_.**

## What is the Orakel of Funk?

The Orakel of Funk automates the testing and validation of Kubernetes workload hardening through controlled runtime experiments. 
It supports both namespace-wide and workload-specific hardening tests, leveraging Kubernetes-native `securityContext` fields such as `runAsNonRoot`, `readOnlyRootFilesystem`, `seccompProfile`.

When a user creates a `WorkloadHardeningCheck` or `NamespaceHardeningCheck` resource, the operator:

- Clones the target namespace (excluding network resources like Ingress).
- Gathers baseline logs and metrics over a configurable `recordingDuration`.
- Iteratively applies individual runtime restrictions to cloned workloads, isolating the impact of each `securityContext` attribute.
- Compares the behavior of each modified workload against the baseline, producing recommended `securityContext` configuration for each `WorkloadHardeningCheck`.
- In case of a `NamespaceHardeningCheck` the recommendations are propagted to the resources, and a final check run is performed where each workload is hardened according to its recommendations.

This approach allows developers and operators to assess the impact of runtime hardening on their workloads in a systematic, reproducible and automated manner, enabling safer adoption of security best practices without introducing disruptions.

## Getting Started

To install the Operator in your cluster a Helm chart is provided, which deploys the operator and a ValKey instance to your cluster.

To generate the Certificates used by the webhooks, the helm chart relies on `cert-manager`, and assumes it is already present in the cluster. If you do not have `cert-manager` installed, check-out the [cert-manager documentation](https://cert-manager.io/docs/installation/) for installation instructions.

## Installation

The Helm chart is available from the Github package registry: [Orakel of Funk Helm Chart](https://github.com/fhnw-imvs/fhnw-kubeseccontext/pkgs/container/fhnw-kubeseccontext%2Forakel-of-funk)

**Since this is an OCI registry, you need to have at least Helm 3.8.0**

To install the chart, you can use the following command:
```
helm install orakel-of-funk oci://ghcr.io/fhnw-imvs/fhnw-kubeseccontext/orakel-of-funk:0.1.0
```

As long as the repository and packages repository are private, you will need to use a personal access token with the `read:packages` scope to authenticate to the registry. You can do this by running the following command:
```
helm registry login ghcr.io -u <your-username>
```

Since the container image is also hosted on the same registry, you can use the same credentials to pull the image used by the operator, but need to create the ImagePullSecret manually, as the intention is to make both the chart and the image available to the public.

Just make sure to set the `imagePullSecrets` in the `global` section of the values file, or use the `--set global.imagePullSecrets[0].name=<your-secret-name>` flag when installing the chart.

##  Usage

To use the operator, you need to create a `WorkloadHardeningCheck` or `NamespaceHardeningCheck` resource. 

### WorkloadHardeningCheck

This resource targets a single workload (currently Deployment, StatefulSet or DaemonSet) and applies the hardening checks to it. The operator will clone the workload and apply the hardening checks to the cloned workload.

An example of a `WorkloadHardeningCheck` resource is shown below:

```yaml
apiVersion: checks.funk.fhnw.ch/v1alpha1
kind: WorkloadHardeningCheck
metadata:
  name: write-fs-emptydir
  namespace: oof-write-fs-emptydir
  labels:
    app.kubernetes.io/part-of: orakel-of-funk
    app.kubernetes.io/component: test
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: write-fs-emptydir
  recordingDuration: 1m
```

Since this resource is namsepace-scoped, it will assume that the target workload is in the same namespace as the `WorkloadHardeningCheck` resource. The operator will clone the workload and apply the hardening checks to the cloned workload.


### NamespaceHardeningCheck

This resource targets a whole namespace and applies the hardening checks to all workloads in the namespace.

An example of a `NamespaceHardeningCheck` resource is shown below:

```yaml
apiVersion: checks.funk.fhnw.ch/v1alpha1
kind: NamespaceHardeningCheck
metadata:
  labels:
    app.kubernetes.io/name: orakel-of-funk
  name: podtato-namespace-hardening
spec:
  targetNamespace: podtato-kubectl
  recordingDuration: 1m
```

The resource is cluster-scoped, and will create `WorkloadHardeningCheck` resources for each workload in the `targetNamespace`.

## Example Workloads

To test the operator, multiple example workloads are provided in the `examples` directory. These workloads can be used to test the operator and see how it works.

### Podtato App

The [Podtato-Head](https://github.com/podtato-head/podtato-head) is a Demo App by the TAG App Delivery. It consists of multiple microservices, running in a single namespace.

This example contains a `NamespaceHardeningCheck` resource that targets the `podtato-kubectl` namespace.

### MariaDB

The MariaDB example contains rendered manifests, based on the MariaDB Helm chart using the default `values.yaml`.

It contains a `WorkloadHardeningCheck` resource that targets the `mariadb` deployment in the `mariadb` namespace.

### Nginx based workloads

There are multiple examples based on simple Nginx containers. They are intended to show how different configurations of a workload can affect the hardening checks and the recommendations made by the operator.

### Custom Workloads

Last but not least, there are a few custom workloads, based on simple bash scripts to test specific scenarios/`securityContext` configurations. These are intended to verify the operator's behavior in specific scenarios, such as:

- A workload that tries to write to the container filesystem.
- The same workload, but adapted to use an `emptyDir` volume.
- A workload trying to `chown` a file to a different user. This requires the `chown` capability to be set in the `securityContext`.
- A workload trying to escalate his privileges.


