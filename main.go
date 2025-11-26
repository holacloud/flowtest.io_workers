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

			workerID, err = worker.RegisterWorker(ctx, serverClient, *server, *suite, *challenge, *workerName)
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

func pullRequest(ctx context.Context, client *http.Client, server, workerID string) (*worker.ProxyRequest, error) {
	url := worker.Endpoint(server, fmt.Sprintf("/agent/v1/workers/%s/pull", workerID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}

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
	url := worker.Endpoint(server, fmt.Sprintf("/agent/v1/workers/%s/result", workerID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("submit failed: %s", strings.TrimSpace(string(data)))
	}
	return nil
}
