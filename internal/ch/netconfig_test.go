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

// ParseNetConfig contract (test-first).
//
// This suite pins the translation from the cloud-hypervisor --net argv-form
// device string — rendered by internal/chclient and supplied through
// SetNetConfig — into the NetConfig JSON structure the vm.create endpoint
// consumes. The contract, in prose:
//
//   - Comma-separated key=value fields; "socket" maps to VhostSocket,
//     "vhost_user" parses as a boolean, "mac" is copied verbatim, and
//     "num_queues" parses as an integer.
//   - A num_queues below 2 is raised to 2: cloud-hypervisor rejects any net
//     device with fewer than two virtqueues outright, so a value of 1 (the
//     historic renderer output) can never be honored. An absent num_queues
//     yields 2, the library default.
//   - Unknown keys, fields without "=", malformed booleans, and malformed
//     integers are errors naming the offending input.
package ch_test

import (
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// TestParseNetConfig pins the happy paths: every field of the argv form
// lands in the JSON structure, including the socket->vhost_socket rename and
// the num_queues floor.
func TestParseNetConfig(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		want    ch.NetConfig
		wantErr bool
	}{
		{
			name: "full vhost-user config with single queue pair",
			give: "vhost_user=true,socket=/run/user/1000/k8snet/node-1.sock,mac=c6:e5:50:1c:ec:ab,num_queues=1",
			want: ch.NetConfig{
				VhostUser:   true,
				VhostSocket: "/run/user/1000/k8snet/node-1.sock",
				MAC:         "c6:e5:50:1c:ec:ab",
				NumQueues:   2,
			},
		},
		{
			name: "explicit two queues preserved",
			give: "vhost_user=true,socket=/run/user/1000/k8snet/n.sock,mac=aa:bb:cc:dd:ee:ff,num_queues=4",
			want: ch.NetConfig{
				VhostUser:   true,
				VhostSocket: "/run/user/1000/k8snet/n.sock",
				MAC:         "aa:bb:cc:dd:ee:ff",
				NumQueues:   4,
			},
		},
		{
			name: "absent num queues defaults to two",
			give: "vhost_user=true,socket=/run/user/1000/k8snet/n.sock,mac=aa:bb:cc:dd:ee:ff",
			want: ch.NetConfig{
				VhostUser:   true,
				VhostSocket: "/run/user/1000/k8snet/n.sock",
				MAC:         "aa:bb:cc:dd:ee:ff",
				NumQueues:   2,
			},
		},
		{
			name:    "unknown key",
			give:    "vhost_user=true,tap=tap0",
			wantErr: true,
		},
		{
			name:    "field without separator",
			give:    "vhost_user=true,socket",
			wantErr: true,
		},
		{
			name:    "malformed boolean",
			give:    "vhost_user=yes",
			wantErr: true,
		},
		{
			name:    "malformed integer",
			give:    "vhost_user=true,num_queues=two",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ch.ParseNetConfig(tt.give)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseNetConfig(%q) = %+v, want error", tt.give, got)
				}

				if !strings.Contains(err.Error(), tt.give) {
					t.Errorf("error %v does not name the input", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseNetConfig(%q) error = %v, want nil", tt.give, err)
			}

			if got != tt.want {
				t.Errorf("ParseNetConfig(%q) = %+v, want %+v", tt.give, got, tt.want)
			}
		})
	}
}
