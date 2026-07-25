package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// --- Styles ---
var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00BFFF"))
	qrStyle    = lipgloss.NewStyle().MarginLeft(4)
	helpStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#A9A9A9"))

	playerListTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAFAFA")).Background(lipgloss.Color("#5C5C5C")).Padding(0, 1)
	playerStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("#E0E0E0"))
	cursorStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF00")) // Bright green for cursor
	selectedStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFF00")) // Yellow for selected

	hotspotActiveStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#00FF88"))
	hotspotInactiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#A9A9A9"))
	warningStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF8C00"))
)

// --- Model ---
type Player struct {
	Username     string
	ControllerID int
}

type model struct {
	qrCode      string
	url         string
	originalURL string // URL before hotspot was activated
	players     []Player
	cursor      int
	selected    int // -1 means nothing is selected
	nodeCmd     *exec.Cmd
	pythonCmd   *exec.Cmd
	udpConn     *net.UDPConn
	err         error
	nodeScanner *bufio.Scanner
	nodeStdin   *bufio.Writer
	// Hotspot state
	wifi            wifiInfo
	hotspotActive   bool
	hotspotStarting bool
	hotspotWarning  bool // waiting for user confirmation
	hotspotErr      string
}

type qrCodeMsg struct{ qr, url string }
type playerConnectMsg Player
type playerDisconnectMsg string
type errorMsg struct{ err error }
type wifiStatusMsg wifiInfo
type hotspotStartedMsg struct{ ip string }
type hotspotStoppedMsg struct{}
type hotspotErrMsg struct{ err error }

func initialModel() model {
	return model{
		players:  []Player{},
		selected: -1,
	}
}

// --- Commands & Logic ---
func (m *model) Init() tea.Cmd {
	return tea.Batch(m.startSubprocesses(), m.waitForNodeActivity(), m.detectWifi())
}

func (m *model) detectWifi() tea.Cmd {
	return func() tea.Msg { return wifiStatusMsg(getWifiInfo()) }
}

func (m *model) doStartHotspot() tea.Cmd {
	return func() tea.Msg {
		if err := startHotspot(m.wifi.iface); err != nil {
			return hotspotErrMsg{err}
		}
		ip, err := waitForHotspotIP()
		if err != nil {
			return hotspotErrMsg{err}
		}
		// Open firewall AFTER IP is assigned — ensures the interface
		// is fully registered with firewalld and zone detection works.
		openFirewallPort(m.wifi.iface)
		return hotspotStartedMsg{ip: ip}
	}
}

func (m *model) doStopHotspot() tea.Cmd {
	return func() tea.Msg {
		stopHotspot()
		return hotspotStoppedMsg{}
	}
}

// sendNodeCmd sends a JSON command to server.js via its stdin.
func (m *model) sendNodeCmd(v interface{}) {
	if m.nodeStdin == nil {
		return
	}
	b, _ := json.Marshal(v)
	m.nodeStdin.Write(b)
	m.nodeStdin.WriteByte('\n')
	m.nodeStdin.Flush()
}

func (m *model) startSubprocesses() tea.Cmd {
	// Start Python
	osType := runtime.GOOS
	var pythonCmdName, pythonScript string
	if osType == "windows" {
		pythonCmdName, pythonScript = "python", "controller-win.py"
	} else {
		pythonCmdName, pythonScript = "python3", "controller-linux.py"
	}
	m.pythonCmd = exec.Command(pythonCmdName, pythonScript)
	m.pythonCmd.Stderr = os.Stderr
	m.pythonCmd.Stdin = nil // Disconnect stdin
	if err := m.pythonCmd.Start(); err != nil {
		return func() tea.Msg { return errorMsg{err} }
	}

	// Start Node.js
	m.nodeCmd = exec.Command("node", "server.js")
	nodePipe, _ := m.nodeCmd.StdoutPipe()
	nodeInPipe, err := m.nodeCmd.StdinPipe()
	if err != nil {
		return func() tea.Msg { return errorMsg{err} }
	}
	m.nodeStdin = bufio.NewWriter(nodeInPipe)
	m.nodeCmd.Stderr = os.Stderr
	m.nodeScanner = bufio.NewScanner(nodePipe)
	if err := m.nodeCmd.Start(); err != nil {
		return func() tea.Msg { return errorMsg{err} }
	}

	// Set up UDP connection to send commands to Python
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:9999")
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return func() tea.Msg { return errorMsg{err} }
	}
	m.udpConn = conn

	return nil
}

