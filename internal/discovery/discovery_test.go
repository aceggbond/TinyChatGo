package discovery

import (
	"net"
	"testing"
)

func TestServicePreferredURL(t *testing.T) {
	service := Service{Host: "192.168.10.8", HTTPPort: 80, HTTPSPort: 443}
	if got := service.PreferredURL(); got != "http://192.168.10.8:80" {
		t.Fatalf("HTTP preferred URL = %q", got)
	}
	service.RedirectHTTPS = true
	if got := service.PreferredURL(); got != "https://192.168.10.8:443" {
		t.Fatalf("HTTPS preferred URL = %q", got)
	}
}

func TestCClassBroadcastAddressesDoNotEnumerateHosts(t *testing.T) {
	addresses := cClassBroadcastAddresses()
	if len(addresses) == 0 {
		t.Fatal("no discovery broadcast address")
	}
	for _, address := range addresses {
		ip := net.IP(address).To4()
		if ip == nil || ip[3] != 255 {
			t.Fatalf("discovery address is not a C-segment broadcast: %v", address)
		}
	}
}
