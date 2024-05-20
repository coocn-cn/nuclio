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
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/nuclio/errors"
	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/platform/kube"

	"github.com/nuclio/logger"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type TCPHandler struct {
	logger             logger.Logger
	namespace          string
	kubeClientSet      *kubernetes.Clientset
	resourceStarter    *ResourceStarter
	lastProxyErrorTime time.Time
	HandleFunc         func(context.Context, net.Conn)
}

func NewTCPHandler(parentLogger logger.Logger,
	namespace string,
	kubeClientSet *kubernetes.Clientset,
	resourceStarter *ResourceStarter) (TCPHandler, error) {

	h := TCPHandler{
		logger:             parentLogger.GetChild("handler"),
		namespace:          namespace,
		kubeClientSet:      kubeClientSet,
		resourceStarter:    resourceStarter,
		lastProxyErrorTime: time.Now(),
	}
	h.HandleFunc = h.handleRequest
	return h, nil
}

func (h *TCPHandler) handleRequest(ctx context.Context, conn net.Conn) {
	var service *v1.Service
	var resourceNames []string

	addr, err := h.getOriginalDst(conn)
	if err != nil {
		h.logger.WarnWith("Failed to get original address",
			"conn", conn)
		return
	}

	nuclioResourceLabelKeyScaleToZeroPort := fmt.Sprintf("%s-%d", NuclioResourceLabelKeyScaleToZeroPort, addr.Port())

	serviceList, serviceErr := h.kubeClientSet.CoreV1().
		Services(h.namespace).
		List(ctx, metav1.ListOptions{LabelSelector: nuclioResourceLabelKeyScaleToZeroPort})

	if serviceErr != nil {
		h.logger.WarnWith("Failed to found service for address",
			"address", addr)
		return
	}

	// there should be only one
	if len(serviceList.Items) != 1 {
		h.logger.WarnWith("Found unexpected number of services for address",
			"addr", addr,
			"serviceList", serviceList.Items)
		return
	}

	service = &serviceList.Items[0]
	functionName, exist := service.Labels[common.NuclioResourceLabelKeyFunctionName]
	if !exist {
		h.logger.WarnWith("When function name label not set, must set function name",
			"labelName", nuclioResourceLabelKeyScaleToZeroPort,
			"service", service)
		return
	}

	functionPort, exist := service.Labels[nuclioResourceLabelKeyScaleToZeroPort]
	if !exist {
		h.logger.WarnWith("When port label not set, must set port",
			"labelName", nuclioResourceLabelKeyScaleToZeroPort,
			"service", service)
		return
	}

	resourceNames = append(resourceNames, functionName)

	statusResult := h.startResources(resourceNames)

	if statusResult != nil && statusResult.Error != nil {
		h.logger.WarnWith("Failed to start resource",
			"addr", addr,
			"error", statusResult.Error)
		return
	}

	funcationAddr := fmt.Sprintf("%s.%s.svc.cluster.local:%s",
		kube.ServiceNameFromFunctionName(functionName),
		h.namespace,
		functionPort)

	var dial net.Dialer
	dstConn, err := dial.DialContext(ctx, "tcp", funcationAddr)
	if err != nil {
		timeSinceLastCtxErr := time.Since(h.lastProxyErrorTime).Hours() > 1
		if strings.Contains(err.Error(), "context canceled") && timeSinceLastCtxErr {
			h.lastProxyErrorTime = time.Now()
		}
		if !strings.Contains(err.Error(), "context canceled") || timeSinceLastCtxErr {
			h.logger.DebugWith("tcp: proxy error", "error", err)
		}

		return
	}
	defer dstConn.Close()

	go io.Copy(conn, dstConn)
	io.Copy(dstConn, conn)
}

func (h *TCPHandler) getOriginalDst(conn net.Conn) (*netip.AddrPort, error) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		file, err := tcpConn.File()
		if err != nil {
			return nil, err
		}
		defer file.Close()

		var ip net.IP
		var port uint16

		fd := int(file.Fd())
		const SO_ORIGINAL_DST = 80
		raddr, err := syscall.GetsockoptIPv6Mreq(fd, syscall.IPPROTO_IP, SO_ORIGINAL_DST)
		if err != nil {
			// 如果没取到, 则说明是原始目标IP就是本机
			addr := tcpConn.LocalAddr()
			tcpAddr, _ := net.ResolveTCPAddr(addr.Network(), addr.String())
			ip = tcpAddr.IP
			port = uint16(tcpAddr.Port)
		} else {

			// 实际ipv6Mreq这块内存被填充的是Linux C层面的结构体
			//struct sockaddr_in {
			//	sa_family_t    sin_family; /* address family: AF_INET */
			//	in_port_t      sin_port;   /* port in network byte order */
			//struct in_addr sin_addr;   /* internet address */
			//};
			//
			//	/* Internet address. */
			//struct in_addr {
			//	uint32_t       s_addr;     /* address in network byte order */
			//};

			// 所以4:8是sin_addr字段，也就是大端字节序的ipv4地址
			ip = net.IP(raddr.Multiaddr[4:8])
			// 所以2:4是sin_port字段。也就是大端字节序的端口
			port = uint16(binary.BigEndian.Uint16(raddr.Multiaddr[2:4]))
		}

		na, _ := netip.AddrFromSlice(ip)
		addr := netip.AddrPortFrom(na, port)
		return &addr, nil
	}

	return nil, fmt.Errorf("unsupported connection type %T", conn)
}

func (h *TCPHandler) startResources(resourceNames []string) *ResourceStatusResult {
	responseChannel := make(chan ResourceStatusResult, len(resourceNames))
	defer close(responseChannel)

	// Start all resources in separate go routines
	for _, resourceName := range resourceNames {
		go h.resourceStarter.handleResourceStart(resourceName, responseChannel)
	}

	// Wait for all resources to finish starting
	for range resourceNames {
		statusResult := <-responseChannel

		if statusResult.Error != nil {
			h.logger.WarnWith("Failed to start resource",
				"resource", statusResult.ResourceName,
				"err", errors.GetErrorStackString(statusResult.Error, 10))
			return &statusResult
		}
	}

	return nil
}
