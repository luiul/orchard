package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeCurl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestCurlArgumentsKeepKeyOffCommandLine(t *testing.T) {
	dir := fakeCurl(t, `printf '%s\n' "$@" > "$CURL_TEST_ARGS"
while [ "$#" -gt 0 ]; do
    if [ "$1" = "--output" ]; then shift; output=$1; fi
    shift
done
IFS= read -r header
printf '%s' "$header" > "$CURL_TEST_HEADER"
printf '%s' '{"data":[]}' > "$output"
printf '%s' '200'
`)
	argsPath, headerPath := filepath.Join(dir, "args"), filepath.Join(dir, "header")
	t.Setenv("CURL_TEST_ARGS", argsPath)
	t.Setenv("CURL_TEST_HEADER", headerPath)
	const key = "private-test-key"
	body, err := curlGet(context.Background(), "https://test.invalid/v1/models", key)
	if err != nil || string(body) != `{"data":[]}` {
		t.Fatalf("curl result: %s, %v", body, err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if !strings.HasPrefix(args, "--disable\n") || strings.Contains(args, key) {
		t.Fatalf("unsafe curl arguments: %s", args)
	}
	for _, want := range []string{"--header\n@-", "--max-time\n10", "--connect-timeout\n5", "--proto\n=http,https"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(args, "--location") || strings.Contains(args, "--insecure") {
		t.Fatal("curl follows redirects or disables TLS validation")
	}
	header, err := os.ReadFile(headerPath)
	if err != nil || string(header) != "Authorization: Bearer "+key {
		t.Fatal("bearer header was not supplied on stdin")
	}
	lines := strings.Split(strings.TrimSpace(args), "\n")
	for i, arg := range lines {
		if arg == "--output" {
			if _, err := os.Stat(lines[i+1]); !os.IsNotExist(err) {
				t.Fatal("temporary response file was not removed")
			}
		}
	}
}

func TestCurlFailureDoesNotExposeKey(t *testing.T) {
	fakeCurl(t, "echo 'private-test-key' >&2\nexit 28\n")
	_, err := curlGet(context.Background(), "https://test.invalid/v1/models", "private-test-key")
	if err == nil || strings.Contains(err.Error(), "private-test-key") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("unsafe or unclear curl error: %v", err)
	}
}

func TestCurlRejectsHTTPFailureAndRedirect(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusFound, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://should-not-be-followed.invalid")
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		_, err := curlGet(context.Background(), server.URL+"/v1/models", "test-key")
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "status ") {
			t.Errorf("HTTP %d accepted: %v", code, err)
		}
	}
}

func TestCurlRejectsInvalidURLAndHeader(t *testing.T) {
	for _, endpoint := range []string{"file:///tmp/models", "https://user:password@test.invalid/models", "not-a-url"} {
		if _, err := curlGet(context.Background(), endpoint, "key"); err == nil {
			t.Errorf("invalid endpoint accepted: %s", endpoint)
		}
	}
	if _, err := curlGet(context.Background(), "https://test.invalid", "key\nInjected: header"); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestCurlCancellationStopsSubprocess(t *testing.T) {
	fakeCurl(t, "exec /bin/sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := curlGet(ctx, "https://test.invalid/models", "test-key")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("curl cancellation failed: %v", err)
	}
}

func TestCurlRejectsLargeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxResponseBytes+1)))
	}))
	defer server.Close()
	if _, err := curlGet(context.Background(), server.URL, "key"); err == nil || !strings.Contains(err.Error(), "response too large") {
		t.Fatalf("oversized response accepted: %v", err)
	}
}
