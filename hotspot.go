package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	hotspotSSID    = "AirInput"
	hotspotPass    = "airinput1"
	hotspotConName = "AirInput-Hotspot"
)

// wifiInfo holds the current state of the WiFi adapter.
type wifiInfo struct {
	iface     string
	available bool
	connected bool // true if currently connected to another network
}

// getWifiInfo detects the WiFi adapter and its connection state.
func getWifiInfo() wifiInfo {
	if runtime.GOOS == "windows" {
		return getWifiInfoWindows()
	}
	return getWifiInfoLinux()
}

func getWifiInfoLinux() wifiInfo {
	out, err := exec.Command("nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device").Output()
	if err != nil {
		return wifiInfo{}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		// parts[1] must be exactly "wifi" (not "wifi-p2p")
		if len(parts) == 3 && parts[1] == "wifi" {
			return wifiInfo{
				iface:     parts[0],
				available: true,
				connected: strings.HasPrefix(parts[2], "connected"),
			}
		}
	}
	return wifiInfo{}
}

func getWifiInfoWindows() wifiInfo {
	out, err := exec.Command("netsh", "wlan", "show", "interfaces").Output()
	if err != nil {
		return wifiInfo{}
	}
	var iface string
	connected := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name") {
			if p := strings.SplitN(line, ":", 2); len(p) == 2 {
				iface = strings.TrimSpace(p[1])
			}
		}
		if strings.Contains(line, "State") && strings.Contains(strings.ToLower(line), "connected") {
			connected = true
		}
	}
	return wifiInfo{iface: iface, available: iface != "", connected: connected}
}

// startHotspot launches the OS hotspot. Blocking — meant to run inside a tea.Cmd.
func startHotspot(iface string) error {
	if runtime.GOOS == "windows" {
		return startHotspotWindows()
	}
	return startHotspotLinux(iface)
}

func startHotspotLinux(iface string) error {
	// Remove any leftover connection from a previous session
	exec.Command("nmcli", "connection", "delete", hotspotConName).Run()

	out, err := exec.Command("nmcli", "device", "wifi", "hotspot",
		"con-name", hotspotConName,
		"ifname", iface,
		"ssid", hotspotSSID,
		"password", hotspotPass,
		"band", "bg",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// openFirewallPort adds the hotspot interface to the trusted zone permanently
// so the rule survives any firewalld reload that NM might trigger.
func openFirewallPort(iface string) {
	// Permanent rule (survives firewalld --reload)
	exec.Command("firewall-cmd", "--permanent", "--zone=trusted", "--add-interface="+iface).Run()
	// Reload to activate the permanent rule immediately
	exec.Command("firewall-cmd", "--reload").Run()
}

// closeFirewallPort removes the permanent rule and restores the interface
// to the default zone when the hotspot is stopped.
func closeFirewallPort(iface string) {
	exec.Command("firewall-cmd", "--permanent", "--zone=trusted", "--remove-interface="+iface).Run()
	exec.Command("firewall-cmd", "--reload").Run()
}


func startHotspotWindows() error {
	steps := [][]string{
		{"netsh", "wlan", "set", "hostednetwork", "mode=allow",
			"ssid=" + hotspotSSID, "key=" + hotspotPass},
		{"netsh", "wlan", "start", "hostednetwork"},
	}
	for _, args := range steps {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// waitForHotspotIP polls until the hotspot interface has an IP or times out (~20s).
func waitForHotspotIP() (string, error) {
	for i := 0; i < 20; i++ {
		ip, err := getHotspotIP()
		if err == nil && ip != "" {
			return ip, nil
		}
		time.Sleep(1 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for hotspot IP address")
}

func getHotspotIP() (string, error) {
	if runtime.GOOS == "windows" {
		return getHotspotIPWindows()
	}
	return getHotspotIPLinux()
}

func getHotspotIPLinux() (string, error) {
	out, err := exec.Command("nmcli", "-g", "IP4.ADDRESS", "connection", "show", hotspotConName).Output()
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("no IP assigned yet")
	}
	// Format returned: "10.42.0.1/24" — strip the prefix length
	return strings.Split(raw, "/")[0], nil
}

func getHotspotIPWindows() (string, error) {
	// The Windows hosted network creates a "Local Area Connection*" virtual adapter
	out, err := exec.Command("ipconfig").Output()
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(out), "\n")
	inHotspot := false
	for _, line := range lines {
		if strings.Contains(line, "Local Area Connection*") {
			inHotspot = true
		}
		if inHotspot && strings.Contains(line, "IPv4 Address") {
			if p := strings.SplitN(line, ":", 2); len(p) == 2 {
				return strings.TrimSpace(p[1]), nil
			}
		}
		if inHotspot && strings.TrimSpace(line) == "" {
			inHotspot = false
		}
	}
	return "", fmt.Errorf("hotspot IP not found in ipconfig output")
}

// stopHotspot shuts down the hotspot cleanly.
func stopHotspot() {
	if runtime.GOOS == "windows" {
		exec.Command("netsh", "wlan", "stop", "hostednetwork").Run()
		return
	}
	// Get iface before bringing the connection down
	out, _ := exec.Command("nmcli", "-g", "GENERAL.DEVICES", "connection", "show", hotspotConName).Output()
	iface := strings.TrimSpace(string(out))
	if iface != "" {
		closeFirewallPort(iface)
	}
	exec.Command("nmcli", "connection", "down", hotspotConName).Run()
	exec.Command("nmcli", "connection", "delete", hotspotConName).Run()
}
