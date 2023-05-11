package rtcconfig

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/logger/pionlogger"
	"github.com/pion/ice/v2"
	"github.com/pion/webrtc/v3"
)

type WebRTCConfig struct {
	Configuration  webrtc.Configuration
	SettingEngine  webrtc.SettingEngine
	UDPMux         ice.UDPMux
	TCPMuxListener *net.TCPListener
	NAT1To1IPs     []string
	UseMDNS        bool
}

func NewWebRTCConfig(rtcConf *RTCConfig, development bool) (*WebRTCConfig, error) {
	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	s := webrtc.SettingEngine{
		LoggerFactory: pionlogger.NewLoggerFactory(logger.GetLogger()),
	}

	var ifFilter func(string) bool
	if len(rtcConf.Interfaces.Includes) != 0 || len(rtcConf.Interfaces.Excludes) != 0 {
		ifFilter = InterfaceFilterFromConf(rtcConf.Interfaces)
		s.SetInterfaceFilter(ifFilter)
	}

	var ipFilter func(net.IP) bool
	if len(rtcConf.IPs.Includes) != 0 || len(rtcConf.IPs.Excludes) != 0 {
		filter, err := IPFilterFromConf(rtcConf.IPs)
		if err != nil {
			return nil, err
		}
		ipFilter = filter
		s.SetIPFilter(filter)
	}

	if !rtcConf.UseMDNS {
		s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	var nat1to1IPs []string
	// force it to the node IPs that the user has set
	if rtcConf.NodeIP != "" && (rtcConf.UseExternalIP || !rtcConf.NodeIPAutoGenerated) {
		if rtcConf.UseExternalIP {
			ips, err := getNAT1to1IPsForConf(rtcConf, ipFilter)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				logger.Infow("no external IPs found, using node IP for NAT1To1Ips", "ip", rtcConf.NodeIP)
				s.SetNAT1To1IPs([]string{rtcConf.NodeIP}, webrtc.ICECandidateTypeHost)
			} else {
				logger.Infow("using external IPs", "ips", ips)
				s.SetNAT1To1IPs(ips, webrtc.ICECandidateTypeHost)
			}
			nat1to1IPs = ips
		} else {
			s.SetNAT1To1IPs([]string{rtcConf.NodeIP}, webrtc.ICECandidateTypeHost)
		}
	}

	var udpMux ice.UDPMux
	var err error
	networkTypes := make([]webrtc.NetworkType, 0, 4)

	if !rtcConf.ForceTCP {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
		)
		if rtcConf.ICEPortRangeStart != 0 && rtcConf.ICEPortRangeEnd != 0 {
			if err := s.SetEphemeralUDPPortRange(uint16(rtcConf.ICEPortRangeStart), uint16(rtcConf.ICEPortRangeEnd)); err != nil {
				return nil, err
			}
		} else if rtcConf.UDPPort != 0 {
			opts := []ice.UDPMuxFromPortOption{
				ice.UDPMuxFromPortWithReadBufferSize(defaultUDPBufferSize),
				ice.UDPMuxFromPortWithWriteBufferSize(defaultUDPBufferSize),
				ice.UDPMuxFromPortWithLogger(s.LoggerFactory.NewLogger("udp_mux")),
			}
			if rtcConf.EnableLoopbackCandidate {
				opts = append(opts, ice.UDPMuxFromPortWithLoopback())
			}
			if ipFilter != nil {
				opts = append(opts, ice.UDPMuxFromPortWithIPFilter(ipFilter))
			}
			if ifFilter != nil {
				opts = append(opts, ice.UDPMuxFromPortWithInterfaceFilter(ifFilter))
			}
			udpMux, err := ice.NewMultiUDPMuxFromPort(int(rtcConf.UDPPort), opts...)
			if err != nil {
				return nil, err
			}

			s.SetICEUDPMux(udpMux)
			if !development {
				checkUDPReadBuffer()
			}
		}
	}

	// use TCP mux when it's set
	var tcpListener *net.TCPListener
	if rtcConf.TCPPort != 0 {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		)
		tcpListener, err = net.ListenTCP("tcp", &net.TCPAddr{
			Port: int(rtcConf.TCPPort),
		})
		if err != nil {
			return nil, err
		}

		tcpMux := ice.NewTCPMuxDefault(ice.TCPMuxParams{
			Logger:          s.LoggerFactory.NewLogger("tcp_mux"),
			Listener:        tcpListener,
			ReadBufferSize:  readBufferSize,
			WriteBufferSize: writeBufferSizeInBytes,
		})

		s.SetICETCPMux(tcpMux)
	}

	if len(networkTypes) == 0 {
		return nil, errors.New("TCP is forced but not configured")
	}
	s.SetNetworkTypes(networkTypes)

	if rtcConf.EnableLoopbackCandidate {
		s.SetIncludeLoopbackCandidate(true)
	}

	if rtcConf.UseICELite {
		s.SetLite(true)
	} else if rtcConf.NodeIP == "" && !rtcConf.UseExternalIP {
		// use STUN servers for server to support NAT
		// when deployed in production, we expect UseExternalIP to be used, and ports accessible
		// this is not compatible with ICE Lite
		// Do not automatically add STUN servers if nodeIP is set
		if len(rtcConf.STUNServers) > 0 {
			c.ICEServers = []webrtc.ICEServer{iceServerForStunServers(rtcConf.STUNServers)}
		} else {
			c.ICEServers = []webrtc.ICEServer{iceServerForStunServers(DefaultStunServers)}
		}
	}

	return &WebRTCConfig{
		Configuration:  c,
		SettingEngine:  s,
		UDPMux:         udpMux,
		TCPMuxListener: tcpListener,
		NAT1To1IPs:     nat1to1IPs,
		UseMDNS:        rtcConf.UseMDNS,
	}, nil
}

