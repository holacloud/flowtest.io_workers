package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		path     string
		expected string
	}{
		{
			name:     "base with trailing slash",
			base:     "https://example.com/",
			path:     "/api/v1/test",
			expected: "https://example.com/api/v1/test",
		},
		{
			name:     "base without trailing slash",
			base:     "https://example.com",
			path:     "/api/v1/test",
			expected: "https://example.com/api/v1/test",
		},
		{
			name:     "base with multiple trailing slashes",
			base:     "https://example.com///",
			path:     "/api/v1/test",
			expected: "https://example.com/api/v1/test",
		},
		{
			name:     "empty path",
			base:     "https://example.com",
			path:     "",
			expected: "https://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Endpoint(tt.base, tt.path)
			if result != tt.expected {
				t.Errorf("Endpoint(%q, %q) = %q; want %q", tt.base, tt.path, result, tt.expected)
			}
		})
	}
}

func TestSleepWithContext(t *testing.T) {
	t.Run("completes normally", func(t *testing.T) {
		ctx := context.Background()
		start := time.Now()
		result := SleepWithContext(ctx, 50*time.Millisecond)
		duration := time.Since(start)

		if !result {
			t.Error("SleepWithContext should return true when completing normally")
		}
		if duration < 50*time.Millisecond {
			t.Errorf("SleepWithContext completed too quickly: %v", duration)
		}
	})

	t.Run("cancelled by context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		// Cancel context after a short delay
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		result := SleepWithContext(ctx, 1*time.Second)
		duration := time.Since(start)

		if result {
			t.Error("SleepWithContext should return false when context is cancelled")
		}
		if duration >= 1*time.Second {
			t.Errorf("SleepWithContext should have been cancelled early, took %v", duration)
		}
	})

	t.Run("already cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		result := SleepWithContext(ctx, 100*time.Millisecond)
		if result {
			t.Error("SleepWithContext should return false for already cancelled context")
		}
	})
}

func TestRegisterWorker(t *testing.T) {
	t.Run("successful registration", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Verify request
			if r.Method != http.MethodPost {
				t.Errorf("Expected POST request, got %s", r.Method)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Expected Content-Type: application/json")
			}

			// Parse request body
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("Failed to decode request body: %v", err)
			}

			// Verify payload
			if payload["suite_id"] != "test-suite" {
				t.Errorf("Expected suite_id=test-suite, got %s", payload["suite_id"])
			}
			if payload["challenge"] != "test-challenge" {
				t.Errorf("Expected challenge=test-challenge, got %s", payload["challenge"])
			}
			if payload["name"] != "test-worker" {
				t.Errorf("Expected name=test-worker, got %s", payload["name"])
			}

			// Send successful response
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"worker_id": "worker-123",
			})
		}))
		defer server.Close()

		ctx := context.Background()
		client := &http.Client{Timeout: 5 * time.Second}
		workerID, err := RegisterWorker(ctx, client, server.URL, "test-suite", "test-challenge", "test-worker")

		if err != nil {
			t.Fatalf("RegisterWorker failed: %v", err)
		}
		if workerID != "worker-123" {
			t.Errorf("Expected worker_id=worker-123, got %s", workerID)
		}
	})

	t.Run("registration without name", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload map[string]string
			json.NewDecoder(r.Body).Decode(&payload)

			// Verify name is not in payload when empty
			if _, exists := payload["name"]; exists {
				t.Error("Empty name should not be included in payload")
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"worker_id": "worker-456",
			})
		}))
		defer server.Close()

		ctx := context.Background()
		client := &http.Client{Timeout: 5 * time.Second}
		workerID, err := RegisterWorker(ctx, client, server.URL, "test-suite", "test-challenge", "")

		if err != nil {
			t.Fatalf("RegisterWorker failed: %v", err)
		}
		if workerID != "worker-456" {
			t.Errorf("Expected worker_id=worker-456, got %s", workerID)
		}
	})

	t.Run("empty worker_id in response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"worker_id": "",
			})
		}))
		defer server.Close()

		ctx := context.Background()
		client := &http.Client{Timeout: 5 * time.Second}
		_, err := RegisterWorker(ctx, client, server.URL, "test-suite", "test-challenge", "test-worker")

		if err == nil {
			t.Fatal("Expected error for empty worker_id")
		}
		if err.Error() != "empty worker id returned" {
			t.Errorf("Unexpected error: %v", err)
		}
	})

	t.Run("client error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("invalid request"))
		}))
		defer server.Close()

		ctx := context.Background()
		client := &http.Client{Timeout: 5 * time.Second}
		_, err := RegisterWorker(ctx, client, server.URL, "test-suite", "test-challenge", "test-worker")

		if err == nil {
			t.Fatal("Expected error for 400 response")
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		// Create a server that never responds
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1 * time.Second) // Simulate long delay
		}))
		defer server.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		client := &http.Client{Timeout: 5 * time.Second}
		_, err := RegisterWorker(ctx, client, server.URL, "test-suite", "test-challenge", "test-worker")

		if err == nil {
			t.Fatal("Expected error when context is cancelled")
		}
		if ctx.Err() != context.DeadlineExceeded {
			t.Errorf("Expected context.DeadlineExceeded, got %v", ctx.Err())
		}
	})
}

