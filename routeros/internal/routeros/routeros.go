// Package routeros polls MikroTik RouterOS from an external pusher.
//
// RouterOS is a closed appliance OS: the native NetPulse agent cannot run on
// it. This package keeps the vendor client outside the NetPulse server and
// returns the same probe.PayloadData used by every external integration.
//
// The binary API on port 8728 is preferred because many RouterOS installs
// keep www/www-ssl disabled. REST is a fallback for readers that only expose
// the web API, and covers system identity/resource only, matching the old
// server fallback.
package routeros

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gnacho/netpulse/agent/probe"
)

// Config is the RouterOS endpoint and credentials for one pusher.
type Config struct {
	Host     string
	User     string
	Password string
	Insecure bool
	Timeout  time.Duration
}

// Poller owns the previous WAN counters so rates survive between polls.
type Poller struct {
	cfg Config

	mu   sync.Mutex
	prev *rateSample
}

// New builds a poller. A zero timeout becomes ten seconds.
func New(cfg Config) *Poller {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &Poller{cfg: cfg}
}

// Poll tries the binary API first and REST second.
func (p *Poller) Poll(ctx context.Context) (*probe.PayloadData, error) {
	apiData, apiErr := p.pollAPI(ctx)
	if apiErr == nil {
		return apiData, nil
	}
	restData, restErr := p.pollREST(ctx)
	if restErr == nil {
		return restData, nil
	}
	return nil, fmt.Errorf("routeros api: %v; rest: %w", apiErr, restErr)
}

type routerOSResource struct {
	CPULoad      string `json:"cpu-load"`
	FreeMemory   string `json:"free-memory"`
	TotalMemory  string `json:"total-memory"`
	Uptime       string `json:"uptime"`
	Version      string `json:"version"`
	BoardName    string `json:"board-name"`
	Architecture string `json:"architecture-name"`
}

type routerOSIdentity struct {
	Name string `json:"name"`
}

var routerOSDurationRe = regexp.MustCompile(`(\d+)([wdhms])`)

func parseRouterOSUptime(s string) (float64, error) {
	matches := routerOSDurationRe.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return 0, fmt.Errorf("routeros: unparseable uptime %q", s)
	}
	unitSeconds := map[string]float64{"w": 604800, "d": 86400, "h": 3600, "m": 60, "s": 1}
	var total float64
	for _, m := range matches {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("routeros: bad uptime component %q: %w", m[0], err)
		}
		total += n * unitSeconds[m[2]]
	}
	return total, nil
}

func buildPayload(res routerOSResource, id routerOSIdentity) (*probe.PayloadData, error) {
	cpuLoad, err := strconv.Atoi(res.CPULoad)
	if err != nil {
		return nil, fmt.Errorf("routeros: bad cpu-load %q: %w", res.CPULoad, err)
	}
	uptime, err := parseRouterOSUptime(res.Uptime)
	if err != nil {
		return nil, err
	}
	total, _ := strconv.ParseFloat(res.TotalMemory, 64)
	free, _ := strconv.ParseFloat(res.FreeMemory, 64)

	model := res.BoardName
	if model == "" {
		model = "RouterOS"
	}
	system := res.Architecture
	if system == "" {
		system = "routeros"
	}
	sys := &probe.SystemData{
		CPU:     &cpuLoad,
		SysInfo: &probe.SysInfo{Uptime: uptime},
		Board: &probe.BoardInfo{
			Model:     model,
			Hostname:  id.Name,
			System:    system,
			BoardName: res.BoardName,
		},
	}
	sys.SysInfo.Memory.Total = total
	sys.SysInfo.Memory.Free = free
	sys.Board.Release.Version = res.Version
	sys.Board.Release.Target = res.Architecture
	if res.BoardName != "" {
		sys.Board.Release.Description = "RouterOS " + res.Version + " (" + res.BoardName + ")"
	} else {
		sys.Board.Release.Description = "RouterOS " + res.Version
	}
	return &probe.PayloadData{System: sys}, nil
}

func (p *Poller) pollREST(ctx context.Context) (*probe.PayloadData, error) {
	client := &http.Client{Timeout: p.cfg.Timeout}
	if p.cfg.Insecure {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // opt-in for RouterOS self-signed certs
	}

	var lastErr error
	for _, scheme := range [...]string{"https", "http"} {
		base := scheme + "://" + restHost(p.cfg.Host)
		var res routerOSResource
		if err := fetchREST(ctx, client, base, p.cfg.User, p.cfg.Password, "/rest/system/resource", &res); err != nil {
			lastErr = err
			continue
		}
		var id routerOSIdentity
		if err := fetchREST(ctx, client, base, p.cfg.User, p.cfg.Password, "/rest/system/identity", &id); err != nil {
			lastErr = err
			continue
		}
		return buildPayload(res, id)
	}
	return nil, lastErr
}

