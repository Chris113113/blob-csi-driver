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

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"sigs.k8s.io/blob-csi-driver/pkg/blob"

	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

func init() {
	_ = flag.Set("logtostderr", "true")
}

var (
	endpoint                   = flag.String("endpoint", "unix://tmp/csi.sock", "CSI endpoint")
	blobfuseProxyEndpoint      = flag.String("blobfuse-proxy-endpoint", "unix://tmp/blobfuse-proxy.sock", "blobfuse-proxy endpoint")
	nodeID                     = flag.String("nodeid", "", "node id")
	version                    = flag.Bool("version", false, "Print the version and exit.")
	metricsAddress             = flag.String("metrics-address", "0.0.0.0:29634", "export the metrics")
	kubeconfig                 = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Required only when running out of cluster.")
	driverName                 = flag.String("drivername", blob.DefaultDriverName, "name of the driver")
	enableBlobfuseProxy        = flag.Bool("enable-blobfuse-proxy", false, "Whether using blobfuse proxy for mounts")
	blobfuseProxyConnTimout    = flag.Int("blobfuse-proxy-connect-timeout", 5, "blobfuse proxy connection timeout(seconds)")
	enableBlobMockMount        = flag.Bool("enable-blob-mock-mount", false, "Whether enable mock mount(only for testing)")
	cloudConfigSecretName      = flag.String("cloud-config-secret-name", "azure-cloud-provider", "secret name of cloud config")
	cloudConfigSecretNamespace = flag.String("cloud-config-secret-namespace", "kube-system", "secret namespace of cloud config")
	customUserAgent            = flag.String("custom-user-agent", "", "custom userAgent")
	userAgentSuffix            = flag.String("user-agent-suffix", "", "userAgent suffix")
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	if *version {
		info, err := blob.GetVersionYAML(*driverName)
		if err != nil {
			klog.Fatalln(err)
		}
		fmt.Println(info)
		os.Exit(0)
	}

	exportMetrics()
	handle()
	os.Exit(0)
}

func handle() {
	driverOptions := blob.DriverOptions{
		NodeID:                     *nodeID,
		DriverName:                 *driverName,
		CloudConfigSecretName:      *cloudConfigSecretName,
		CloudConfigSecretNamespace: *cloudConfigSecretNamespace,
		BlobfuseProxyEndpoint:      *blobfuseProxyEndpoint,
		EnableBlobfuseProxy:        *enableBlobfuseProxy,
		BlobfuseProxyConnTimout:    *blobfuseProxyConnTimout,
		EnableBlobMockMount:        *enableBlobMockMount,
		CustomUserAgent:            *customUserAgent,
		UserAgentSuffix:            *userAgentSuffix,
	}
	driver := blob.NewDriver(&driverOptions)
	if driver == nil {
		klog.Fatalln("Failed to initialize Azure Blob Storage CSI driver")
	}
	driver.Run(*endpoint, *kubeconfig, false)
}

func exportMetrics() {
	l, err := net.Listen("tcp", *metricsAddress)
	if err != nil {
		klog.Warningf("failed to get listener for metrics endpoint: %v", err)
		return
	}
	serve(context.Background(), l, serveMetrics)
}

func serve(ctx context.Context, l net.Listener, serveFunc func(net.Listener) error) {
	path := l.Addr().String()
	klog.V(2).Infof("set up prometheus server on %v", path)
	go func() {
		defer l.Close()
		if err := serveFunc(l); err != nil {
			klog.Fatalf("serve failure(%v), address(%v)", err, path)
		}
	}()
}

func serveMetrics(l net.Listener) error {
	m := http.NewServeMux()
	m.Handle("/metrics", legacyregistry.Handler()) //nolint, because azure cloud provider uses legacyregistry currently
	return trapClosedConnErr(http.Serve(l, m))
}

func trapClosedConnErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}
