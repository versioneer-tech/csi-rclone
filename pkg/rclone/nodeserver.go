// Node(Server) takes charge of volume mounting and unmounting.

package rclone

// Restructure this file !!!
// Follow lifecycle

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/ini.v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/fernet/fernet-go"
	"github.com/versioneer-tech/csi-rclone/pkg/kube"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/utils/mount"

	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter   *mount.SafeFormatAndMount
	RcloneOps Operations
}

// Mounting Volume (Preparation)
func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method NodeStageVolume not implemented")
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method NodeUnstageVolume not implemented")
}

// Mounting Volume (Actual Mounting)
func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolumeRequest=%+v", *req)
	if err := validatePublishVolumeRequest(req); err != nil {
		return nil, err
	}

	targetPath := req.GetTargetPath()
	volumeId := req.GetVolumeId()
	volumeContext := req.GetVolumeContext()
	readOnly := req.GetReadonly()
	secretName, foundSecret := volumeContext["secretName"]
	secretNamespace, foundSecretNamespace := volumeContext["secretNamespace"]
	var pvcSecret *v1.Secret = nil
	var err error
	if foundSecret && foundSecretNamespace {
		pvcSecret, err = getSecret(ctx, secretNamespace, secretName)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}

	remote, remotePath, configData, parameters, e := extractFlags(req.GetVolumeContext(), req.GetSecrets(), pvcSecret)
	delete(parameters, "secretName")
	delete(parameters, "secretNamespace")
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}
	notMnt, err := ns.mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		if _, err := os.ReadDir(targetPath); err == nil {
			klog.Infof("already mounted to target %s", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// todo: mount link is invalid, now unmount and remount later (built-in functionality)
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", targetPath, err)

		if err := ns.mounter.Unmount(targetPath); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}
	}

	rcloneVol := &RcloneVolume{
		ID:         volumeId,
		Remote:     remote,
		RemotePath: remotePath,
	}
	err = ns.RcloneOps.Mount(ctx, rcloneVol, targetPath, configData, readOnly, parameters)
	if err != nil {
		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	// err = ns.WaitForMountAvailable(targetPath)
	// if err != nil {
	// 	return nil, status.Error(codes.Internal, err.Error())
	// }
	return &csi.NodePublishVolumeResponse{}, nil
}

func getSecret(ctx context.Context, namespace, name string) (*v1.Secret, error) {
	cs, err := kube.GetK8sClient()
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		return nil, fmt.Errorf("Failed to read Secret with K8s client because namespace is blank")
	}
	if name == "" {
		return nil, fmt.Errorf("Failed to read Secret with K8s client because name is blank")
	}
	return cs.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

func getPVC(ctx context.Context, namespace, name string) (*v1.PersistentVolumeClaim, error) {
	cs, err := kube.GetK8sClient()
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		return nil, fmt.Errorf("Failed to read PVC with K8s client because namespace is blank")
	}
	if name == "" {
		return nil, fmt.Errorf("Failed to read PVC with K8s client because name is blank")
	}
	return cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
}

func validatePublishVolumeRequest(req *csi.NodePublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "empty volume id")
	}

	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "empty target path")
	}

	if req.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument, "no volume capability set")
	}
	return nil
}

func extractFlags(volumeContext map[string]string, secret map[string]string, pvcSecret *v1.Secret) (string, string, string, map[string]string, error) {

	// Empty argument list
	flags := make(map[string]string)

	// Secret values are default, gets merged and overriden by corresponding PV values
	if len(secret) > 0 {
		// Needs byte to string casting for map values
		for k, v := range secret {
			flags[k] = string(v)
		}
	} else {
		klog.Infof("No csi-rclone connection defaults secret found.")
	}

	if len(volumeContext) > 0 {
		for k, v := range volumeContext {
			if strings.HasPrefix(k, "storage.kubernetes.io/") {
				continue
			}
			flags[k] = v
		}
	}
	if pvcSecret != nil {
		if len(pvcSecret.Data) > 0 {
			for k, v := range pvcSecret.Data {
				flags[k] = string(v)
			}
		}
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath = remotePath + remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	configData, flags := extractConfigData(flags)

	return remote, remotePath, configData, flags, nil
}

func decryptSecrets(flags map[string]string, savedPvcSecret *v1.Secret) (map[string]string, error) {
	savedSecrets := make(map[string]string)

	userSecretKey, ok := flags["secretKey"]
	if !ok {
		return savedSecrets, status.Error(codes.InvalidArgument, "missing user secret key")
	}
	fernetKey, err := fernet.DecodeKey(userSecretKey)
	if err != nil {
		return savedSecrets, status.Errorf(codes.InvalidArgument, "cannot decode user secret key: %s", err)
	}

	if len(savedPvcSecret.Data) > 0 {
		for k, v := range savedPvcSecret.Data {
			savedSecrets[k] = string(fernet.VerifyAndDecrypt([]byte(v), 0, []*fernet.Key{fernetKey}))
		}
	}

	return savedSecrets, nil
}

func updateConfigData(remote string, configData string, savedSecrets map[string]string) (string, error) {
	iniData, err := ini.Load([]byte(configData))
	if err != nil {
		return "", fmt.Errorf("cannot load ini config data: %s", err)
	}

	section := iniData.Section(remote)
	for k, v := range savedSecrets {
		section.Key(k).SetValue(v)
	}

	buf := new(bytes.Buffer)
	iniData.WriteTo(buf)

	return buf.String(), nil
}

func extractConfigData(parameters map[string]string) (string, map[string]string) {
	flags := make(map[string]string)
	for k, v := range parameters {
		flags[k] = v
	}
	var configData string
	var ok bool
	if configData, ok = flags["configData"]; ok {
		delete(flags, "configData")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")

	return configData, flags
}

// Unmounting Volumes
func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.Infof("NodeUnpublishVolume called with: %s", req)
	if err := validateUnPublishVolumeRequest(req); err != nil {
		return nil, err
	}
	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		klog.Warning("no target path provided for NodeUnpublishVolume")
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	if _, err := ns.RcloneOps.GetVolumeById(ctx, req.GetVolumeId()); err == ErrVolumeNotFound {
		klog.Warning("VolumeId not found for NodeUnpublishVolume")
		mount.CleanupMountPoint(req.GetTargetPath(), ns.mounter, false)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	if err := ns.RcloneOps.Unmount(ctx, req.GetVolumeId(), targetPath); err != nil {
		klog.Warningf("Unmounting volume failed: %s", err)
	}
	mount.CleanupMountPoint(req.GetTargetPath(), ns.mounter, false)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func validateUnPublishVolumeRequest(req *csi.NodeUnpublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "empty volume id")
	}

	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "empty target path")
	}

	return nil
}

// Resizing Volume
func (*nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method NodeExpandVolume not implemented")
}

func (ns *nodeServer) WaitForMountAvailable(mountpoint string) error {
	for {
		select {
		case <-time.After(100 * time.Millisecond):
			notMnt, _ := ns.mounter.IsLikelyNotMountPoint(mountpoint)
			if !notMnt {
				return nil
			}
		case <-time.After(3 * time.Second):
			return errors.New("wait for mount available timeout")
		}
	}
}
