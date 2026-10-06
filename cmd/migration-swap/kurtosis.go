package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/CPerezz/pbt-devnet/internal/cli"
	"github.com/CPerezz/pbt-devnet/internal/migmon"
	"github.com/CPerezz/pbt-devnet/internal/migsched"
)

// node is one participant's EL/CL service names.
type node struct {
	EL, CL string
}

// sh runs a command, returning trimmed stdout; stderr is folded into the error.
func sh(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, cli.Trim(stderr.Bytes(), 400))
	}
	return out, nil
}

// shCombined runs a command and returns everything it printed, stdout and stderr interleaved
// as a log reader would see it; used for container output that is evidence, not a value to parse.
func shCombined(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *runner) serviceStop(svc string) error {
	_, err := sh("kurtosis", "service", "stop", r.enc, svc)
	return err
}

func (r *runner) serviceStart(svc string) error {
	_, err := sh("kurtosis", "service", "start", r.enc, svc)
	return err
}

func (r *runner) containerID(svc string) (string, error) {
	out, err := sh("docker", "ps", "-a",
		"--filter", "label=kurtosis_service_name="+svc,
		"--filter", "label=kurtosis_enclave_name="+r.enc,
		"-q")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("no container for service %s", svc)
	}
	return fields[0], nil
}

func (r *runner) containerRunning(cid string) (bool, error) {
	out, err := sh("docker", "inspect", "-f", "{{.State.Running}}", cid)
	if err != nil {
		return false, err
	}
	return out == "true", nil
}

func (r *runner) dockerExec(cid string, cmd ...string) (string, error) {
	args := append([]string{"exec", cid}, cmd...)
	return sh("docker", args...)
}

func (r *runner) dockerCP(src, dst string) error {
	_, err := sh("docker", "cp", src, dst)
	return err
}

func (r *runner) dockerRM(cid string) {
	if _, err := sh("docker", "rm", cid); err != nil {
		say("docker rm %s: %v", cid, err)
	}
}

// streamDatadir pipes `docker cp` from one container straight into another without
// touching the host disk; pipefail surfaces either side's failure.
func streamDatadir(srcContainer, srcPath, dstContainer, dstPath string) error {
	_, err := sh("bash", "-o", "pipefail", "-c",
		fmt.Sprintf("docker cp %s:%s - | docker cp - %s:%s", srcContainer, srcPath, dstContainer, dstPath))
	return err
}

// portURL resolves a service's published host URL, normalising the scheme as
// scripts/pbt.py's url() does. Erigon publishes its JSON-RPC port as "ws-rpc", not "rpc".
func (r *runner) portURL(svc, port string) (string, error) {
	out, err := sh("kurtosis", "port", "print", r.enc, svc, port)
	if err != nil || out == "" {
		if port == "rpc" {
			return r.portURL(svc, "ws-rpc")
		}
		if err == nil {
			err = fmt.Errorf("empty port")
		}
		return "", fmt.Errorf("port %s on %s: %w", port, svc, err)
	}
	if !strings.HasPrefix(out, "http") {
		out = "http://" + out
	}
	return out, nil
}

var svcRowRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// resolveNodes maps participant index to EL/CL service names, read from
// `kurtosis enclave inspect`: service rows begin with a 12-hex UUID. It also
// returns every service name, so optional ones (chaos, disruptoor) can be absent.
func resolveNodes(enc string) (map[int]node, map[string]bool, error) {
	out, err := sh("kurtosis", "enclave", "inspect", enc)
	if err != nil {
		return nil, nil, fmt.Errorf("inspecting enclave %s: %w", enc, err)
	}
	elRe := regexp.MustCompile(`^el-(\d+)-`)
	clRe := regexp.MustCompile(`^cl-(\d+)-`)
	nodes := make(map[int]node)
	services := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || !svcRowRe.MatchString(parts[0]) {
			continue
		}
		name := parts[1]
		services[name] = true
		if m := elRe.FindStringSubmatch(name); m != nil {
			idx, _ := strconv.Atoi(m[1])
			n := nodes[idx]
			n.EL = name
			nodes[idx] = n
		} else if m := clRe.FindStringSubmatch(name); m != nil {
			idx, _ := strconv.Atoi(m[1])
			n := nodes[idx]
			n.CL = name
			nodes[idx] = n
		}
	}
	for i := 1; i <= len(nodes); i++ {
		if nodes[i].EL == "" {
			return nil, nil, fmt.Errorf("enclave %s: no el service found for node %d", enc, i)
		}
	}
	return nodes, services, nil
}

// loadSchedule reads the chaos driver's resolved schedule from its logs. Lines may carry a
// kurtosis log prefix ("[svc] "); strip to the first '{' as lap.sh's sed does. With no
// chaos driver in the enclave (chaos_profile: none) there is no schedule to keep clear of.
func loadSchedule(enc string, services map[string]bool) (migsched.Dump, error) {
	if !services["migration-chaos"] {
		say("no migration-chaos service: no chaos schedule to keep clear of")
		return migsched.Dump{}, nil
	}
	out, err := sh("kurtosis", "service", "logs", enc, "migration-chaos", "-a")
	if err != nil {
		return migsched.Dump{}, fmt.Errorf("reading migration-chaos logs: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		i := strings.IndexByte(line, '{')
		if i < 0 {
			continue
		}
		var ev migmon.Event
		if err := json.Unmarshal([]byte(line[i:]), &ev); err != nil {
			continue
		}
		if ev.Kind == migmon.EvSchedule {
			return migsched.ParseDump(ev.Raw)
		}
	}
	return migsched.Dump{}, fmt.Errorf("migration-chaos logs: no schedule event found")
}
