# Releasing a new version of apiserver-network-proxy

Please note this guide is only intended for the admins of this repository, and requires write access.

Creating a new release of network proxy involves releasing a new version of the client library (konnectivity-client) and new images for the proxy agent and server. Generally we also want to upgrade kubernetes/kubernetes with the latest version of the images and library, but this is a GCE specific change.

1. The first step involves creating a new git tag for the release, following semvar for go libraries. A tag is required for both the repository and the konnectivity-client library. For example releasing the `0.0.15` version will have two tags `v0.0.15` and `konnectivity-client/v0.0.15` on the appropriate commit.

    The exact commands are

    ```
    # Check out the appropriate commit (usually head of master)
    git tag -a v0.0.15
    git tag konnectivity-client/v0.0.15
    git push upstream v0.0.15
    git push upstream konnectivity-client/v0.0.15
    ```

    Once the two tags are created, the konnectivity-client can be imported as a library in kubernetes/kubernetes and other go programs.

2. To publish the proxy server and proxy agent images, they must be promoted from the k8s staging repo. An example PR can be seen here: [https://github.com/kubernetes/k8s.io/pull/1602](https://github.com/kubernetes/k8s.io/pull/1602)

    The SHA in the PR corresponds to the SHA of the image within the k8s staging repo. (This is under the **Name** column)

    The images can be found here for the [proxy server](http://console.cloud.google.com/gcr/images/k8s-staging-kas-network-proxy/GLOBAL/proxy-server?gcrImageListsize=30) and [proxy agent](http://console.cloud.google.com/gcr/images/k8s-staging-kas-network-proxy/GLOBAL/proxy-agent?gcrImageListsize=30).

    Please ensure that the commit shown on the tag of the image matches the one shown in the tags page in the network proxy repo.

    <img src="https://user-images.githubusercontent.com/7691399/106816880-09040600-6644-11eb-8907-f50c53dfe475.png" width="400px" height="300px" /> <img src="https://user-images.githubusercontent.com/7691399/106815303-a4e04280-6641-11eb-82d2-4ef4fb34437a.png" width="400px" height="300px" />

3. Finally, update kubernetes/kubernetes with the new client library and images.

    An example PR can be found here: [https://github.com/kubernetes/kubernetes/pull/94983](https://github.com/kubernetes/kubernetes/pull/94983)

    Image paths must be bumped and are located in:

    - cluster/gce/addons/konnectivity-agent/konnectivity-agent-ds.yaml
    - cluster/gce/manifests/konnectivity-server.yaml

    To update the library, no go.mod files need to be manually modified and running these commands in k/k will perform the necessary go.mod changes.

    ```
    ./hack/pin-dependency.sh sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.0.15
    make clean generated_files
    ./hack/update-vendor.sh
    ```
