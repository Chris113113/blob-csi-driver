/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package blob

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	volumehelper "sigs.k8s.io/blob-csi-driver/pkg/util"

	"github.com/Azure/azure-sdk-for-go/storage"
	"github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	mount "k8s.io/mount-utils"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	mount_azure_blob "sigs.k8s.io/blob-csi-driver/pkg/blobfuse-proxy/pb"
)

type MountClient struct {
	service mount_azure_blob.MountServiceClient
}

// NewMountClient returns a new mount client
func NewMountClient(cc *grpc.ClientConn) *MountClient {
	service := mount_azure_blob.NewMountServiceClient(cc)
	return &MountClient{service}
}

// NodePublishVolume mount the volume from staging to target path
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	volumeID := req.GetVolumeId()
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	target := req.GetTargetPath()
	if len(target) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path not provided")
	}

	context := req.GetVolumeContext()
	if context != nil && strings.EqualFold(context[ephemeralField], trueValue) {
		context[secretNamespaceField] = context[podNamespaceField]
		// only get storage account from secret
		context[getAccountKeyFromSecretField] = trueValue
		context[storageAccountField] = ""
		klog.V(2).Infof("NodePublishVolume: ephemeral volume(%s) mount on %s, VolumeContext: %v", volumeID, target, context)
		_, err := d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
			StagingTargetPath: target,
			VolumeContext:     context,
			VolumeCapability:  volCap,
			VolumeId:          volumeID,
		})
		return &csi.NodePublishVolumeResponse{}, err
	}

	source := req.GetStagingTargetPath()
	if len(source) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	mountOptions := []string{"bind"}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	mnt, err := d.ensureMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount target %q: %v", target, err)
	}
	if mnt {
		klog.V(2).Infof("NodePublishVolume: volume %s is already mounted on %s", volumeID, target)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	klog.V(2).Infof("NodePublishVolume: volume %s mounting %s at %s with mountOptions: %v", volumeID, source, target, mountOptions)
	if d.enableBlobMockMount {
		klog.Warningf("NodePublishVolume: mock mount on volumeID(%s), this is only for TESTING!!!", volumeID)
		if err := volumehelper.MakeDir(target); err != nil {
			klog.Errorf("MakeDir failed on target: %s (%v)", target, err)
			return nil, err
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err := d.mounter.Mount(source, target, "", mountOptions); err != nil {
		if removeErr := os.Remove(target); removeErr != nil {
			return nil, status.Errorf(codes.Internal, "Could not remove mount target %q: %v", target, removeErr)
		}
		return nil, status.Errorf(codes.Internal, "Could not mount %q at %q: %v", source, target, err)
	}
	klog.V(2).Infof("NodePublishVolume: volume %s mount %s at %s successfully", volumeID, source, target)

	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) mountBlobfuseWithProxy(args string, authEnv []string) (string, error) {
	klog.V(2).Infof("mouting using blobfuse proxy")
	var resp *mount_azure_blob.MountAzureBlobResponse
	var output string
	connectionTimout := time.Duration(d.blobfuseProxyConnTimout)
	ctx, cancel := context.WithTimeout(context.Background(), connectionTimout*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, d.blobfuseProxyEndpoint, grpc.WithInsecure(), grpc.WithBlock())
	if err == nil {
		mountClient := NewMountClient(conn)
		mountreq := mount_azure_blob.MountAzureBlobRequest{
			MountArgs: args,
			AuthEnv:   authEnv,
		}
		klog.V(2).Infof("calling BlobfuseProxy: MountAzureBlob function")
		resp, err = mountClient.service.MountAzureBlob(context.TODO(), &mountreq)
		if err != nil {
			klog.Error("GRPC call returned with an error:", err)
		}
		output = resp.GetOutput()
	}
	return output, err
}

func (d *Driver) mountBlobfuseInsideDriver(args string, authEnv []string) (string, error) {
	klog.V(2).Infof("mounting blobfuse inside driver")
	cmd := exec.Command("blobfuse", strings.Split(args, " ")...)
	cmd.Env = append(os.Environ(), authEnv...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// NodeUnpublishVolume unmount the volume from the target path
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetTargetPath()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	targetPath := req.GetTargetPath()
	volumeID := req.GetVolumeId()

	klog.V(2).Infof("NodeUnpublishVolume: unmounting volume %s on %s", volumeID, targetPath)
	err := mount.CleanupMountPoint(targetPath, d.mounter, true /*extensiveMountPointCheck*/)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount target %q: %v", targetPath, err)
	}
	klog.V(2).Infof("NodeUnpublishVolume: unmount volume %s on %s successfully", volumeID, targetPath)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeStageVolume mount the volume to a staging path
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	targetPath := req.GetStagingTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}
	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability not provided")
	}

	if acquired := d.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.volumeLocks.Release(volumeID)

	mnt, err := d.ensureMountPoint(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not mount target %q: %v", targetPath, err)
	}
	if mnt {
		klog.V(2).Infof("NodeStageVolume: volume %s is already mounted on %s", volumeID, targetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	mountFlags := req.GetVolumeCapability().GetMount().GetMountFlags()
	attrib := req.GetVolumeContext()
	secrets := req.GetSecrets()

	var serverAddress, storageEndpointSuffix, protocol, ephemeralVolMountOptions string
	var ephemeralVol, isHnsEnabled bool
	for k, v := range attrib {
		switch strings.ToLower(k) {
		case serverNameField:
			serverAddress = v
		case protocolField:
			protocol = v
		case storageEndpointSuffixField:
			storageEndpointSuffix = v
		case ephemeralField:
			ephemeralVol = strings.EqualFold(v, trueValue)
		case mountOptionsField:
			ephemeralVolMountOptions = v
		case isHnsEnabledField:
			isHnsEnabled = strings.EqualFold(v, trueValue)
		}
	}

	accountName, containerName, authEnv, err := d.GetAuthEnv(ctx, volumeID, protocol, attrib, secrets)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(storageEndpointSuffix) == "" {
		if d.cloud.Environment.StorageEndpointSuffix != "" {
			storageEndpointSuffix = d.cloud.Environment.StorageEndpointSuffix
		} else {
			storageEndpointSuffix = storage.DefaultBaseURL
		}
	}

	if strings.TrimSpace(serverAddress) == "" {
		// server address is "accountname.blob.core.windows.net" by default
		serverAddress = fmt.Sprintf("%s.blob.%s", accountName, storageEndpointSuffix)
	}

	if protocol == nfs {
		klog.V(2).Infof("target %v\nprotocol %v\n\nvolumeId %v\ncontext %v\nmountflags %v\nserverAddress %v",
			targetPath, protocol, volumeID, attrib, mountFlags, serverAddress)

		source := fmt.Sprintf("%s:/%s/%s", serverAddress, accountName, containerName)
		mountOptions := util.JoinMountOptions(mountFlags, []string{"sec=sys,vers=3,nolock"})
		if err := wait.PollImmediate(1*time.Second, 2*time.Minute, func() (bool, error) {
			return true, d.mounter.MountSensitive(source, targetPath, nfs, mountOptions, []string{})
		}); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("volume(%s) mount %q on %q failed with %v", volumeID, source, targetPath, err))
		}

		// set 0777 for NFSv3 root folder
		if err := os.Chmod(targetPath, 0777); err != nil {
			return nil, status.Error(codes.Internal, fmt.Sprintf("Chmod(%s) failed with %v", targetPath, err))
		}
		klog.V(2).Infof("volume(%s) mount %q on %q succeeded", volumeID, source, targetPath)

		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Get mountOptions that the volume will be formatted and mounted with
	mountOptions := mountFlags
	if ephemeralVol {
		mountOptions = util.JoinMountOptions(mountOptions, strings.Split(ephemeralVolMountOptions, ","))
	}
	if isHnsEnabled {
		mountOptions = util.JoinMountOptions(mountOptions, []string{"--use-adls=true"})
	}
	// set different tmp-path with time info
	tmpPath := fmt.Sprintf("%s/%s#%d", "/mnt", volumeID, time.Now().Unix())
	mountOptions = appendDefaultMountOptions(mountOptions, tmpPath, containerName)

	args := targetPath
	for _, opt := range mountOptions {
		args = args + " " + opt
	}

	klog.V(2).Infof("target %v\nprotocol %v\n\nvolumeId %v\ncontext %v\nmountflags %v\nmountOptions %v\nargs %v\nserverAddress %v",
		targetPath, protocol, volumeID, attrib, mountFlags, mountOptions, args, serverAddress)

	authEnv = append(authEnv, "AZURE_STORAGE_ACCOUNT="+accountName, "AZURE_STORAGE_BLOB_ENDPOINT="+serverAddress)
	if d.enableBlobMockMount {
		klog.Warningf("NodeStageVolume: mock mount on volumeID(%s), this is only for TESTING!!!", volumeID)
		if err := volumehelper.MakeDir(targetPath); err != nil {
			klog.Errorf("MakeDir failed on target: %s (%v)", targetPath, err)
			return nil, err
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}

	var output string
	if d.enableBlobfuseProxy {
		output, err = d.mountBlobfuseWithProxy(args, authEnv)
	} else {
		output, err = d.mountBlobfuseInsideDriver(args, authEnv)
	}

	if err != nil {
		err = fmt.Errorf("Mount failed with error: %v, output: %v", err, output)
		klog.Errorf("%v", err)
		notMnt, mntErr := d.mounter.IsLikelyNotMountPoint(targetPath)
		if mntErr != nil {
			klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
			return nil, err
		}
		if !notMnt {
			if mntErr = d.mounter.Unmount(targetPath); mntErr != nil {
				klog.Errorf("Failed to unmount: %v", mntErr)
				return nil, err
			}
			notMnt, mntErr := d.mounter.IsLikelyNotMountPoint(targetPath)
			if mntErr != nil {
				klog.Errorf("IsLikelyNotMountPoint check failed: %v", mntErr)
				return nil, err
			}
			if !notMnt {
				// This is very odd, we don't expect it.  We'll try again next sync loop.
				klog.Errorf("%s is still mounted, despite call to unmount().  Will try again next sync loop.", targetPath)
				return nil, err
			}
		}
		os.Remove(targetPath)
		return nil, err
	}

	klog.V(2).Infof("volume(%s) mount on %q succeeded", volumeID, targetPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmount the volume from the staging path
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target not provided")
	}

	if acquired := d.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.volumeLocks.Release(volumeID)

	klog.V(2).Infof("NodeUnstageVolume: volume %s unmounting on %s", volumeID, stagingTargetPath)
	err := mount.CleanupMountPoint(stagingTargetPath, d.mounter, true /*extensiveMountPointCheck*/)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount staging target %q: %v", stagingTargetPath, err)
	}
	klog.V(2).Infof("NodeUnstageVolume: volume %s unmount on %s successfully", volumeID, stagingTargetPath)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodeGetCapabilities return the capabilities of the Node plugin
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: d.NSCap,
	}, nil
}

