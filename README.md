
# csi-rclone

implementing a k8s container storage interface (csi) plugin using [rclone](https://rclone.org/) to mount a remote

## install 

review and execute `kubectl apply -f k8s/manifests.yaml` to install the csi plugin into your k8s cluster in the `csi-rclone` namespace

among other things, this will install a StorageClass `csi-rclone` to be used for dynamic volume provisioning

## example

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: mount1
  namespace: test
spec:
  accessModes:
  - ReadOnlyMany
  resources:
    requests:
      storage: 1Mi # not used
  storageClassName: csi-rclone
---
apiVersion: v1
kind: Secret
type: Opaque
metadata:
  name: mount1 # must match name of PVC above
  namespace: test
stringData:
  remote: aws-public
  remotePath: "/esa-worldcover-s2"
  configData: |
    [aws-public]
    type = s3
    provider = AWS
    region = eu-central-1
    # access_key_id = xxx
    # secret_access_key = xxx
---
apiVersion: v1
kind: Pod
metadata:
  name: mount1
  namespace: test
spec:
  containers:
  - name: bash
    image: ubuntu:24.04
    command: ["/bin/bash", "-c", "sleep infinity"]
    volumeMounts:
    - name: data
      mountPath: "/data"
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: mount1
```

run e.g. `kubectl exec -it mount1 -n test -- ls -la /data/rgbnir/2021/S22/` to see satellite data from the [ESA WorldCover product](https://esa-worldcover.org/en/data-access).

## Acknowledgement
implementation is derived (all Apache-2.0 licensed) from:
- https://github.com/ctrox/csi-s3
- https://github.com/wunderio/csi-rclone
- https://github.com/SwissDataScienceCenter/csi-rclone

## License

[Apache 2.0](LICENSE) (Apache License Version 2.0, January 2004) from https://www.apache.org/licenses/LICENSE-2.0