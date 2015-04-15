// Copyright 2015 CoreOS, Inc.
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

package stage0

import (
	"encoding/json"
	"fmt"
	"io/ioutil"

	"github.com/coreos/rkt/Godeps/_workspace/src/github.com/appc/spec/schema"
	"github.com/coreos/rkt/common"
)

const (
	enterEntrypoint = "coreos.com/rkt/stage1/enter"
	runEntrypoint   = "coreos.com/rkt/stage1/run"
)

// getEntrypoint retrieves the named entrypoint from the stage1 manifest for a given pod
func getStage1Entrypoint(cdir string, entrypoint string) (string, error) {
	b, err := ioutil.ReadFile(common.Stage1ManifestPath(cdir))
	if err != nil {
		return "", fmt.Errorf("error reading pod manifest: %v", err)
	}

	s1m := schema.ImageManifest{}
	if err := json.Unmarshal(b, &s1m); err != nil {
		return "", fmt.Errorf("error unmarshaling stage1 manifest: %v", err)
	}

	if ep, ok := s1m.Annotations.Get(entrypoint); ok {
		return ep, nil
	}

	return "", fmt.Errorf("entrypoint %q not found", entrypoint)
}
