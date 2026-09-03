/*
Copyright 2026 The cluster-api-hypervisor Authors.

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

// Package etcdsnap captures consistent etcd snapshots from a control-plane
// VM through the etcd v3 grpc-gateway kv snapshot endpoint. The lab etcd
// runs without TLS and without client-cert authentication on the isolated
// k8netd L2 segment, and the gateway is enabled by default; the provider
// reaches the endpoint through the k8netd-published host port of the VM's
// client port (the same loopback reachability the apiserver healthz poller
// uses), never through the VM internal IP, which has no host route.
package etcdsnap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultTimeout bounds one snapshot capture request.
const DefaultTimeout = 5 * time.Minute

// Capture fetches one consistent point-in-time etcd snapshot from the v3
// grpc-gateway kv snapshot endpoint at http://host:port and returns the
// snapshot bytes. The snapshot is a binary file suitable for
// `etcdctl snapshot restore`. A non-200 answer, a truncated body, or a
// request failure surfaces as an error.
func Capture(ctx context.Context, host string, port int32) ([]byte, error) {
	url := "http://" + host + ":" + strconv.Itoa(int(port)) + "/v3/kv/snapshot"

	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build etcd snapshot request for %q: %w", url, err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post etcd snapshot request to %q: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read etcd snapshot response from %q: %w", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("etcd snapshot endpoint %q answered %s: %s", url, resp.Status, truncateErr(body))
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("etcd snapshot endpoint %q answered 200 with an empty body", url)
	}

	return body, nil
}

// truncateErr renders a bounded view of an error body for error messages.
func truncateErr(body []byte) string {
	const max = 256
	if len(body) > max {
		return string(body[:max]) + "..."
	}

	return string(body)
}
