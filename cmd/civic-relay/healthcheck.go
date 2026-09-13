package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"civic-ai-relay/internal/config"
)

// runHealthcheck probes this process's own /healthz endpoint and returns a
// process exit status. Container images built from this repository ship no
// shell and no curl, so the HEALTHCHECK directive reuses the relay binary
// itself instead of adding a second probe artifact.
//
// It exits 0 only on HTTP 200; any connection error, timeout or non-200 status
// is a failure so the container runtime can act on it.
func runHealthcheck() int {
	host, port := healthcheckTarget()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: unexpected status %d\n", response.StatusCode)
		return 1
	}
	return 0
}

// healthcheckTarget resolves the address the server listens on, using the same
// source of truth as the server (relay.env) so that a port change cannot leave
// the container permanently unhealthy. PORT is only a fallback for the case
// where the configuration file cannot be read.
//
// The probe is strictly read-only: it must never create or rewrite the
// configuration, even when the file is missing.
func healthcheckTarget() (host, port string) {
	const loopback = "127.0.0.1"
	if path, err := config.DefaultPath(os.Getenv("CIVIC_RELAY_CONFIG_FILE")); err == nil {
		if mapping, readErr := config.NewStore(path).ReadMapping(); readErr == nil {
			if settings, parseErr := config.Parse(mapping); parseErr == nil {
				host = loopback
				// 通配地址无法作为连接目标，探针回落到回环地址。
				if settings.Host != "" && settings.Host != "0.0.0.0" && settings.Host != "::" {
					host = settings.Host
				}
				return host, strconv.Itoa(settings.Port)
			}
		}
	}
	port = strings.TrimSpace(os.Getenv("PORT"))
	if _, err := strconv.Atoi(port); err != nil {
		port = "8000"
	}
	return loopback, port
}
