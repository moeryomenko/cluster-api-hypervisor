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

// Package mac derives the default MAC address for a machine from its
// cluster and machine names.
package mac

import (
	"crypto/sha256"
	"fmt"
)

// macFamilyPrefix is the locally administered prefix shared by every
// address this package derives.
const macFamilyPrefix = "c6:e5:50:1c:ec"

// Derive returns the deterministic MAC address for a machine in a cluster.
// The first five octets are the fixed family prefix; the last octet comes
// from the SHA-256 hash of the cluster/machine pair, so the same machine
// name in two clusters and two machines in one cluster receive distinct
// addresses. Empty names are tolerated and still produce a family-format
// address.
func Derive(clusterName, machineName string) string {
	sum := sha256.Sum256([]byte(clusterName + "/" + machineName))
	return fmt.Sprintf("%s:%02x", macFamilyPrefix, sum[0])
}