// NodeGetInfo return info of the node on which this plugin is running
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.NodeID,
	}, nil
}

// NodeGetVolumeStats get volume stats
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if len(req.VolumeId) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume ID was empty")
	}
	if len(req.VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeGetVolumeStats volume path was empty")
	}

	_, err := os.Stat(req.VolumePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "path %s does not exist", req.VolumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat file %s: %v", req.VolumePath, err)
	}

	volumeMetrics, err := volume.NewMetricsStatFS(req.VolumePath).GetMetrics()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get metrics: %v", err)
	}

	available, ok := volumeMetrics.Available.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume available size(%v)", volumeMetrics.Available)
	}
	capacity, ok := volumeMetrics.Capacity.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume capacity size(%v)", volumeMetrics.Capacity)
	}
	used, ok := volumeMetrics.Used.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform volume used size(%v)", volumeMetrics.Used)
	}

	inodesFree, ok := volumeMetrics.InodesFree.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes free(%v)", volumeMetrics.InodesFree)
	}
	inodes, ok := volumeMetrics.Inodes.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes(%v)", volumeMetrics.Inodes)
	}
	inodesUsed, ok := volumeMetrics.InodesUsed.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "failed to transform disk inodes used(%v)", volumeMetrics.InodesUsed)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Available: available,
				Total:     capacity,
				Used:      used,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
			},
		},
	}, nil
}

// NodeExpandVolume node expand volume
func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume is not yet implemented")
}

// ensureMountPoint: create mount point if not exists
// return <true, nil> if it's already a mounted point otherwise return <false, nil>
func (d *Driver) ensureMountPoint(target string) (bool, error) {
	notMnt, err := d.mounter.IsLikelyNotMountPoint(target)
	if err != nil && !os.IsNotExist(err) {
		if IsCorruptedDir(target) {
			notMnt = false
			klog.Warningf("detected corrupted mount for targetPath [%s]", target)
		} else {
			return !notMnt, err
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		_, err := ioutil.ReadDir(target)
		if err == nil {
			klog.V(2).Infof("already mounted to target %s", target)
			return !notMnt, nil
		}
		// mount link is invalid, now unmount and remount later
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", target, err)
		if err := d.mounter.Unmount(target); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", target, err)
			return !notMnt, err
		}
		notMnt = true
		return !notMnt, err
	}
	if err := volumehelper.MakeDir(target); err != nil {
		klog.Errorf("MakeDir failed on target: %s (%v)", target, err)
		return !notMnt, err
	}
	return !notMnt, nil
}
