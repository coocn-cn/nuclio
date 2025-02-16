/*
Copyright 2023 The Nuclio Authors.

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

package ruby

import (
	"fmt"

	"github.com/nuclio/nuclio/pkg/processor/build/runtime"
	"github.com/nuclio/nuclio/pkg/processor/build/runtimeconfig"
)

type ruby struct {
	*runtime.AbstractRuntime
}

// GetName returns the name of the runtime, including version if applicable
func (r *ruby) GetName() string {
	return "ruby"
}

// GetProcessorDockerfileInfo returns information required to build the processor Dockerfile
func (r *ruby) GetProcessorDockerfileInfo(runtimeConfig *runtimeconfig.Config, onbuildImageRegistry string) (*runtime.ProcessorDockerfileInfo, error) {

	processorDockerfileInfo := runtime.ProcessorDockerfileInfo{}

	processorDockerfileInfo.BaseImage = "ruby:2.4.4-alpine"

	processorDockerfileInfo.ImageArtifactPaths = map[string]string{
		"handler": "/opt/nuclio",
	}

	// fill onbuild artifact
	artifact := runtime.Artifact{
		Name: "ruby-onbuild",
		Image: fmt.Sprintf("%s/coocn/handler-builder-ruby-onbuild:%s-%s",
			onbuildImageRegistry,
			r.VersionInfo.Label,
			r.VersionInfo.Arch),
		Paths: map[string]string{
			"/home/nuclio/bin/processor":  "/usr/local/bin/processor",
			"/home/nuclio/bin/wrapper.rb": "/opt/nuclio/wrapper.rb",
		},
	}
	processorDockerfileInfo.OnbuildArtifacts = []runtime.Artifact{artifact}

	return &processorDockerfileInfo, nil
}
