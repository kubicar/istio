# Configurable Istio Images

This document describes how Istio component images are configured using environment variables, enabling support for different image variants like FIPS-compliant or OS-specific builds.

## Overview

Kyma Istio Operator uses environment variables to configure full image references for each Istio component. This approach provides flexibility to use different image registries, image names, and tags for each component independently.

The following Istio component images are configurable:

| Component       | Environment Variable | IstioOperator Path          |
|-----------------|----------------------|-----------------------------|
| Pilot           | `pilot`              | `values.pilot.image`        |
| Proxy (sidecar) | `proxyv2`            | `values.global.proxy.image` |
| CNI             | `install-cni`        | `values.cni.image`          |

The following Istio component images are configurable for FIPS-compliant configurations:

| Component       | Environment Variable | IstioOperator Path          |
|-----------------|----------------------|-----------------------------|
| Pilot           | `pilot-fips`         | `values.pilot.image`        |
| Proxy (sidecar) | `proxyv2-fips`       | `values.global.proxy.image` |
| CNI             | `install-cni-fips`   | `values.cni.image`          |
 

## Image Format

Each environment variable must contain a full image reference in the format:

```
<registry>/<repository>/<image-name>:<tag>
```

**Example:**
```
europe-docker.pkg.dev/kyma-project/prod/external/istio/pilot:1.24.0-distroless
```