func TestProxyRequestMarshaling(t *testing.T) {
	t.Run("marshal and unmarshal with all fields", func(t *testing.T) {
		original := &ProxyRequest{
			ID:     "req-123",
			Method: "POST",
			URL:    "https://example.com/api/test",
			Header: http.Header{
				"Content-Type": []string{"application/json"},
				"X-Custom":     []string{"value1", "value2"},
			},
			Host: "custom-host.com",
			Body: []byte("test body content"),
		}

		// Marshal to JSON
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("Failed to marshal ProxyRequest: %v", err)
		}

		// Unmarshal back
		var decoded ProxyRequest
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal ProxyRequest: %v", err)
		}

		// Verify fields
		if decoded.ID != original.ID {
			t.Errorf("ID mismatch: got %q, want %q", decoded.ID, original.ID)
		}
		if decoded.Method != original.Method {
			t.Errorf("Method mismatch: got %q, want %q", decoded.Method, original.Method)
		}
		if decoded.URL != original.URL {
			t.Errorf("URL mismatch: got %q, want %q", decoded.URL, original.URL)
		}
		if decoded.Host != original.Host {
			t.Errorf("Host mismatch: got %q, want %q", decoded.Host, original.Host)
		}
		if string(decoded.Body) != string(original.Body) {
			t.Errorf("Body mismatch: got %q, want %q", decoded.Body, original.Body)
		}
	})

	t.Run("marshal with omitempty fields", func(t *testing.T) {
		req := &ProxyRequest{
			ID:     "req-456",
			Method: "GET",
			URL:    "https://example.com/",
			Header: http.Header{},
			// Host and Body are empty, should be omitted
		}

		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var rawMap map[string]interface{}
		if err := json.Unmarshal(data, &rawMap); err != nil {
			t.Fatalf("Failed to unmarshal to map: %v", err)
		}

		// Verify omitempty fields are not present
		if _, exists := rawMap["host"]; exists {
			t.Error("Empty host field should be omitted")
		}
		if _, exists := rawMap["body"]; exists {
			t.Error("Empty body field should be omitted")
		}
	})
}

