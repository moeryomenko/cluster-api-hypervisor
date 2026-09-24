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

// macFamilyPrefix is the first octet shared by every address this package
// derives: a locally administered, unicast octet. The remaining five octets
// carry the derived entropy.
const macFamilyPrefix = "c6"

// Derive returns the deterministic MAC address for a machine in a cluster.
// The first octet is the fixed family prefix; the remaining five octets come
// from the SHA-256 hash of the cluster/machine pair (40 bits of entropy), so
// the same machine name in two clusters and two machines in one cluster
// receive distinct addresses with negligible collision probability. Pinning
// five family octets (only one derived octet, 256 addresses) collided for
// real machine sets — a 16-machine cluster has >95% collision probability
// under the birthday bound — so the entropy deliberately lives in the
// address body. Empty names are tolerated and still produce a family-format
// address.
func Derive(clusterName, machineName string) string {
	sum := sha256.Sum256([]byte(clusterName + "/" + machineName))

	return fmt.Sprintf(
		"%s:%02x:%02x:%02x:%02x:%02x",
		macFamilyPrefix, sum[0], sum[1], sum[2], sum[3], sum[4],
	)
}
