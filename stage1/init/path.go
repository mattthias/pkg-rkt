// Copyright 2014 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//+build linux

package main

import (
	"path/filepath"
	"strings"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema/types"
	"github.com/coreos/rkt/common"
)

const (
	envDir          = "/rkt/env" // TODO(vc): perhaps this doesn't belong in /rkt?
	unitsDir        = "/usr/lib/systemd/system"
	defaultWantsDir = unitsDir + "/default.target.wants"
	socketsWantsDir = unitsDir + "/sockets.target.wants"
)

// ServiceUnitName returns a systemd service unit name for the given imageID
func ServiceUnitName(imageID types.Hash) string {
	return types.ShortHash(imageID.String()) + ".service"
}

// ServiceUnitPath returns the path to the systemd service file for the given
// imageID
func ServiceUnitPath(root string, imageID types.Hash) string {
	return filepath.Join(common.Stage1RootfsPath(root), unitsDir, ServiceUnitName(imageID))
}

// RelEnvFilePath returns the path to the environment file for the given imageID
// relative to the pod's root
func RelEnvFilePath(imageID types.Hash) string {
	return filepath.Join(envDir, types.ShortHash(imageID.String()))
}

// EnvFilePath returns the path to the environment file for the given imageID
func EnvFilePath(root string, imageID types.Hash) string {
	return filepath.Join(common.Stage1RootfsPath(root), RelEnvFilePath(imageID))
}

// ServiceWantPath returns the systemd default.target want symlink path for the
// given imageID
func ServiceWantPath(root string, imageID types.Hash) string {
	return filepath.Join(common.Stage1RootfsPath(root), defaultWantsDir, ServiceUnitName(imageID))
}

// InstantiatedPrepareAppUnitName returns the systemd service unit name for prepare-app
// instantiated for the given root
func InstantiatedPrepareAppUnitName(imageID types.Hash) string {
	// Naming respecting escaping rules, see systemd.unit(5) and systemd-escape(1)
	escaped_root := common.RelAppRootfsPath(imageID)
	escaped_root = strings.Replace(escaped_root, "-", "\\x2d", -1)
	escaped_root = strings.Replace(escaped_root, "/", "-", -1)
	return "prepare-app@" + escaped_root + ".service"
}

// SocketUnitName returns a systemd socket unit name for the given imageID
func SocketUnitName(imageID types.Hash) string {
	return imageID.String() + ".socket"
}

// SocketUnitPath returns the path to the systemd socket file for the given imageID
func SocketUnitPath(root string, imageID types.Hash) string {
	return filepath.Join(common.Stage1RootfsPath(root), unitsDir, SocketUnitName(imageID))
}

// SocketWantPath returns the systemd sockets.target.wants symlink path for the
// given imageID
func SocketWantPath(root string, imageID types.Hash) string {
	return filepath.Join(common.Stage1RootfsPath(root), socketsWantsDir, SocketUnitName(imageID))
}
