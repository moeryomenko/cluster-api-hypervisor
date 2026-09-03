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

package ch

import (
	"fmt"
	"strconv"
	"strings"
)

// minNetQueues is the hard floor cloud-hypervisor enforces on the number of
// virtio-net virtqueues: validation rejects any NetConfig with fewer than 2
// queues (one rx plus one tx), independent of the device backend. Verified
// against cloud-hypervisor 48 and 53 ("Number of queues (N) to virtio_net
// should be higher than 2").
const minNetQueues = 2

// defaultNetQueues is the queue count applied when the parsed config names
// none or one. A single rx/tx pair — the "single queue pair" of REQ-005 —
// maps to two virtqueues in cloud-hypervisor terms, which is also the
// library default.
const defaultNetQueues = minNetQueues

// ParseNetConfig parses a cloud-hypervisor --net device string — the
// comma-separated key=value form rendered by internal/chclient, for example
//
//	vhost_user=true,socket=/run/user/1000/k8snet/m.sock,mac=c6:e5:50:1c:ec:ab,num_queues=1
//
// into the NetConfig JSON structure the vm.create endpoint consumes. The
// argv parameter "socket" maps to the JSON field vhost_socket. A
// num_queues value below 2 is raised to 2 because cloud-hypervisor rejects
// smaller values outright; an absent value yields the library default of 2.
// Unknown keys, empty input, malformed booleans or integers, and a missing
// value are errors.
func ParseNetConfig(netConfig string) (NetConfig, error) {
	cfg := NetConfig{}

	for field := range strings.SplitSeq(netConfig, ",") {
		key, value, found := strings.Cut(field, "=")
		if !found {
			return NetConfig{}, fmt.Errorf("parse net config %q: field %q has no key=value separator", netConfig, field)
		}

		switch key {
		case "vhost_user":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return NetConfig{}, fmt.Errorf("parse net config %q: invalid vhost_user %q: %w", netConfig, value, err)
			}

			cfg.VhostUser = b
		case "socket":
			cfg.VhostSocket = value
		case "mac":
			cfg.MAC = value
		case "num_queues":
			n, err := strconv.Atoi(value)
			if err != nil {
				return NetConfig{}, fmt.Errorf("parse net config %q: invalid num_queues %q: %w", netConfig, value, err)
			}

			if n < minNetQueues {
				n = defaultNetQueues
			}

			cfg.NumQueues = n
		default:
			return NetConfig{}, fmt.Errorf("parse net config %q: unknown key %q", netConfig, key)
		}
	}

	if cfg.NumQueues == 0 {
		cfg.NumQueues = defaultNetQueues
	}

	return cfg, nil
}
