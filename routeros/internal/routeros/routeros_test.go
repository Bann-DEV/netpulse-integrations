package routeros

import (
	"testing"

	"github.com/gnacho/netpulse/agent/probe"
)

func TestParseUptime(t *testing.T) {
	cases := map[string]float64{
		"5s":         5,
		"4m5s":       245,
		"3h4m5s":     11045,
		"2d3h4m5s":   183845,
		"1w2d3h4m5s": 788645,
	}
	for input, want := range cases {
		got, err := parseRouterOSUptime(input)
		if err != nil {
			t.Fatalf("parseRouterOSUptime(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("parseRouterOSUptime(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestParseUptimeRejectsGarbage(t *testing.T) {
	if _, err := parseRouterOSUptime("not-a-duration"); err == nil {
		t.Fatal("expected an error for unparseable uptime")
	}
}

func TestBuildPayloadMapsSystem(t *testing.T) {
	res := routerOSResource{
		CPULoad:      "3",
		FreeMemory:   "104857600",
		TotalMemory:  "134217728",
		Uptime:       "1d02h03m04s",
		Version:      "7.15",
		BoardName:    "RB4011",
		Architecture: "arm",
	}
	data, err := buildPayload(res, routerOSIdentity{Name: "MikroTik-Core"})
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	sys := data.System
	if sys.CPU == nil || *sys.CPU != 3 {
		t.Errorf("CPU = %v, want 3", sys.CPU)
	}
	if sys.SysInfo.Memory.Total != 134217728 || sys.SysInfo.Memory.Free != 104857600 {
		t.Errorf("memory = %+v", sys.SysInfo.Memory)
	}
	if sys.Board.Hostname != "MikroTik-Core" || sys.Board.Release.Version != "7.15" {
		t.Errorf("board = %+v", sys.Board)
	}
}

func TestBuildPayloadRejectsBadCPU(t *testing.T) {
	res := routerOSResource{CPULoad: "n/a", Uptime: "1s"}
	if _, err := buildPayload(res, routerOSIdentity{}); err == nil {
		t.Fatal("expected an error for bad cpu-load")
	}
}

func TestRouterOSPortsOnlyPhysicalEthernet(t *testing.T) {
	ports := routerOSPorts([]map[string]string{
		{"name": "ether1", "type": "ether", "running": "true"},
		{"name": "bridge", "type": "bridge"},
		{"name": "ether2", "type": "ether", "running": "false"},
	})
	if len(ports) != 2 {
		t.Fatalf("ports = %+v, want 2", ports)
	}
	if ports[0].ID != "ether1" || !ports[0].Up || ports[1].ID != "ether2" || ports[1].Up {
		t.Fatalf("ports = %+v", ports)
	}
}

func TestRouterOSLeasesAndArpUseUpperMAC(t *testing.T) {
	leases := routerOSLeases([]map[string]string{
		{"mac-address": "aa:bb:cc:dd:ee:ff", "address": "192.168.1.20", "host-name": "laptop"},
	})
	if len(leases) != 1 || leases[0].MAC != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("leases = %+v", leases)
	}
	arp, stale := routerOSArp([]map[string]string{
		{"mac-address": "aa:bb:cc:dd:ee:ff", "address": "192.168.1.20", "status": "stale"},
		{"mac-address": "", "address": "192.168.1.21"},
	})
	if arp["AA:BB:CC:DD:EE:FF"] != "192.168.1.20" {
		t.Fatalf("arp = %+v", arp)
	}
	if len(stale) != 1 || stale[0] != "AA:BB:CC:DD:EE:FF" {
		t.Fatalf("stale = %+v", stale)
	}
}

func TestRouterOSFDBExcludesLocalEntries(t *testing.T) {
	fdb := routerOSFDB([]map[string]string{
		{"mac-address": "aa:bb:cc:dd:ee:ff", "on-interface": "bridge", "local": "false"},
		{"mac-address": "11:22:33:44:55:66", "on-interface": "bridge", "local": "true"},
	})
	if len(fdb) != 1 || fdb["AA:BB:CC:DD:EE:FF"] != "bridge" {
		t.Fatalf("fdb = %+v", fdb)
	}
}

func TestRouterOSPackageUpdate(t *testing.T) {
	if got := routerOSPackageUpdate([]map[string]string{{"latest-version": "7.16", "installed-version": "7.15"}}); got != "7.16" {
		t.Fatalf("update = %q, want 7.16", got)
	}
	if got := routerOSPackageUpdate([]map[string]string{{"latest-version": "7.15", "installed-version": "7.15"}}); got != "" {
		t.Fatalf("update = %q, want empty", got)
	}
}

func TestAPIHostStripsSchemePathAndPort(t *testing.T) {
	cases := map[string]string{
		"192.168.88.1":          "192.168.88.1",
		"https://192.168.88.1":  "192.168.88.1",
		"http://router.local/":  "router.local",
		"https://router:8443/x": "router",
		"router.lan":            "router.lan",
	}
	for input, want := range cases {
		if got := apiHost(input); got != want {
			t.Errorf("apiHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRestHostKeepsPort(t *testing.T) {
	cases := map[string]string{
		"https://router:8443":      "router:8443",
		"http://192.168.88.1:8080": "192.168.88.1:8080",
		"router.lan:8443":          "router.lan:8443",
		"[fd00::1]:8443":           "[fd00::1]:8443",
	}
	for input, want := range cases {
		if got := restHost(input); got != want {
			t.Errorf("restHost(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPayloadKeepsFlashAndFirmware(t *testing.T) {
	data := &probe.PayloadData{System: &probe.SystemData{Flash: "512 MB", FirmwareAvailable: "7.16"}}
	if data.System.Flash != "512 MB" || data.System.FirmwareAvailable != "7.16" {
		t.Fatalf("system = %+v", data.System)
	}
}