func TestProxyResponseMarshaling(t *testing.T) {
	t.Run("marshal and unmarshal with all fields", func(t *testing.T) {
		original := &ProxyResponse{
			RequestID: "req-789",
			Status:    200,
			Header: http.Header{
				"Content-Type":   []string{"application/json"},
				"Content-Length": []string{"42"},
			},
			Body:       []byte("response body"),
			Error:      "",
			WorkerName: "worker-abc",
		}

		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("Failed to marshal ProxyResponse: %v", err)
		}

		var decoded ProxyResponse
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal ProxyResponse: %v", err)
		}

		if decoded.RequestID != original.RequestID {
			t.Errorf("RequestID mismatch: got %q, want %q", decoded.RequestID, original.RequestID)
		}
		if decoded.Status != original.Status {
			t.Errorf("Status mismatch: got %d, want %d", decoded.Status, original.Status)
		}
		if string(decoded.Body) != string(original.Body) {
			t.Errorf("Body mismatch: got %q, want %q", decoded.Body, original.Body)
		}
		if decoded.WorkerName != original.WorkerName {
			t.Errorf("WorkerName mismatch: got %q, want %q", decoded.WorkerName, original.WorkerName)
		}
	})

	t.Run("marshal with error", func(t *testing.T) {
		resp := &ProxyResponse{
			RequestID: "req-error",
			Status:    0,
			Header:    http.Header{},
			Error:     "connection timeout",
		}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var decoded ProxyResponse
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Failed to unmarshal: %v", err)
		}

		if decoded.Error != resp.Error {
			t.Errorf("Error mismatch: got %q, want %q", decoded.Error, resp.Error)
		}
	})

	t.Run("marshal with omitempty fields", func(t *testing.T) {
		resp := &ProxyResponse{
			RequestID: "req-minimal",
			Status:    204,
			Header:    http.Header{},
			// Body, Error, and WorkerName are empty
		}

		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("Failed to marshal: %v", err)
		}

		var rawMap map[string]interface{}
		if err := json.Unmarshal(data, &rawMap); err != nil {
			t.Fatalf("Failed to unmarshal to map: %v", err)
		}

		// Verify omitempty fields
		if _, exists := rawMap["body"]; exists {
			t.Error("Empty body field should be omitted")
		}
		if _, exists := rawMap["error"]; exists {
			t.Error("Empty error field should be omitted")
		}
		if _, exists := rawMap["worker_name"]; exists {
			t.Error("Empty worker_name field should be omitted")
		}
	})
}

func TestProxyRequestJSONStructure(t *testing.T) {
	jsonData := `{
		"id": "test-id",
		"method": "PUT",
		"url": "https://api.example.com/resource",
		"header": {
			"Authorization": ["Bearer token123"],
			"Accept": ["application/json"]
		},
		"host": "api.example.com",
		"body": "eyJ0ZXN0IjoidmFsdWUifQ=="
	}`

	var req ProxyRequest
	if err := json.Unmarshal([]byte(jsonData), &req); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if req.ID != "test-id" {
		t.Errorf("ID = %q; want %q", req.ID, "test-id")
	}
	if req.Method != "PUT" {
		t.Errorf("Method = %q; want %q", req.Method, "PUT")
	}
	if req.URL != "https://api.example.com/resource" {
		t.Errorf("URL = %q; want %q", req.URL, "https://api.example.com/resource")
	}
	if req.Host != "api.example.com" {
		t.Errorf("Host = %q; want %q", req.Host, "api.example.com")
	}
	if req.Header.Get("Authorization") != "Bearer token123" {
		t.Errorf("Authorization header = %q; want %q", req.Header.Get("Authorization"), "Bearer token123")
	}
}

func TestProxyResponseJSONStructure(t *testing.T) {
	jsonData := `{
		"request_id": "request-xyz",
		"status": 201,
		"header": {
			"Location": ["https://example.com/resource/123"]
		},
		"body": "cmVzcG9uc2UgZGF0YQ==",
		"worker_name": "worker-001"
	}`

	var resp ProxyResponse
	if err := json.Unmarshal([]byte(jsonData), &resp); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	if resp.RequestID != "request-xyz" {
		t.Errorf("RequestID = %q; want %q", resp.RequestID, "request-xyz")
	}
	if resp.Status != 201 {
		t.Errorf("Status = %d; want %d", resp.Status, 201)
	}
	if resp.WorkerName != "worker-001" {
		t.Errorf("WorkerName = %q; want %q", resp.WorkerName, "worker-001")
	}
	if resp.Header.Get("Location") != "https://example.com/resource/123" {
		t.Errorf("Location header = %q", resp.Header.Get("Location"))
	}
}
