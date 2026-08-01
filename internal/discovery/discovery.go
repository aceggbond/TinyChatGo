package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Port = 39216

	requestMagic  = "LANCHATGO_DISCOVER_V1"
	responseMagic = "LANCHATGO_SERVICE_V1"
)

// Service is the small, non-sensitive announcement exchanged on the local
// network. Host is taken from the UDP response source instead of trusting data
// supplied inside the packet.
type Service struct {
	Magic          string `json:"magic"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Host           string `json:"-"`
	HTTPPort       int    `json:"httpPort"`
	HTTPSPort      int    `json:"httpsPort,omitempty"`
	RedirectHTTPS  bool   `json:"redirectHTTPS,omitempty"`
	ClientDownload bool   `json:"clientDownload,omitempty"`
}

func (s Service) PreferredURL() string {
	scheme, port := "http", s.HTTPPort
	if s.RedirectHTTPS && s.HTTPSPort > 0 {
		scheme, port = "https", s.HTTPSPort
	}
	if strings.TrimSpace(s.Host) == "" || port < 1 || port > 65535 {
		return ""
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(s.Host, strconv.Itoa(port))}).String()
}

// StartResponder listens for the single TinyChatGo discovery datagram. Failure
// to bind this optional port should be logged by the server but must not stop
// HTTP/HTTPS service startup.
func StartResponder(ctx context.Context, provider func() Service) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: Port})
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		defer conn.Close()
		buffer := make([]byte, 256)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, remote, readErr := conn.ReadFromUDP(buffer)
			if readErr != nil {
				if ctx.Err() != nil || errors.Is(readErr, net.ErrClosed) {
					return
				}
				if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
					continue
				}
				continue
			}
			if strings.TrimSpace(string(buffer[:n])) != requestMagic || remote == nil {
				continue
			}
			service := provider()
			service.Magic = responseMagic
			payload, marshalErr := json.Marshal(service)
			if marshalErr == nil && len(payload) <= 2048 {
				_, _ = conn.WriteToUDP(payload, remote)
			}
		}
	}()
	return nil
}

// ScanCClass sends one broadcast per local IPv4 C segment and waits briefly
// for replies. It never walks individual hosts or probes unrelated ports.
func ScanCClass(ctx context.Context, wait time.Duration) ([]Service, error) {
	if wait <= 0 {
		wait = 1400 * time.Millisecond
	}
	conn, err := listenBroadcastUDP()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	deadline := time.Now().Add(wait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetDeadline(deadline)
	payload := []byte(requestMagic)
	for _, address := range cClassBroadcastAddresses() {
		_, _ = conn.WriteToUDP(payload, &net.UDPAddr{IP: address, Port: Port})
	}
	// Also cover a client and server running on the same computer without
	// turning loopback into part of the LAN broadcast list.
	_, _ = conn.WriteToUDP(payload, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: Port})

	services := make(map[string]Service)
	buffer := make([]byte, 2048)
	for {
		if ctx.Err() != nil {
			break
		}
		n, remote, readErr := conn.ReadFromUDP(buffer)
		if readErr != nil {
			if timeout, ok := readErr.(net.Error); ok && timeout.Timeout() {
				break
			}
			if errors.Is(readErr, net.ErrClosed) {
				break
			}
			return nil, readErr
		}
		var service Service
		if json.Unmarshal(buffer[:n], &service) != nil ||
			service.Magic != responseMagic ||
			remote == nil ||
			service.HTTPPort < 1 || service.HTTPPort > 65535 ||
			service.HTTPSPort < 0 || service.HTTPSPort > 65535 {
			continue
		}
		service.Host = remote.IP.String()
		service.Magic = ""
		if service.Name == "" {
			service.Name = "TinyChatGo"
		}
		key := net.JoinHostPort(service.Host, strconv.Itoa(service.HTTPPort))
		services[key] = service
	}
	result := make([]Service, 0, len(services))
	for _, service := range services {
		result = append(result, service)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].PreferredURL() < result[j].PreferredURL()
	})
	return result, nil
}

func cClassBroadcastAddresses() []net.IP {
	seen := make(map[string]struct{})
	result := make([]net.IP, 0, 4)
	add := func(ip net.IP) {
		key := ip.String()
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		result = append(result, append(net.IP(nil), ip...))
	}
	interfaces, _ := net.Interfaces()
	for _, item := range interfaces {
		if item.Flags&net.FlagUp == 0 || item.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := item.Addrs()
		for _, address := range addresses {
			ip, _, parseErr := net.ParseCIDR(address.String())
			ip = ip.To4()
			if parseErr != nil || ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			add(net.IPv4(ip[0], ip[1], ip[2], 255))
		}
	}
	if len(result) == 0 {
		add(net.IPv4bcast)
	}
	return result
}
