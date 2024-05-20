/*
Copyright 2019 Iguazio Systems Ltd.

Licensed under the Apache License, Version 2.0 (the "License") with
an addition restriction as set forth herein. You may not use this
file except in compliance with the License. You may obtain a copy of
the License at http://www.apache.org/licenses/LICENSE-2.0.

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.

In addition, you may not use the software for any purposes that are
illegal under applicable law, and the grant of the foregoing license
under the Apache 2.0 license is conditioned upon your compliance with
such restriction.
*/

package dlx

import (
	"context"
	"net"
	"strconv"

	"github.com/coreos/go-iptables/iptables"
	"github.com/v3io/scaler/pkg/scalertypes"
	"k8s.io/client-go/kubernetes"

	"github.com/nuclio/errors"
	"github.com/nuclio/logger"
	"github.com/v3io/scaler/pkg/dlx"
)

type DLX struct {
	*dlx.DLX
	logger            logger.Logger
	iptables          *iptables.IPTables
	iptableChainName  string
	tcpListen         net.Listener
	tcpHandler        TCPHandler
	tcpListenAddress  string
	httpListenAddress string
}

func NewDLX(parentLogger logger.Logger,
	kubeClientSet *kubernetes.Clientset,
	resourceScaler scalertypes.ResourceScaler,
	options DLXOptions) (*DLX, error) {
	childLogger := parentLogger.GetChild("dlx")
	childLogger.InfoWith("Creating DLX",
		"options", options)

	goIptables, err := iptables.New()
	if err != nil {
		return nil, err
	}

	orginalDlx, err := dlx.NewDLX(parentLogger,
		resourceScaler,
		options.DLXOptions)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create function starter")
	}

	resourceStarter, err := NewResourceStarter(childLogger,
		resourceScaler,
		options.Namespace,
		options.ResourceReadinessTimeout.Duration)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create function starter")
	}

	tcpHandler, err := NewTCPHandler(childLogger,
		options.Namespace,
		kubeClientSet,
		resourceStarter)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create handler")
	}

	return &DLX{
		DLX:               orginalDlx,
		logger:            childLogger,
		iptables:          goIptables,
		iptableChainName:  "NUCLIO_DLX_INBOUND",
		tcpListen:         nil,
		tcpHandler:        tcpHandler,
		tcpListenAddress:  options.TCPListenAddress,
		httpListenAddress: options.ListenAddress,
	}, nil
}

func (d *DLX) Start() error {
	err := d.DLX.Start()
	if err != nil {
		return err
	}

	ctx := context.Background()
	d.logger.DebugWith("Starting", "tcp server", d.tcpListenAddress)
	d.tcpListen, err = net.Listen("tcp", d.tcpListenAddress)
	if err != nil {
		return errors.Wrap(err, "Failed to create tcp listen")
	}

	if err := d.setIptable(ctx, true, true); err != nil {
		return errors.Wrap(err, "Failed to create socket redirect rules")
	}

	go func() {
		for {
			conn, err := d.tcpListen.Accept()
			if err != nil {
				d.logger.Warn("Failed to accept tcp listen", "err", err)
				continue
			}

			go func() {
				defer conn.Close()
				d.tcpHandler.HandleFunc(ctx, conn)
			}() // nolint: errcheck
		}
	}()

	return nil
}

func (d *DLX) setIptable(_ context.Context, delete bool, create bool) (err error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", d.tcpListenAddress)
	if err != nil {
		return errors.Wrap(err, "Failed to Resolve http listen address")
	}
	tcpRedirectPort := strconv.Itoa(tcpAddr.Port)

	httpAddr, err := net.ResolveTCPAddr("tcp", d.httpListenAddress)
	if err != nil {
		return errors.Wrap(err, "Failed to Resolve http listen address")
	}
	httpRedirectPort := strconv.Itoa(httpAddr.Port)

	if delete {
		exist, err := d.iptables.ChainExists("nat", d.iptableChainName)
		if err != nil {
			return errors.Wrap(err, "Failed to delete iptable chain "+d.iptableChainName)
		}

		if exist {
			err = d.iptables.DeleteIfExists("nat", "PREROUTING", "-p", "tcp", "-j", d.iptableChainName)
			if err != nil {
				return errors.Wrap(err, "Failed to delete iptable redirect rule from PREROUTING")
			}

			err = d.iptables.ClearAndDeleteChain("nat", d.iptableChainName)
			if err != nil {
				return errors.Wrap(err, "Failed to delete iptable chain "+d.iptableChainName)
			}
		}
	}

	if create {
		err = d.iptables.NewChain("nat", d.iptableChainName)
		if err != nil {
			return errors.Wrap(err, "Failed to create iptable chain "+d.iptableChainName)
		}
		err = d.iptables.Append("nat", "PREROUTING", "-p", "tcp", "-j", d.iptableChainName)
		if err != nil {
			return errors.Wrap(err, "Failed to create iptable redirect rule to PREROUTING")
		}
		err = d.iptables.Append("nat", d.iptableChainName, "-p", "tcp", "--dport", tcpRedirectPort, "-j", "RETURN")
		if err != nil {
			return errors.Wrap(err, "Failed to create iptable redirect rule to "+d.iptableChainName)
		}
		err = d.iptables.Append("nat", d.iptableChainName, "-p", "tcp", "--dport", httpRedirectPort, "-j", "RETURN")
		if err != nil {
			return errors.Wrap(err, "Failed to create iptable redirect rule to "+d.iptableChainName)
		}
		err = d.iptables.Append("nat", d.iptableChainName, "-p", "tcp", "-j", "REDIRECT", "--to-port", tcpRedirectPort)
		if err != nil {
			return errors.Wrap(err, "Failed to create iptable redirect rule to "+d.iptableChainName)
		}
	}

	return nil
}

func (d *DLX) Stop(ctx context.Context) (err error) {
	d.logger.DebugWith("Stopping", "tcp server", d.tcpListenAddress)

	if err := d.setIptable(ctx, true, false); err != nil {
		d.logger.WarnWithCtx(ctx, "Failed to clean socket redirect rules")
	}

	if err := d.tcpListen.Close(); err != nil {
		d.logger.WarnWithCtx(ctx, "Failed to close tcp listen")
	}

	return d.DLX.Stop(ctx)
}
