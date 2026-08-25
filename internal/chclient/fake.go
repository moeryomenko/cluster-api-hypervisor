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

package chclient

import (
	"context"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// FakeClient is a programmable Client for controller tests. Its behaviour is
// fully controlled through exported fields and every call is appended to
// Calls: State holds the VM state Info reports (for example "Running" or
// "Shutdown"); InfoErr, EnsureRunningErr, ShutdownErr and StopErr hold the
// error each operation returns when set, and the operation succeeds when the
// field is nil. Calls records each invoked operation by name in call order:
// "EnsureRunning", "Shutdown", "Stop", "Info". NetConfigs records every net
// device string supplied through SetNetConfig, Firmwares every firmware path
// supplied through SetFirmware, and DiskPathSets every disk path list
// supplied through SetDiskPaths, in call order. The zero value is usable:
// every operation succeeds and no state is reported.
type FakeClient struct {
	State            ch.VMState
	EnsureRunningErr error
	ShutdownErr      error
	StopErr          error
	InfoErr          error
	Calls            []string
	NetConfigs       []string
	Firmwares        []string
	DiskPathSets     [][]string
}

// SetNetConfig records the net device string for assertions.
func (f *FakeClient) SetNetConfig(netConfig string) {
	f.NetConfigs = append(f.NetConfigs, netConfig)
}

// SetFirmware records the firmware path for assertions.
func (f *FakeClient) SetFirmware(firmware string) {
	f.Firmwares = append(f.Firmwares, firmware)
}

// SetDiskPaths records the disk path list for assertions.
func (f *FakeClient) SetDiskPaths(paths []string) {
	f.DiskPathSets = append(f.DiskPathSets, paths)
}

// EnsureRunning records the call and returns the configured error, if any.
func (f *FakeClient) EnsureRunning(context.Context) error {
	f.Calls = append(f.Calls, "EnsureRunning")
	return f.EnsureRunningErr
}

// Shutdown records the call and returns the configured error, if any.
func (f *FakeClient) Shutdown(context.Context) error {
	f.Calls = append(f.Calls, "Shutdown")
	return f.ShutdownErr
}

// Stop records the call and returns the configured error, if any.
func (f *FakeClient) Stop(context.Context) error {
	f.Calls = append(f.Calls, "Stop")
	return f.StopErr
}

// Info records the call and reports the configured State, or the configured
// InfoErr when one is set. On error the reported state is empty.
func (f *FakeClient) Info(context.Context) (ch.VMState, error) {
	f.Calls = append(f.Calls, "Info")
	if f.InfoErr != nil {
		return "", f.InfoErr
	}
	return f.State, nil
}
