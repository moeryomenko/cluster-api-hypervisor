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

// Published endpoint status contract for HypervisorMachine (test-first, RED).
//
// REQ-008 / TASK-013: HypervisorMachineStatus gains
//
//	PublishedPorts []MachinePublishedPort `json:"publishedPorts,omitempty"`
//
// with
//
//	type MachinePublishedPort struct {
//	    VMPort   int32 `json:"vmPort"`
//	    HostPort int32 `json:"hostPort"`
//	}
//
// The machine controller records the k8netd PublishPort allocations there and
// the control-plane controller reads the 6443 entry to render the kubeconfig
// server URL.
//
// This file is RED: neither MachinePublishedPort nor Status.PublishedPorts
// exists yet, so the package does not compile ("undefined:
// v1alpha1.MachinePublishedPort"). The deepcopy assertions expect the
// regenerated zz_generated.deepcopy.go to deep-copy the new slice.
package v1alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"

	apiequality "k8s.io/apimachinery/pkg/api/equality"

	"github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// machineWithPublishedPorts builds a HypervisorMachine whose status carries
// the two control-plane allocations (apiserver 6443 and SSH 22).
func machineWithPublishedPorts() *v1alpha1.HypervisorMachine {
	return &v1alpha1.HypervisorMachine{
		Status: v1alpha1.HypervisorMachineStatus{
			PublishedPorts: []v1alpha1.MachinePublishedPort{
				{VMPort: 6443, HostPort: 26443},
				{VMPort: 22, HostPort: 20022},
			},
		},
	}
}

// TestHypervisorMachinePublishedPortsJSONShape pins the serialized shape of
// the published endpoints: status.publishedPorts is a list of objects with
// exactly the vmPort and hostPort keys, absent entirely when no ports are
// published (omitempty).
func TestHypervisorMachinePublishedPortsJSONShape(t *testing.T) {
	t.Run("published entries serialize under status.publishedPorts", func(t *testing.T) {
		raw, err := json.Marshal(machineWithPublishedPorts())
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc struct {
			Status struct {
				PublishedPorts []map[string]json.RawMessage `json:"publishedPorts"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}

		if len(doc.Status.PublishedPorts) != 2 {
			t.Fatalf("status.publishedPorts = %s, want 2 entries", string(raw))
		}

		want := []map[string]int32{
			{"vmPort": 6443, "hostPort": 26443},
			{"vmPort": 22, "hostPort": 20022},
		}

		for i, entry := range doc.Status.PublishedPorts {
			if len(entry) != 2 {
				t.Errorf("publishedPorts[%d] carries %d keys (%s), want exactly vmPort and hostPort", i, len(entry), entry)
			}

			for _, key := range []string{"vmPort", "hostPort"} {
				if _, ok := entry[key]; !ok {
					t.Errorf("publishedPorts[%d] missing key %q (entry %s)", i, key, entry)
				}
			}

			var got struct {
				VMPort   int32 `json:"vmPort"`
				HostPort int32 `json:"hostPort"`
			}
			if err := json.Unmarshal(mustJSON(t, entry), &got); err != nil {
				t.Fatalf("unmarshal publishedPorts[%d]: %v", i, err)
			}

			if got.VMPort != want[i]["vmPort"] || got.HostPort != want[i]["hostPort"] {
				t.Errorf(
					"publishedPorts[%d] = {vmPort:%d hostPort:%d}, want {vmPort:%d hostPort:%d}",
					i, got.VMPort, got.HostPort, want[i]["vmPort"], want[i]["hostPort"],
				)
			}
		}
	})

	t.Run("no published ports are omitted", func(t *testing.T) {
		raw, err := json.Marshal(&v1alpha1.HypervisorMachine{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}

		status, ok := doc["status"]
		if !ok {
			return // whole status omitted; publishedPorts cannot leak
		}

		var statusDoc map[string]json.RawMessage
		if err := json.Unmarshal(status, &statusDoc); err != nil {
			t.Fatalf("unmarshal status: %v", err)
		}

		if _, present := statusDoc["publishedPorts"]; present {
			t.Errorf("empty status serializes publishedPorts (%s), want it omitted", status)
		}
	})
}

// TestHypervisorMachinePublishedPortsRoundTrip verifies a populated
// PublishedPorts list survives a marshal/unmarshal cycle unchanged.
func TestHypervisorMachinePublishedPortsRoundTrip(t *testing.T) {
	give := machineWithPublishedPorts()

	raw, err := json.Marshal(give)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got v1alpha1.HypervisorMachine
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(%s): %v", raw, err)
	}

	if !apiequality.Semantic.DeepEqual(&got, give) {
		t.Errorf("round trip mismatch:\nwant: %#v\ngot:  %#v", give, &got)
	}
}

// TestHypervisorMachinePublishedPortsDeepCopyNonAliasing verifies that
// DeepCopyObject returns a fully independent object: mutating the copy's
// published-ports slice must not touch the original. This pins the
// regenerated zz_generated.deepcopy.go handling of the new field.
func TestHypervisorMachinePublishedPortsDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.HypervisorMachine)
	}{
		{
			name: "status.publishedPorts append",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.PublishedPorts = append(m.Status.PublishedPorts, v1alpha1.MachinePublishedPort{
					VMPort:   2379,
					HostPort: 22379,
				})
			},
		},
		{
			name: "status.publishedPorts element mutation",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.PublishedPorts[0].HostPort = 1
			},
		},
		{
			name: "status.publishedPorts replaced by nil",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.PublishedPorts = nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := machineWithPublishedPorts()
			obj := original.DeepCopyObject()

			copyObj, ok := obj.(*v1alpha1.HypervisorMachine)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *v1alpha1.HypervisorMachine", obj)
			}

			if copyObj == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}

			if !reflect.DeepEqual(copyObj, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copyObj, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := machineWithPublishedPorts()

			tt.mutate(copyObj)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}

// mustJSON re-marshals a decoded JSON value.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}

	return raw
}
