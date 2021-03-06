# AIS helm chart 

## Overview

This repo includes all the definition of launching a AIS proxy and target on a K8s cluster.

### PREREQUISITES
One (and only one) of the nodes in the K8s cluster must have a label "initial_primary_proxy" with value "yes". This can be set by command:
```bash
kubectl label nodes <A-node-name> initial_primary_proxy=yes
```

### Installation

1. Prepare chart dependencies (even if you intend not to install Grafana and Graphite):

    helm repo add kiwigrid https://kiwigrid.github.io       # graphite lives here
    helm dependency update                                  # pull in charts we depend upon

2. The default is to install instances of Graphite and Grafana alongside AIS.

   * To install AIS with Graphite/Grafana metrics but with no persistence of Graphite/Grafana metrics and state:
     ```bash
     helm install --name=devops-ais \
        --set image.dockerRepoToken=<token-to-pull-docker-image> \
        --set graphite.persistence.enabled=false \
        --set grafana.persistence.enabled=false \
        .
      ```
   * To install AIS along with Graphite/Grafana, with persistence for Graphite/Grafana:
     Persistence is enabled by default, but you need to supply a path to a pre-created area in which to persist data. In the example below these areas are /data/{graphite,grafana} on node cpu01.
     ```bash
     helm install --name=devops-ais \
        --set image.dockerRepoToken=<token-to-pull-docker-image> \
        --set graphite.ais.pv.path=/data/graphite \
        --set graphite.ais.pv.node=cpu01 \
        --set graphite.ais.pv.capacity=250Gi \
        --set grafana.ais.pv.path=/data/graphite \
        --set grafana.ais.pv.node=cpu01 \
        --set grafana.ais.pv.capacity=250Gi \
        .
    ```

   * To install AIS using an external instance of Graphite:
     ```bash
     helm install --name=devops-ais \
        --set image.dockerRepoToken=<token-to-pull-docker-image> \
        --set tags.builtin_monitoring=false \
        --set external_monitoring.graphite_host=... \
        --set external_monitoring.graphite_port=... \
        .
    ```

### Deletion
You can delete the release using:
```bash
    helm delete --purge devops-ais
```
This will not delete buckets and objects stored in AIS filesystems, but it will forfeit AIS state.

#### Values

The following can be changed in values.yaml or specified on the install cmdline with `--set`. Some other values in `values.yaml` can be changed, but others may break the chart deployment or AIS installation so proceed with care!
| Key                               | Default                           | Description  |
| --------------------------------- | --------------------------------- | ------------ |
| `image.repository`                | `quay.io/nvidia/ais_k8s`          | Docker repo for AIS image |
| `image.tag`                       | `stable`                          | Image tag value for AIS image |
| `image.dockerRepoToken`           | nil                               | Token for private repo access |
| `target.mountPaths`               | `["/ais/sda", ..., "/ais/sdj"]`   | AIS filesystem paths |
| `tags.builtin_monitoring`         | `true`                            | If true then deploy Graphite/Grafana in cluster |
| `graphite.persistence.enabled`    | true                              | Applies if `builtin_monitorting` is true |
| `graphite.ais.pv.capacity`        | `250Gi`                           | If persisting, capacity of storage provided to Graphite |
| `graphite.ais.pv.node`            | nil                               | Required if persisting; node providing path for storage
| `graphite.ais.pv.path`            | nil                               | Required if persisting; local storage path of storage provided to Graphite |
| `grafana.persistence.enabled`    | true                              | Applies if `builtin_monitorting` is true |
| `grafana.ais.pv.capacity`        | `250Gi`                           | If persisting, capacity of storage provided to Grafana |
| `grafana.ais.pv.node`            | nil                               | Required if persisting; node providing path for storage
| `grafana.ais.pv.path`            | nil                               | Required if persisting; local storage path of storage provided to Grafana |
| `external_monitoring.graphite_host` | nil                             | Applies if `tags.builtin_monitoring` is false |
| `external_monitoring.graphite_port` | 2003                            | Applies if `tags.builtin_monitoring` is false |

