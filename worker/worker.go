package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// ProxyRequest represents an HTTP request that must be executed by a remote worker.
type ProxyRequest struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	URL    string      `json:"url"`
	Header http.Header `json:"header"`
	Host   string      `json:"host,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// ProxyResponse is the remote worker response for a proxied request.
type ProxyResponse struct {
	RequestID string      `json:"request_id"`
	Status    int         `json:"status"`
	Header    http.Header `json:"header"`
	Body      []byte      `json:"body,omitempty"`
	Error     string      `json:"error,omitempty"`

	WorkerName string `json:"worker_name,omitempty"`
}

// RegisterWorker registers a new worker with the Flowtest server.
// It retries on transient errors until successful or the context is cancelled.
func RegisterWorker(ctx context.Context, client *http.Client, server, suite, challenge, name string) (string, error) {
	payload := map[string]string{"suite_id": suite, "challenge": challenge}
	trimmedName := strings.TrimSpace(name)
	if trimmedName != "" {
		payload["name"] = trimmedName
	}
	body, _ := json.Marshal(payload)
	url := Endpoint(server, "/agent/v1/workers/register")

	for {
		reqBody := io.NopCloser(strings.NewReader(string(body)))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reqBody)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			log.Printf("register worker: %v; retrying in 5s", err)
			if !SleepWithContext(ctx, 5*time.Second) {
				return "", ctx.Err()
			}
			continue
		}

		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			var result struct {
				WorkerID string `json:"worker_id"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return "", err
			}
			if result.WorkerID == "" {
				return "", errors.New("empty worker id returned")
			}
			return result.WorkerID, nil
		}

		trimmed := strings.TrimSpace(string(data))
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			if trimmed == "" {
				trimmed = resp.Status
			}
			log.Printf("register worker failed (%d): %s; retrying in 5s", resp.StatusCode, trimmed)
			if !SleepWithContext(ctx, 5*time.Second) {
				return "", ctx.Err()
			}
			continue
		}

		if trimmed == "" {
			trimmed = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return "", fmt.Errorf("register worker failed: %s", trimmed)
	}
}

// SleepWithContext sleeps for the specified duration or until the context is cancelled.
// Returns true if the sleep completed normally, false if the context was cancelled.
func SleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Endpoint constructs a full URL by combining a base server URL with a path.
func Endpoint(base, path string) string {
	trimmed := strings.TrimRight(base, "/")
	return trimmed + path
}
