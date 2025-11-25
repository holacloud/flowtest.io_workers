package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/holacloud/flowtest_workers/config"
	"github.com/holacloud/flowtest_workers/worker"
)

const maxResponseBody = 512 * 1024

func main() {

	fmt.Println("flowtest.io worker")
	fmt.Println("version: ", config.VERSION)

	server := flag.String("server", "https://flowtest.io", "Flowtest server base URL")
	suite := flag.String("suite", "", "Suite identifier to join")
	challenge := flag.String("challenge", "", "Challenge")
	workerName := flag.String("name", "", "Human-friendly identifier for this worker")
	timeout := flag.Duration("timeout", 30*time.Second, "HTTP timeout for proxied requests")
	flag.Parse()

	if strings.TrimSpace(*suite) == "" {
		log.Fatal("suite id is required (use -suite)")
	}

	if strings.TrimSpace(*challenge) == "" {
		log.Fatal("challenge is required (use -challenge)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	serverClient := &http.Client{Timeout: 30 * time.Second}
	targetClient := &http.Client{
		Timeout:       *timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}

	workerID := "new-worker"

	for {
		select {
		case <-ctx.Done():
			log.Println("context canceled; shutting down")
			return
		default:
		}

		req, err := pullRequest(ctx, serverClient, *server, workerID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			log.Printf("pull request error: %v", err)
			time.Sleep(2 * time.Second)

			workerID, err = registerWorker(ctx, serverClient, *server, *suite, *challenge, *workerName)
			if err != nil {
				log.Printf("register worker: %v", err)
				time.Sleep(2 * time.Second)
			}
			if strings.TrimSpace(*workerName) != "" {
				log.Printf("registered worker %s (%s) for suite %s", workerID, *workerName, *suite)
			} else {
				log.Printf("registered worker %s for suite %s", workerID, *suite)
			}
			continue
		}

		if req == nil {
			continue
		}

		response := executeRequest(ctx, targetClient, req)

		if err := submitResponse(ctx, serverClient, *server, workerID, response); err != nil {
			log.Printf("submit response error: %v", err)
			time.Sleep(time.Second)
		}
	}
}

func registerWorker(ctx context.Context, client *http.Client, server, suite, challenge, name string) (string, error) {
	payload := map[string]string{"suite_id": suite, "challenge": challenge}
	trimmedName := strings.TrimSpace(name)
	if trimmedName != "" {
		payload["name"] = trimmedName
	}
	body, _ := json.Marshal(payload)
	url := endpoint(server, "/agent/v1/workers/register")

	for {
		reqBody := bytes.NewReader(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, reqBody)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			log.Printf("register worker: %v; retrying in 5s", err)
			if !sleepWithContext(ctx, 5*time.Second) {
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
			if !sleepWithContext(ctx, 5*time.Second) {
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

func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func pullRequest(ctx context.Context, client *http.Client, server, workerID string) (*worker.ProxyRequest, error) {
	url := endpoint(server, fmt.Sprintf("/agent/v1/workers/%s/pull", workerID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("pull failed: %s", strings.TrimSpace(string(data)))
	}

	var proxyReq worker.ProxyRequest
	if err := json.NewDecoder(resp.Body).Decode(&proxyReq); err != nil {
		return nil, err
	}
	return &proxyReq, nil
}

func executeRequest(ctx context.Context, client *http.Client, proxyReq *worker.ProxyRequest) *worker.ProxyResponse {
	response := &worker.ProxyResponse{RequestID: proxyReq.ID, Header: http.Header{}}

	reqBody := bytes.NewReader(proxyReq.Body)
	req, err := http.NewRequestWithContext(ctx, proxyReq.Method, proxyReq.URL, reqBody)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	if proxyReq.Header != nil {
		req.Header = proxyReq.Header.Clone()
	}
	if proxyReq.Host != "" {
		req.Host = proxyReq.Host
	}

	resp, err := client.Do(req)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	defer resp.Body.Close()

	response.Status = resp.StatusCode
	response.Header = resp.Header.Clone()

	limited := io.LimitReader(resp.Body, maxResponseBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		response.Error = err.Error()
		return response
	}
	if len(data) > maxResponseBody {
		response.Body = data[:maxResponseBody]
		response.Error = fmt.Sprintf("response too large (>%d bytes)", maxResponseBody)
		return response
	}
	response.Body = data
	return response
}

func submitResponse(ctx context.Context, client *http.Client, server, workerID string, response *worker.ProxyResponse) error {
	body, _ := json.Marshal(response)
	url := endpoint(server, fmt.Sprintf("/agent/v1/workers/%s/result", workerID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("submit failed: %s", strings.TrimSpace(string(data)))
	}
	return nil
}

func endpoint(base, path string) string {
	trimmed := strings.TrimRight(base, "/")
	return trimmed + path
}