func fetchREST(ctx context.Context, client *http.Client, base, user, pass, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(user, pass)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return fmt.Errorf("routeros: GET %s: %s: %s", path, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type routerOSAPIConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialRouterOSAPI(ctx context.Context, host, user, pass string, timeout time.Duration) (*routerOSAPIConn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "8728"))
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c := &routerOSAPIConn{conn: conn, r: bufio.NewReader(conn)}
	if err := c.writeSentence("/login", "=name="+user, "=password="+pass); err != nil {
		conn.Close()
		return nil, err
	}
	reply, err := c.readSentence()
	if err != nil {
		conn.Close()
		return nil, err
	}
	if len(reply) == 0 || reply[0] != "!done" {
		conn.Close()
		return nil, fmt.Errorf("routeros api: login: %s", strings.Join(reply, " "))
	}
	return c, nil
}

func (c *routerOSAPIConn) Close() { _ = c.conn.Close() }

func (c *routerOSAPIConn) query(cmd string) ([]map[string]string, error) {
	if err := c.writeSentence(cmd); err != nil {
		return nil, err
	}
	var rows []map[string]string
	for {
		sentence, err := c.readSentence()
		if err != nil {
			return nil, err
		}
		if len(sentence) == 0 {
			continue
		}
		switch sentence[0] {
		case "!done":
			return rows, nil
		case "!trap":
			return nil, fmt.Errorf("routeros api %s: %s", cmd, strings.Join(sentence[1:], " "))
		case "!re":
			row := map[string]string{}
			for _, w := range sentence[1:] {
				w = strings.TrimPrefix(w, "=")
				if k, v, ok := strings.Cut(w, "="); ok {
					row[k] = v
				}
			}
			rows = append(rows, row)
		}
	}
}

func (c *routerOSAPIConn) writeSentence(words ...string) error {
	for _, w := range words {
		if err := writeRouterOSWord(c.conn, w); err != nil {
			return err
		}
	}
	return writeRouterOSWord(c.conn, "")
}

func (c *routerOSAPIConn) readSentence() ([]string, error) {
	var words []string
	for {
		w, err := readRouterOSWord(c.r)
		if err != nil {
			return nil, err
		}
		if w == "" {
			return words, nil
		}
		words = append(words, w)
	}
}

func writeRouterOSWord(w io.Writer, word string) error {
	if err := writeRouterOSLen(w, len(word)); err != nil {
		return err
	}
	_, err := w.Write([]byte(word))
	return err
}

func writeRouterOSLen(w io.Writer, l int) error {
	switch {
	case l < 0x80:
		_, err := w.Write([]byte{byte(l)})
		return err
	case l < 0x4000:
		v := l | 0x8000
		return binary.Write(w, binary.BigEndian, uint16(v))
	case l < 0x200000:
		buf := []byte{byte(l>>16) | 0xC0, byte(l >> 8), byte(l)}
		_, err := w.Write(buf)
		return err
	case l < 0x10000000:
		buf := []byte{byte(l>>24) | 0xE0, byte(l >> 16), byte(l >> 8), byte(l)}
		_, err := w.Write(buf)
		return err
	default:
		if _, err := w.Write([]byte{0xF0}); err != nil {
			return err
		}
		return binary.Write(w, binary.BigEndian, uint32(l))
	}
}

