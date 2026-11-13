package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shurshur/magnet/internal/daemon"
	"github.com/shurshur/magnet/internal/model"
)

// Error is a non-2xx API response.
type Error struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
}

// Client talks to a running magnet daemon.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient creates an API client. A missing scheme defaults to http://.
func NewClient(baseURL, token string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8787"
	}
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// AddResult is returned by Add.
type AddResult struct {
	Jobs     []model.Job
	Created  int
	Existing int
}

// Add submits one or more magnet links.
func (c *Client) Add(ctx context.Context, magnets []string) (AddResult, error) {
	var resp struct {
		Jobs     []model.Job `json:"jobs"`
		Created  int         `json:"created"`
		Existing int         `json:"existing"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/jobs", map[string]any{"magnets": magnets}, &resp)
	return AddResult{Jobs: resp.Jobs, Created: resp.Created, Existing: resp.Existing}, err
}

// Get returns one job.
func (c *Client) Get(ctx context.Context, id string) (model.Job, error) {
	var resp struct {
		Job model.Job `json:"job"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil, &resp)
	return resp.Job, err
}

// ListResult is a page of jobs.
type ListResult struct {
	Jobs   []model.Job `json:"jobs"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// List returns jobs, optionally filtered by state.
func (c *Client) List(ctx context.Context, state model.State, limit, offset int) (ListResult, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", string(state))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	path := "/v1/jobs"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp ListResult
	err := c.doJSON(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}

// Cancel cancels a queued or searching job.
func (c *Client) Cancel(ctx context.Context, id string) (model.Job, error) {
	var resp struct {
		Job model.Job `json:"job"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", nil, &resp)
	return resp.Job, err
}

// Retry re-queues a failed or canceled job.
func (c *Client) Retry(ctx context.Context, id string) (model.Job, error) {
	var resp struct {
		Job model.Job `json:"job"`
	}
	err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/retry", nil, &resp)
	return resp.Job, err
}

// Delete removes a job from the daemon. purge also deletes the .torrent file.
func (c *Client) Delete(ctx context.Context, id string, purge bool) error {
	path := "/v1/jobs/" + url.PathEscape(id)
	if purge {
		path += "?purge=true"
	}
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// StatsResponse is the response of GET /v1/stats.
type StatsResponse struct {
	Stats         daemon.Stats `json:"stats"`
	UptimeSeconds int64        `json:"uptime_seconds"`
}

// Stats returns daemon counters.
func (c *Client) Stats(ctx context.Context) (StatsResponse, error) {
	var resp StatsResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/stats", nil, &resp)
	return resp, err
}

// HealthResponse is the response of GET /healthz.
type HealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// Health checks whether the daemon is alive.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var resp HealthResponse
	err := c.doJSON(ctx, http.MethodGet, "/healthz", nil, &resp)
	return resp, err
}

// OpenTorrent downloads a completed .torrent file.
func (c *Client) OpenTorrent(ctx context.Context, id string) (io.ReadCloser, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/jobs/"+url.PathEscape(id)+"/torrent", nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/x-bittorrent")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request %s: %w", c.BaseURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, "", decodeError(resp)
	}
	filename := ""
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			filename = params["filename"]
		}
	}
	return resp.Body, filename, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		return &Error{
			StatusCode: resp.StatusCode,
			Code:       envelope.Error.Code,
			Message:    envelope.Error.Message,
		}
	}
	msg := strings.TrimSpace(string(data))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{StatusCode: resp.StatusCode, Message: msg}
}
