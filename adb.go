package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type AdbDevice struct {
	Serial string
	State  string
	Model  string
}

func (a *App) adb(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.adbPath, args...)
	cmd.Dir = a.toolDir
	cmd.Env = append(os.Environ(), "ADB_MDNS_OPENSCREEN=1")
	hideWindow(cmd)
	// adb may spawn its server daemon, which can inherit our pipe; don't wait on it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func (a *App) adbCtx(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, a.adbPath, args...)
	cmd.Dir = a.toolDir
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (a *App) startServer() {
	cmd := exec.Command(a.adbPath, "start-server")
	cmd.Dir = a.toolDir
	cmd.Env = append(os.Environ(), "ADB_MDNS_OPENSCREEN=1")
	hideWindow(cmd)
	if cmd.Start() == nil {
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	}
}

func parseAdbDevices(out string) []AdbDevice {
	var res []AdbDevice
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "adb ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		d := AdbDevice{Serial: f[0], State: f[1]}
		for _, kv := range f[2:] {
			if strings.HasPrefix(kv, "model:") {
				d.Model = strings.ReplaceAll(strings.TrimPrefix(kv, "model:"), "_", " ")
			}
		}
		res = append(res, d)
	}
	return res
}

func (a *App) adbDevices() ([]AdbDevice, error) {
	out, err := a.adb(8*time.Second, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return parseAdbDevices(out), nil
}

// `adb mdns services` lines: "<instance>\t_adb-tls-connect._tcp\t192.168.0.10:37123"
func parseAdbMdns(out string) []MdnsService {
	var res []MdnsService
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		svc := strings.TrimSuffix(f[1], ".")
		var full string
		switch svc {
		case "_adb-tls-connect._tcp":
			full = svcConnect
		case "_adb-tls-pairing._tcp":
			full = svcPairing
		default:
			continue
		}
		host, port := splitHostPort(f[len(f)-1])
		if host == "" || port == 0 {
			continue
		}
		res = append(res, MdnsService{Instance: f[0], Service: full, IP: host, Port: port})
	}
	return res
}

func isWifiSerial(s string) bool {
	return strings.Contains(s, ":") || strings.Contains(s, "._adb-tls-connect.")
}

var reInet = regexp.MustCompile(`inet (\d+\.\d+\.\d+\.\d+)/`)
var reGuid = regexp.MustCompile(`guid=adb-([^\]\s]+)`)

// serialFromInstance extracts the device serial from "adb-<serial>-<random>".
func serialFromInstance(inst string) string {
	if !strings.HasPrefix(inst, "adb-") {
		return ""
	}
	s := strings.TrimPrefix(inst, "adb-")
	if i := strings.LastIndex(s, "-"); i > 0 {
		s = s[:i]
	}
	return s
}