func readRouterOSWord(r *bufio.Reader) (string, error) {
	l, err := readRouterOSLen(r)
	if err != nil {
		return "", err
	}
	if l == 0 {
		return "", nil
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func readRouterOSLen(r *bufio.Reader) (int, error) {
	c0, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	switch {
	case c0&0x80 == 0:
		return int(c0), nil
	case c0&0xC0 == 0x80:
		c1, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		return int(c0&^0xC0)<<8 | int(c1), nil
	case c0&0xE0 == 0xC0:
		b := make([]byte, 2)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		return int(c0&^0xE0)<<16 | int(b[0])<<8 | int(b[1]), nil
	case c0&0xF0 == 0xE0:
		b := make([]byte, 3)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		return int(c0&^0xF0)<<24 | int(b[0])<<16 | int(b[1])<<8 | int(b[2]), nil
	case c0 == 0xF0:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		return int(binary.BigEndian.Uint32(b)), nil
	default:
		return 0, fmt.Errorf("routeros api: invalid length prefix 0x%02x", c0)
	}
}

func (p *Poller) pollAPI(ctx context.Context) (*probe.PayloadData, error) {
	conn, err := dialRouterOSAPI(ctx, apiHost(p.cfg.Host), p.cfg.User, p.cfg.Password, p.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resRows, err := conn.query("/system/resource/print")
	if err != nil {
		return nil, err
	}
	if len(resRows) == 0 {
		return nil, fmt.Errorf("routeros api: no rows from /system/resource/print")
	}
	idRows, err := conn.query("/system/identity/print")
	if err != nil {
		return nil, err
	}
	if len(idRows) == 0 {
		return nil, fmt.Errorf("routeros api: no rows from /system/identity/print")
	}

	rr := resRows[0]
	res := routerOSResource{
		CPULoad:      rr["cpu-load"],
		FreeMemory:   rr["free-memory"],
		TotalMemory:  rr["total-memory"],
		Uptime:       rr["uptime"],
		Version:      rr["version"],
		BoardName:    rr["board-name"],
		Architecture: rr["architecture-name"],
	}
	data, err := buildPayload(res, routerOSIdentity{Name: idRows[0]["name"]})
	if err != nil {
		return nil, err
	}
	sys := data.System

	var ifRows []map[string]string
	if rows, err := conn.query("/interface/print"); err == nil {
		ifRows = rows
		sys.BridgeMAC = routerOSBridgeMAC(rows)
		data.FDB = &probe.FDBData{
			MACs:  map[string]string{},
			Ports: routerOSPorts(rows),
		}
	}
	if rows, err := conn.query("/ip/dhcp-server/lease/print"); err == nil {
		data.DHCP = &probe.DHCPData{Leases: routerOSLeases(rows)}
	}
	if rows, err := conn.query("/ip/arp/print"); err == nil {
		arp, stale := routerOSArp(rows)
		data.Arp = arp
		data.ArpStale = stale
	}
	if rows, err := conn.query("/interface/bridge/host/print"); err == nil {
		if data.FDB == nil {
			data.FDB = &probe.FDBData{MACs: map[string]string{}}
		}
		data.FDB.MACs = routerOSFDB(rows)
	}
	if rows, err := conn.query("/system/health/print"); err == nil {
		if temp := routerOSTemp(rows); temp > 0 {
			sys.Temp = &temp
		}
	}
	if cpuRows, err := conn.query("/system/resource/cpu/print"); err == nil && len(cpuRows) > 0 {
		sum := 0
		for _, row := range cpuRows {
			l, _ := strconv.Atoi(row["load"])
			sum += l
		}
		avg := (sum + len(cpuRows)/2) / len(cpuRows)
		sys.CPU = &avg
	}
	if hdd, _ := strconv.ParseFloat(rr["total-hdd-space"], 64); hdd > 0 {
		sys.Flash = fmt.Sprintf("%.0f MB", hdd/1e6)
	}
	if rows, err := conn.query("/system/package/update/print"); err == nil {
		sys.FirmwareAvailable = routerOSPackageUpdate(rows)
		if sys.FirmwareAvailable == "" {
			_ = conn.writeSentence("/system/package/update/check-for-updates")
			if _, err := conn.readSentence(); err == nil {
				if rowsAfter, err := conn.query("/system/package/update/print"); err == nil {
					sys.FirmwareAvailable = routerOSPackageUpdate(rowsAfter)
				}
			}
		}
	}
	if routeRows, err := conn.query("/ip/route/print"); err == nil {
		if wanIface := routerOSWANInterface(routeRows); wanIface != "" {
			sys.RxBps, sys.TxBps = p.netRate(ifRows, wanIface)
		}
	}
	return data, nil
}

type rateSample struct {
	at time.Time
	rx uint64
	tx uint64
}

func (p *Poller) netRate(ifRows []map[string]string, wanIface string) (*float64, *float64) {
	var rxBytes, txBytes uint64
	found := false
	for _, row := range ifRows {
		if row["name"] != wanIface {
			continue
		}
		rxBytes, _ = strconv.ParseUint(row["rx-byte"], 10, 64)
		txBytes, _ = strconv.ParseUint(row["tx-byte"], 10, 64)
		found = true
		break
	}
	if !found {
		return nil, nil
	}
	now := time.Now()
	p.mu.Lock()
	prev := p.prev
	p.prev = &rateSample{at: now, rx: rxBytes, tx: txBytes}
	p.mu.Unlock()
	if prev == nil {
		return nil, nil
	}
	dt := now.Sub(prev.at).Seconds()
	if dt <= 0 || rxBytes < prev.rx || txBytes < prev.tx {
		return nil, nil
	}
	rx := float64(rxBytes-prev.rx) * 8 / dt
	tx := float64(txBytes-prev.tx) * 8 / dt
	return &rx, &tx
}

func routerOSWANInterface(routeRows []map[string]string) string {
	for _, row := range routeRows {
		if row["dst-address"] != "0.0.0.0/0" {
			continue
		}
		if iface := row["interface"]; iface != "" {
			return iface
		}
		if gw := row["gateway"]; gw != "" && net.ParseIP(gw) == nil {
			return gw
		}
	}
	return ""
}

func routerOSPorts(rows []map[string]string) []probe.EthPort {
	ports := make([]probe.EthPort, 0, len(rows))
	for _, row := range rows {
		if row["type"] != "ether" {
			continue
		}
		name := row["name"]
		ports = append(ports, probe.EthPort{
			ID:    name,
			Label: name,
			Up:    row["running"] == "true",
			Iface: name,
		})
	}
	return ports
}

func routerOSLeases(rows []map[string]string) []probe.DhcpLease {
	leases := make([]probe.DhcpLease, 0, len(rows))
	for _, row := range rows {
		mac := strings.ToUpper(row["mac-address"])
		if mac == "" {
			continue
		}
		var expAt *int64
		if exp := row["expires-after"]; exp != "" {
			if sec, err := parseRouterOSUptime(exp); err == nil && sec > 0 {
				at := time.Now().Unix() + int64(sec)
				expAt = &at
			}
		}
		leases = append(leases, probe.DhcpLease{
			MAC:            mac,
			IP:             row["address"],
			Hostname:       row["host-name"],
			LeaseExpiresAt: expAt,
		})
	}
	return leases
}

func routerOSBridgeMAC(rows []map[string]string) string {
	for _, row := range rows {
		if row["type"] == "bridge" {
			mac := strings.ToUpper(strings.TrimSpace(row["mac-address"]))
			if mac != "" && mac != "00:00:00:00:00:00" {
				return mac
			}
		}
	}
	for _, row := range rows {
		if row["type"] == "ether" {
			mac := strings.ToUpper(strings.TrimSpace(row["mac-address"]))
			if mac != "" && mac != "00:00:00:00:00:00" {
				return mac
			}
		}
	}
	for _, row := range rows {
		mac := strings.ToUpper(strings.TrimSpace(row["mac-address"]))
		if mac != "" && mac != "00:00:00:00:00:00" {
			return mac
		}
	}
	return ""
}

func routerOSArp(rows []map[string]string) (map[string]string, []string) {
	arp := make(map[string]string, len(rows))
	stale := make([]string, 0)
	for _, row := range rows {
		if row["disabled"] == "true" || row["invalid"] == "true" {
			continue
		}
		mac := strings.ToUpper(strings.TrimSpace(row["mac-address"]))
		ip := strings.TrimSpace(row["address"])
		if mac == "" || ip == "" {
			continue
		}
		arp[mac] = ip
		if row["status"] == "failed" || row["status"] == "stale" {
			stale = append(stale, mac)
		}
	}
	return arp, stale
}

func routerOSFDB(rows []map[string]string) map[string]string {
	fdb := make(map[string]string, len(rows))
	for _, row := range rows {
		if row["local"] == "true" {
			continue
		}
		mac := strings.ToUpper(strings.TrimSpace(row["mac-address"]))
		port := strings.TrimSpace(row["on-interface"])
		if mac != "" && port != "" {
			fdb[mac] = port
		}
	}
	return fdb
}

func routerOSTemp(rows []map[string]string) int {
	for _, row := range rows {
		name := row["name"]
		if name == "cpu-temperature" || name == "temperature" || name == "board-temperature" {
			if v, err := strconv.Atoi(row["value"]); err == nil && v > 0 {
				return v
			}
		}
	}
	return 0
}

func routerOSPackageUpdate(rows []map[string]string) string {
	for _, row := range rows {
		latest := strings.TrimSpace(row["latest-version"])
		installed := strings.TrimSpace(row["installed-version"])
		if latest != "" && latest != installed {
			return latest
		}
	}
	return ""
}

func restHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

func apiHost(raw string) string {
	host := restHost(raw)
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.Trim(host, "[]")
}