// waitForNodeActivity listens for JSON messages from server.js
func (m *model) waitForNodeActivity() tea.Cmd {
	return func() tea.Msg {
		if m.nodeScanner == nil {
			return errorMsg{fmt.Errorf("node process scanner not available")}
		}
		if !m.nodeScanner.Scan() {
			if err := m.nodeScanner.Err(); err != nil {
				return errorMsg{fmt.Errorf("error reading from node process: %w", err)}
			}
			return errorMsg{fmt.Errorf("node process exited unexpectedly")}
		}
		line := m.nodeScanner.Text()
		var msgData map[string]interface{}
		if err := json.Unmarshal([]byte(line), &msgData); err != nil {
			// Ignore lines that are not valid JSON and keep listening
			return m.waitForNodeActivity()()
		}

		switch msgData["event"] {
		case "server_ready":
			return qrCodeMsg{
				qr:  msgData["qr"].(string),
				url: msgData["url"].(string),
			}
		case "player_connect":
			return playerConnectMsg{Username: msgData["username"].(string), ControllerID: int(msgData["controllerId"].(float64))}
		case "player_disconnect":
			return playerDisconnectMsg(msgData["username"].(string))
		}
		return m.waitForNodeActivity()()
	}
}

func (m *model) sendSwapCommand(userA, userB string) {
	if m.udpConn != nil {
		cmd := fmt.Sprintf("system:swap:%s:%s", userA, userB)
		m.udpConn.Write([]byte(cmd))
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cleanup()
			return m, tea.Quit

		case "h":
			m.hotspotErr = ""
			if m.hotspotActive {
				// Stop the hotspot
				return m, m.doStopHotspot()
			}
			if !m.wifi.available || m.hotspotStarting {
				break
			}
			if m.wifi.connected {
				// Warn user before cutting their WiFi
				m.hotspotWarning = true
			} else {
				m.hotspotStarting = true
				return m, m.doStartHotspot()
			}

		case "y":
			if m.hotspotWarning {
				m.hotspotWarning = false
				m.hotspotStarting = true
				return m, m.doStartHotspot()
			}

		case "n", "esc":
			if m.hotspotWarning {
				m.hotspotWarning = false
				break
			}
			m.selected = -1

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.players)-1 {
				m.cursor++
			}
		case "enter", " ":
			if m.cursor >= len(m.players) {
				break
			}
			if m.selected == -1 {
				m.selected = m.cursor
			} else {
				if m.selected == m.cursor {
					m.selected = -1
				} else {
					playerA := m.players[m.selected]
					playerB := m.players[m.cursor]
					m.sendSwapCommand(playerA.Username, playerB.Username)
					m.players[m.selected].ControllerID, m.players[m.cursor].ControllerID = m.players[m.cursor].ControllerID, m.players[m.selected].ControllerID
					m.selected = -1
				}
			}
		}
	case qrCodeMsg:
		m.qrCode = msg.qr
		m.url = msg.url
		if m.originalURL == "" {
			m.originalURL = msg.url
		}
		return m, m.waitForNodeActivity()

	case wifiStatusMsg:
		m.wifi = wifiInfo(msg)

	case hotspotStartedMsg:
		m.hotspotStarting = false
		m.hotspotActive = true
		newURL := fmt.Sprintf("http://%s:3000", msg.ip)
		m.sendNodeCmd(map[string]string{"cmd": "generate_qr", "url": newURL})

	case hotspotStoppedMsg:
		m.hotspotActive = false
		m.hotspotStarting = false
		// Restore original QR/URL
		if m.originalURL != "" {
			m.sendNodeCmd(map[string]string{"cmd": "generate_qr", "url": m.originalURL})
		}

	case hotspotErrMsg:
		m.hotspotStarting = false
		m.hotspotActive = false
		m.hotspotErr = msg.err.Error()
	case playerConnectMsg:
		m.players = append(m.players, Player(msg))
		return m, m.waitForNodeActivity()
	case playerDisconnectMsg:
		var updatedPlayers []Player
		usernameToDisconnect := string(msg)
		for _, p := range m.players {
			if p.Username != usernameToDisconnect {
				updatedPlayers = append(updatedPlayers, p)
			}
		}
		m.players = updatedPlayers
		if m.cursor >= len(m.players) && len(m.players) > 0 {
			m.cursor = len(m.players) - 1
		} else if len(m.players) == 0 {
			m.cursor = 0
		}
		m.selected = -1
		return m, m.waitForNodeActivity()
	case errorMsg:
		m.err = msg.err
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\nError: %v\n\n", m.err)
	}

	var b strings.Builder

	b.WriteString(titleStyle.Render("🚀 AirInput Server is Running") + "\n")
	if m.url != "" {
		b.WriteString(fmt.Sprintf("URL: %s\n", lipgloss.NewStyle().Bold(true).Render(m.url)))
	} else {
		b.WriteString("Waiting for server to start...\n")
	}

	if m.qrCode != "" {
		b.WriteString(qrStyle.Render(m.qrCode) + "\n\n")
	} else if m.url != "" {
		b.WriteString("Generating QR code...\n\n")
	}

	// --- Hotspot section ---
	if m.hotspotWarning {
		b.WriteString(warningStyle.Render("⚠️  Activar el hotspot desconectará tu WiFi actual.") + "\n")
		b.WriteString(helpStyle.Render("   Presiona 'y' para continuar o 'n' para cancelar.") + "\n\n")
	} else if m.hotspotStarting {
		b.WriteString(hotspotInactiveStyle.Render("📡 Iniciando hotspot...") + "\n\n")
	} else if m.hotspotActive {
		b.WriteString(hotspotActiveStyle.Render(
			fmt.Sprintf("📡 [ACTIVO]  Red: %s   Contraseña: %s", hotspotSSID, hotspotPass),
		) + "\n\n")
	} else if m.hotspotErr != "" {
		b.WriteString(warningStyle.Render("❌ Error hotspot: "+m.hotspotErr) + "\n\n")
	} else if m.wifi.available {
		b.WriteString(hotspotInactiveStyle.Render("📡 Hotspot inactivo") + "\n\n")
	}

	b.WriteString(playerListTitleStyle.Render("🎮 Connected Players") + "\n")
	if len(m.players) == 0 {
		b.WriteString("No players connected yet.\n")
	} else {
		for i, p := range m.players {
			cursor := " "
			if m.cursor == i {
				cursor = cursorStyle.Render(">")
			}
			line := fmt.Sprintf(" %s (Controller %d)", p.Username, p.ControllerID)
			if m.cursor == i {
				line = cursorStyle.Render(line)
			}
			if m.selected == i {
				line = selectedStyle.Render(line)
			}
			b.WriteString(cursor + line + "\n")
		}
	}

	var help string
	switch {
	case m.hotspotWarning:
		help = "'y' confirmar  'n' cancelar"
	case m.selected != -1:
		help = fmt.Sprintf("Swapping '%s'. Select another to swap or Esc to cancel.", m.players[m.selected].Username)
	case m.hotspotActive:
		help = "↑/↓ navegar  Enter seleccionar  'h' apagar hotspot  'q' salir"
	default:
		help = "↑/↓ navegar  Enter seleccionar  'h' hotspot  'q' salir"
	}
	b.WriteString("\n" + helpStyle.Render(help) + "\n")

	return b.String()
}

func (m *model) cleanup() {
	if m.hotspotActive {
		stopHotspot()
	}
	if m.udpConn != nil {
		m.udpConn.Close()
	}
	if m.nodeCmd != nil && m.nodeCmd.Process != nil {
		m.nodeCmd.Process.Signal(syscall.SIGTERM)
	}
	if m.pythonCmd != nil && m.pythonCmd.Process != nil {
		m.pythonCmd.Process.Signal(syscall.SIGTERM)
	}
}

func main() {
	m := initialModel()
	p := tea.NewProgram(&m)

	// Use an `if` block instead of `log.Fatalf` for a cleaner shutdown
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running program: %v\n", err)
		m.cleanup() // Try to clean up even on error
		os.Exit(1)
	}

	// Cleanup for a normal exit was already done in Update, so here we just print.
	fmt.Println("Bye!")
}
