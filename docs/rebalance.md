## Table of Contents

- [Global Rebalancing](#global-rebalancing)
- [Local Rebalancing](#local-rebalancing)
- [Limitations](#limitations)

## Global Rebalancing

To maintain [consistent distribution of user data at all times](https://en.wikipedia.org/wiki/Consistent_hashing#Examples_of_use), AIStore rebalances itself based on *new* versions of its [cluster map](/cluster/map.go).

More exactly:

* When storage targets join or leave the cluster, the current *primary* (leader) proxy transactionally creates the *next* updated version of the cluster map;
* [Synchronizes](/ais/metasync.go) the new map across the entire cluster so that each and every node gets the version;
* Which further results in each AIS target starting to traverse its locally stored content, recomputing object locations,
* And sending at least some of the objects to their respective *new* locations
* Whereby object migration is carried out via intra-cluster optimized [communication mechanism](/transport/README.md) and over a separate [physical or logical network](/cmn/network.go), if provisioned.

Thus, cluster-wide rebalancing is totally and completely decentralized. When a single server joins (or goes down in a) cluster of N servers, approximately 1/Nth of the entire namespace will get rebalanced via direct target-to-target transfers.

Further, cluster-wide rebalancing does not require any downtime. Incoming GET requests for the objects that haven't yet migrated (or are being moved) are handled internally via the mechanism that we call "get-from-neighbor". The (rebalancing) target that must (according to the new cluster map) have the object but doesn't will locate its "neighbor", get the object, and satisfy the original GET request transparently from the user.

## Local Rebalancing

While global rebalancing (previous section) takes care of the *cluster-grow* and *cluster-shrink* events, local rebalancing, as the name implies, is responsible for the *mountpath-added* and *mountpath-removed* events that are handled locally within (and by) each storage target.

> Terminology: *mountpath* is a triplet **(local filesystem (LFS), disks that this LFS utilizes, LFS directory)**. The following rules apply: 1) different mountpaths use different LFSes, and 2) different LFSes use different disks.

Further, mountpath removal can be done administratively or be triggered by a disk fault (see [filesystem health checking](/health/fshc.md). Irrespectively of the original cause, mountpath-level events activate local rebalancer that in many ways performs the same set of steps as the global one. The one salient difference is that all object migrations are local (and, therefore, relatively fast(er)).

## Limitations

AIS *cluster rebalancing* has limitations that we are aware of. As of the v2.0, the limitations are:

* **Single target at a time**. Joining another storage target, or targets, while AIS cluster is still rebalancing is **not recommended** as it may result in GET errors.
* **IO performance**. During rebalancing, response latency and overall cluster throughput may substantially degrade.

All of these limitations will be addressed and removed in the nearest (v2.1) future.
