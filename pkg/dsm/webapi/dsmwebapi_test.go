/*
 * Copyright 2021 Synology Inc.
 */

package webapi

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSanitizeQueryForLogRedactsSensitiveValues(t *testing.T) {
	params := url.Values{}
	params.Set("api", "SYNO.API.Auth")
	params.Set("account", "admin")
	params.Set("passwd", "secret")
	params.Set("password", "another-secret")
	params.Set("sid", "session-id")
	params.Set("username", "volume-user")

	got := sanitizeQueryForLog(params)
	for _, raw := range []string{"admin", "secret", "another-secret", "session-id", "volume-user"} {
		if strings.Contains(got, raw) {
			t.Fatalf("sanitized query contains raw sensitive value %q: %s", raw, got)
		}
	}
	if !strings.Contains(got, "api=SYNO.API.Auth") {
		t.Fatalf("sanitized query dropped non-sensitive value: %s", got)
	}
	if strings.Count(got, "%3Credacted%3E") != 5 {
		t.Fatalf("sanitized query redaction count = %d, want 5: %s", strings.Count(got, "%3Credacted%3E"), got)
	}
}

// TestHTTPClientReuse verifies that the cached HTTP client is reused across
// multiple calls to initHTTPClient instead of creating a new client each time.
func TestHTTPClientReuse(t *testing.T) {
	dsm := &DSM{
		Ip:       "127.0.0.1",
		Port:     5000,
		Https:    false,
		Username: "testuser",
		Password: "testpass",
	}

	// Before any request, the client should be nil
	if dsm.httpClient != nil {
		t.Fatal("Expected httpClient to be nil before initialization")
	}

	// Call initHTTPClient once
	dsm.initHTTPClient()
	firstClient := dsm.httpClient
	if firstClient == nil {
		t.Fatal("Expected httpClient to be non-nil after initialization")
	}

	// Call initHTTPClient again — should not replace the client
	dsm.initHTTPClient()
	secondClient := dsm.httpClient
	if secondClient != firstClient {
		t.Fatal("Expected httpClient to be the same instance after second initHTTPClient call")
	}
}

// TestHTTPClientDoLazyInit verifies that httpClientDo initializes the client
// lazily on first call and that subsequent calls reuse the same client.
func TestHTTPClientDoLazyInit(t *testing.T) {
	dsm := &DSM{
		Ip:       "127.0.0.1",
		Port:     5000,
		Https:    false,
		Username: "testuser",
		Password: "testpass",
	}

	// Verify the client is nil before any call
	if dsm.httpClient != nil {
		t.Fatal("Expected httpClient to be nil before httpClientDo")
	}

	// httpClientDo should initialize the client lazily.
	// The request will fail because there's no server, but the client is created.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:5000/webapi/entry.cgi", nil)
	_, _ = dsm.httpClientDo(req) // ignore error — no real server

	clientAfter := dsm.httpClient
	if clientAfter == nil {
		t.Fatal("Expected httpClient to be initialized after httpClientDo call")
	}

	// A subsequent call should reuse the same client
	clientBefore := clientAfter
	req2, _ := http.NewRequest("GET", "http://127.0.0.1:5000/webapi/entry.cgi", nil)
	_, _ = dsm.httpClientDo(req2)

	if dsm.httpClient != clientBefore {
		t.Fatal("Expected httpClient to be reused (same pointer) on subsequent httpClientDo call")
	}
}

// TestConcurrentClientInit verifies that concurrent access to the cached client
// initialization is safe and does not lead to race conditions.
func TestConcurrentClientInit(t *testing.T) {
	dsm := &DSM{
		Ip:       "127.0.0.1",
		Port:     5000,
		Https:    false,
		Username: "testuser",
		Password: "testpass",
	}

	var wg sync.WaitGroup
	clients := make([]*http.Client, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dsm.clientInitMu.Lock()
			if dsm.httpClient == nil {
				dsm.httpClient = newDSMHTTPClient(dsm.Https)
			}
			clients[idx] = dsm.httpClient
			dsm.clientInitMu.Unlock()
		}(i)
	}
	wg.Wait()

	// All goroutines should see the same client instance
	first := clients[0]
	for i := 1; i < 100; i++ {
		if clients[i] != first {
			t.Fatalf("Concurrent client init produced different instances at index %d", i)
		}
	}
}

// TestClientInitMuNoDeadlock verifies that clientInitMu does not deadlock
// when initHTTPClient is called under reqMu (the normal pattern).
func TestClientInitMuNoDeadlock(t *testing.T) {
	dsm := &DSM{
		Ip:       "127.0.0.1",
		Port:     5000,
		Https:    false,
		Username: "testuser",
		Password: "testpass",
	}

	done := make(chan struct{}, 1)
	go func() {
		dsm.reqMu.Lock()
		defer dsm.reqMu.Unlock()
		dsm.initHTTPClient()
		done <- struct{}{}
	}()
	select {
	case <-done:
		// OK — no deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("Deadlock detected: initHTTPClient under reqMu hung")
	}
}

// TestLoginInitializesClient verifies that Login() initializes the cached client.
func TestLoginInitializesClient(t *testing.T) {
	dsm := &DSM{
		Ip:       "127.0.0.1",
		Port:     5000,
		Https:    false,
		Username: "testuser",
		Password: "testpass",
	}

	// Before Login, the client should be nil
	if dsm.httpClient != nil {
		t.Fatal("Expected httpClient to be nil before Login")
	}

	// Login will try to make an HTTP request (which will fail since there's no server)
	_ = dsm.Login()

	// But the client should have been initialized regardless of the request result
	if dsm.httpClient == nil {
		t.Fatal("Expected httpClient to be initialized after Login attempt")
	}
}

// TestMultipleDSMInstances verifies that each DSM instance has its own cached client.
func TestMultipleDSMInstances(t *testing.T) {
	dsm1 := &DSM{
		Ip:       "192.168.1.1",
		Port:     5000,
		Https:    false,
		Username: "user1",
		Password: "pass1",
	}
	dsm2 := &DSM{
		Ip:       "192.168.1.2",
		Port:     5001,
		Https:    true,
		Username: "user2",
		Password: "pass2",
	}

	dsm1.initHTTPClient()
	dsm2.initHTTPClient()

	if dsm1.httpClient == nil || dsm2.httpClient == nil {
		t.Fatal("Expected both DSM instances to have initialized clients")
	}
	if dsm1.httpClient == dsm2.httpClient {
		t.Fatal("Expected different DSM instances to have different HTTP clients")
	}
}

func TestGetAnotherControllerDoesNotMutateSharedDSM(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("api"); got != "SYNO.Core.Network.Interface" {
			t.Fatalf("unexpected api query: %s", got)
		}
		ifaces := []NetworkInterface{
			{
				Ifname: "eth0",
				Ip:     "127.0.0.1",
				Status: "connected",
			},
		}
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"data":    ifaces,
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, rawPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}

	dsm := &DSM{
		Ip:         host,
		Port:       port,
		Controller: "primary",
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			another, err := dsm.GetAnotherController()
			if err != nil {
				errs <- err
				return
			}
			if another.Controller != "A" {
				errs <- &unexpectedControllerError{got: another.Controller}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if dsm.Controller != "primary" {
		t.Fatalf("GetAnotherController mutated shared DSM Controller = %q", dsm.Controller)
	}
}

type unexpectedControllerError struct {
	got string
}

func (err *unexpectedControllerError) Error() string {
	return "unexpected another controller: " + err.got
}
