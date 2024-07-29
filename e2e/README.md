# End-to-end tests for konnectivity-network-proxy running in a KinD cluster

These e2e tests deploy the KNP agent and server to a local [KinD](https://kind.sigs.k8s.io/)
cluster to verify their functionality.

These can be run automatically using `make e2e-test`.

## Setup in `main_test.go`

Before any of the actual tests are run, the `TestMain()` function
in `main_test.go` performs the following set up steps:

- Spin up a new KinD cluster with the node image provided by the `-kind-image` flag.
- Sideload the KNP agent and server images provided with `-agent-image` and `-server-image` into the cluster.
- Deploy the necessary RBAC and service templates for both the KNP agent and server (see `renderAndApplyManifests`).

## The tests

### `static_count_test.go`

These tests deploy the KNP servers and agents to the previously created KinD cluster.
After the deployments are up, the tests check that both the agent and server report
the correct number of connections on their metrics endpoints.
