// Package observe supplies the generated terraform for a dataset.
//
// The Source interface in source.go is the contract; everything else here is one way of satisfying it
// — a GraphQL client for the Observe meta API, its getTerraform call, an on-disk cache that wraps any
// Source, and a fixture-backed Source for tests.
package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client issues GraphQL requests against one tenant's meta endpoint.
type Client struct {
	URL        string
	CustomerID string
	Token      string
	HTTP       *http.Client

	// Retries is the number of additional attempts after the first failure.
	Retries int
	// RetryWait is the base delay, doubled per attempt.
	RetryWait time.Duration
}

// New builds a Client with defaults suited to a few hundred sequential calls.
func New(url, customerID, token string) *Client {
	return &Client{
		URL:        url,
		CustomerID: customerID,
		Token:      token,
		HTTP:       &http.Client{Timeout: 90 * time.Second},
		Retries:    4,
		RetryWait:  500 * time.Millisecond,
	}
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message string `json:"message"`
	Path    []any  `json:"path,omitempty"`
}

func (e gqlError) Error() string { return e.Message }

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors,omitempty"`
}

// Query executes one GraphQL operation and unmarshals the `data` field into out.
func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}

	var lastErr error
	wait := c.RetryWait
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}

		raw, err := c.do(ctx, body)
		if err != nil {
			lastErr = err
			if !retryable(err) {
				return err
			}
			continue
		}

		var resp gqlResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		if len(resp.Errors) > 0 {
			// GraphQL-level errors are the server rejecting the request, not a transport
			// hiccup: retrying sends the identical query and gets the identical answer.
			msgs := make([]string, len(resp.Errors))
			for i, e := range resp.Errors {
				msgs[i] = e.Message
			}
			return fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Data, out)
	}
	return fmt.Errorf("after %d attempts: %w", c.Retries+1, lastErr)
}

// httpStatusError marks a response whose status code decides retryability.
type httpStatusError struct {
	Code int
	Body string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Code, e.Body)
}

func retryable(err error) bool {
	var se *httpStatusError
	if !errors.As(err, &se) {
		// Transport errors (dial, timeout, EOF) are worth another attempt.
		return true
	}
	return se.Code == http.StatusTooManyRequests || se.Code >= 500
}

func (c *Client) do(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s %s", c.CustomerID, c.Token))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(raw)
		if len(snippet) > 400 {
			snippet = snippet[:400] + "…"
		}
		return nil, &httpStatusError{Code: resp.StatusCode, Body: snippet}
	}
	return raw, nil
}
