package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20

// runCurl is the gateway process seam. Keep the bearer token on stdin,
// never in process arguments, log output, or a curl configuration file.
var runCurl = curlGet

func curlGet(ctx context.Context, endpoint, apiKey string) ([]byte, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("invalid router URL; use an HTTP or HTTPS endpoint without credentials")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return nil, fmt.Errorf("router key contains a newline")
	}
	file, err := os.CreateTemp("", "orchard-router-response-*")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if err := file.Close(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout+2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "curl",
		"--disable", "--silent", "--show-error",
		"--connect-timeout", "5", "--max-time", "10",
		"--max-filesize", strconv.Itoa(maxResponseBytes),
		"--proto", "=http,https",
		"--header", "@-", "--output", path,
		"--write-out", "%{http_code}", "--url", endpoint,
	)
	cmd.Stdin = strings.NewReader("Authorization: Bearer " + apiKey + "\n")
	cmd.WaitDelay = time.Second
	var status, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &status, &diagnostics
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("curl: %w", ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Never surface raw curl stderr: it may echo request details.
			return nil, fmt.Errorf("curl exit %d: %s", exitErr.ExitCode(), curlErrorSummary(exitErr.ExitCode()))
		}
		return nil, fmt.Errorf("run curl: %w", err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(status.String()))
	if err != nil {
		return nil, fmt.Errorf("curl did not report an HTTP status")
	}
	if code != 200 {
		return nil, fmt.Errorf("status %d", code)
	}
	response, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Close() }()
	body, err := io.ReadAll(io.LimitReader(response, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response too large")
	}
	return body, nil
}

func curlErrorSummary(code int) string {
	switch code {
	case 5, 6:
		return "could not resolve router or proxy"
	case 7:
		return "could not connect to router"
	case 28:
		return "router request timed out"
	case 35, 60:
		return "router TLS connection failed"
	case 63:
		return "response too large"
	default:
		return "router request failed"
	}
}