func iceServerForStunServers(servers []string) webrtc.ICEServer {
	iceServer := webrtc.ICEServer{}
	for _, stunServer := range servers {
		iceServer.URLs = append(iceServer.URLs, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}

func getNAT1to1IPsForConf(rtcConf *RTCConfig, ipFilter func(net.IP) bool) ([]string, error) {
	stunServers := rtcConf.STUNServers
	if len(stunServers) == 0 {
		stunServers = DefaultStunServers
	}
	localIPs, err := GetLocalIPAddresses(rtcConf.EnableLoopbackCandidate)
	if err != nil {
		return nil, err
	}
	type ipmapping struct {
		externalIP string
		localIP    string
	}
	addrCh := make(chan ipmapping, len(localIPs))

	var udpPorts []int
	if rtcConf.ICEPortRangeStart != 0 && rtcConf.ICEPortRangeEnd != 0 {
		portRangeStart, portRangeEnd := uint16(rtcConf.ICEPortRangeStart), uint16(rtcConf.ICEPortRangeEnd)
		for i := 0; i < 5; i++ {
			udpPorts = append(udpPorts, rand.Intn(int(portRangeEnd-portRangeStart))+int(portRangeStart))
		}
	} else if rtcConf.UDPPort != 0 {
		udpPorts = append(udpPorts, int(rtcConf.UDPPort))
	} else {
		udpPorts = append(udpPorts, 0)
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	for _, ip := range localIPs {
		if ipFilter != nil && !ipFilter(net.ParseIP(ip)) {
			continue
		}

		wg.Add(1)
		go func(localIP string) {
			defer wg.Done()
			for _, port := range udpPorts {
				addr, err := GetExternalIP(ctx, stunServers, &net.UDPAddr{IP: net.ParseIP(localIP), Port: port})
				if err != nil {
					if strings.Contains(err.Error(), "address already in use") {
						logger.Debugw("failed to get external ip, address already in use", "local", localIP, "port", port)
						continue
					}
					logger.Infow("failed to get external ip", "local", localIP, "err", err)
					return
				}
				addrCh <- ipmapping{externalIP: addr, localIP: localIP}
				return
			}
			logger.Infow("failed to get external ip after all ports tried", "local", localIP, "ports", udpPorts)
		}(ip)
	}

	var firstResolved bool
	natMapping := make(map[string]string)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

done:
	for {
		select {
		case mapping := <-addrCh:
			if !firstResolved {
				firstResolved = true
				timeout.Reset(1 * time.Second)
			}
			if local, ok := natMapping[mapping.externalIP]; ok {
				logger.Infow("external ip already solved, ignore duplicate",
					"external", mapping.externalIP,
					"local", local,
					"ignore", mapping.localIP)
			} else {
				natMapping[mapping.externalIP] = mapping.localIP
			}

		case <-timeout.C:
			break done
		}
	}
	cancel()
	wg.Wait()

	if len(natMapping) == 0 {
		// no external ip resolved
		return nil, nil
	}

	// mapping unresolved local ip to itself
	for _, local := range localIPs {
		var found bool
		for _, localIPMapping := range natMapping {
			if local == localIPMapping {
				found = true
				break
			}
		}
		if !found {
			natMapping[local] = local
		}
	}

	nat1to1IPs := make([]string, 0, len(natMapping))
	for external, local := range natMapping {
		nat1to1IPs = append(nat1to1IPs, fmt.Sprintf("%s/%s", external, local))
	}
	return nat1to1IPs, nil
}

func InterfaceFilterFromConf(ifs InterfacesConfig) func(string) bool {
	includes := ifs.Includes
	excludes := ifs.Excludes
	return func(s string) bool {
		// filter by include interfaces
		if len(includes) > 0 {
			for _, iface := range includes {
				if iface == s {
					return true
				}
			}
			return false
		}

		// filter by exclude interfaces
		if len(excludes) > 0 {
			for _, iface := range excludes {
				if iface == s {
					return false
				}
			}
		}
		return true
	}
}

func IPFilterFromConf(ips IPsConfig) (func(ip net.IP) bool, error) {
	var ipnets [2][]*net.IPNet
	var err error
	for i, ips := range [][]string{ips.Includes, ips.Excludes} {
		ipnets[i], err = func(fromIPs []string) ([]*net.IPNet, error) {
			var toNets []*net.IPNet
			for _, ip := range fromIPs {
				_, ipnet, err := net.ParseCIDR(ip)
				if err != nil {
					return nil, err
				}
				toNets = append(toNets, ipnet)
			}
			return toNets, nil
		}(ips)

		if err != nil {
			return nil, err
		}
	}

	includes, excludes := ipnets[0], ipnets[1]

	return func(ip net.IP) bool {
		if len(includes) > 0 {
			for _, ipn := range includes {
				if ipn.Contains(ip) {
					return true
				}
			}
			return false
		}

		if len(excludes) > 0 {
			for _, ipn := range excludes {
				if ipn.Contains(ip) {
					return false
				}
			}
		}
		return true
	}, nil
}