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

package app

import (
	"context"
	"net/http"
	"sync"

	"github.com/nuclio/logger"
	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/loggersink"
	nuclioioclient "github.com/nuclio/nuclio/pkg/platform/kube/client/clientset/versioned"
	"github.com/nuclio/nuclio/pkg/platform/kube/resourcescaler"
	"github.com/nuclio/nuclio/pkg/platformconfig"

	"github.com/nuclio/errors"
	"github.com/v3io/scaler/pkg/dlx"
	"github.com/v3io/scaler/pkg/scalertypes"
	"k8s.io/client-go/rest"

	// load all sinks
	_ "github.com/nuclio/nuclio/pkg/sinks"
)

func Run(platformConfigurationPath string,
	namespace string,
	kubeconfigPath string,
	functionReadinessVerificationEnabled bool) error {

	// create dlx
	dlxInstance, err := newDLX(platformConfigurationPath,
		namespace,
		kubeconfigPath,
		functionReadinessVerificationEnabled)
	if err != nil {
		return errors.Wrap(err, "Failed to create dlx")
	}

	// start dlx and run forever
	if err = dlxInstance.Start(); err != nil {
		return errors.Wrap(err, "Failed to start dlx")
	}
	select {}
}

type scaler struct {
	scalertypes.ResourceScaler

	logger       logger.Logger
	server       *http.Server
	readinessMap sync.Map
}

func New(parentLogger logger.Logger, resourceScaler scalertypes.ResourceScaler, listenAddress string) scalertypes.ResourceScaler {
	childLogger := parentLogger.GetChild("dlx")

	return &scaler{
		ResourceScaler: resourceScaler,
		logger:         childLogger,
		server: &http.Server{
			Addr: listenAddress,
		},
	}
}

func (s *scaler) Start() error {
	s.logger.DebugWith("Starting", "server", s.server.Addr)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		name := r.URL.Query().Get("resource")
		if ch, exist := s.readinessMap.Load(name); exist {
			ch.(chan bool) <- true
		}
		w.WriteHeader(http.StatusOK)
	})
	go s.server.ListenAndServe() // nolint: errcheck
	return nil
}

func (s *scaler) SetScaleCtx(ctx context.Context, resources []scalertypes.Resource, scale int) error {
	if scale > 0 {
		for _, resource := range resources {
			s.readinessMap.Store(resource.Namespace+"::"+resource.Name, make(chan bool, 1))
		}
	}
	defer func() {
		for _, resource := range resources {
			ch, exist := s.readinessMap.Load(resource.Namespace + "::" + resource.Name)

			s.readinessMap.Delete(resource.Namespace + "::" + resource.Name)
			if exist {
				close(ch.(chan bool))
			}
		}
	}()

	return s.ResourceScaler.SetScaleCtx(ctx, resources, scale)
}

func newDLX(platformConfigurationPath string,
	namespace string,
	kubeconfigPath string,
	functionReadinessVerificationEnabled bool) (*dlx.DLX, error) {

	// get platform configuration
	platformConfiguration, err := platformconfig.NewPlatformConfig(platformConfigurationPath)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get platform configuration")
	}

	// create root logger
	rootLogger, err := loggersink.CreateSystemLogger("autoscaler", platformConfiguration)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create logger")
	}

	restConfig, err := common.GetClientConfig(kubeconfigPath)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get client configuration")
	}

	nuclioClientSet, err := nuclioioclient.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create nuclio client set")
	}

	// create resource scaler
	resourceScaler, err := resourcescaler.New(rootLogger, namespace, nuclioClientSet, platformConfiguration)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create resource scaler")
	}

	resourceScaler = New(rootLogger, resourceScaler, ":8088")

	// NOTE: hacky, making sure that argument passes to the struct itself
	// on 1.5.x both dlx/autoscaler were merged onto nuclio from v3io/scaler to stop using v3io/scaler as a plugin
	// this intermediate work status does not allow us (yet) the flexibility to inject
	// arguments via the resourcescaler structure and hence, we will cast it (it is safe) and set the argument.
	resourceScaler.(*resourcescaler.NuclioResourceScaler).SetFunctionReadinessVerificationEnabled(functionReadinessVerificationEnabled)

	// get resource scaler configuration
	resourceScalerConfig, err := resourceScaler.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get resource scaler config")
	}

	// create dlx instance
	dlxInstance, err := dlx.NewDLX(rootLogger, resourceScaler, resourceScalerConfig.DLXOptions)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create dlx instance")
	}

	rest.SetDefaultWarningHandler(common.NewKubernetesClientWarningHandler(rootLogger.GetChild("kube_warnings")))
	return dlxInstance, nil
}
