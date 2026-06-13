// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testResponse represents a single mock HTTP response for a RoundTripper.
type testResponse struct {
	status int
	body   string
	err    error
}

// mockTransport is a custom http.RoundTripper that returns a sequence of
// canned responses. It also tracks how many RoundTrip calls were made.
type mockTransport struct {
	responses []testResponse
	index     int
}

func (m *mockTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	if m.index >= len(m.responses) {
		return nil, errors.New("no more mock responses")
	}
	resp := m.responses[m.index]
	m.index++
	if resp.err != nil {
		return nil, resp.err
	}
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// TestRetryDelay exercises the retryDelay function directly.
func TestRetryDelay(t *testing.T) {
	orig := retryDelay
	defer func() { retryDelay = orig }()

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second}, // beyond max retries, verifies formula
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tc.attempt), func(t *testing.T) {
			got := retryDelay(tc.attempt)
			if got != tc.expected {
				t.Fatalf("expected %v, got %v", tc.expected, got)
			}
		})
	}
}

// TestGenerateEmbedding_Retries exercises every retry path in GenerateEmbedding
// via a mock http.RoundTripper. retryDelay is overridden to zero so tests
// execute instantly.
func TestGenerateEmbedding_Retries(t *testing.T) {
	orig := retryDelay
	retryDelay = func(_ int) time.Duration { return 0 }
	defer func() { retryDelay = orig }()

	tests := []struct {
		name           string
		responses      []testResponse
		expectedErr    bool
		expectedErrMsg string
		expectSuccess  bool
	}{
		// Happy path
		{
			name:          "success_first_attempt",
			responses:     []testResponse{{status: 200, body: `{"embedding":[0.1,0.2,0.3]}`}},
			expectSuccess: true,
		},
		// Transient failure -> success on 2nd attempt
		{
			name: "transient_failure_then_success",
			responses: []testResponse{
				{status: 0, err: errors.New("connection refused")},
				{status: 200, body: `{"embedding":[0.1,0.2,0.3]}`},
			},
			expectSuccess: true,
		},
		// Non-200 -> success on 2nd attempt
		{
			name: "non_200_then_success",
			responses: []testResponse{
				{status: 500, body: `{"error":"internal"}`},
				{status: 200, body: `{"embedding":[0.1,0.2,0.3]}`},
			},
			expectSuccess: true,
		},
		// JSON decode error -> success on 2nd attempt
		{
			name: "json_decode_error_then_success",
			responses: []testResponse{
				{status: 200, body: `not json`},
				{status: 200, body: `{"embedding":[0.1,0.2,0.3]}`},
			},
			expectSuccess: true,
		},
		// Empty embedding -> success on 2nd attempt
		{
			name: "empty_embedding_then_success",
			responses: []testResponse{
				{status: 200, body: `{"embedding":[]}`},
				{status: 200, body: `{"embedding":[0.1,0.2,0.3]}`},
			},
			expectSuccess: true,
		},
		// Persistent failure (exhaust all 3 retries)
		{
			name: "persistent_transport_failure_exhausts_retries",
			responses: []testResponse{
				{status: 0, err: errors.New("fail1")},
				{status: 0, err: errors.New("fail2")},
				{status: 0, err: errors.New("fail3")},
			},
			expectedErr:    true,
			expectedErrMsg: "fail3",
		},
		// Non-200 exhausted
		{
			name: "non_200_exhausts_retries",
			responses: []testResponse{
				{status: 503, body: "busy"},
				{status: 503, body: "busy"},
				{status: 503, body: "busy"},
			},
			expectedErr:    true,
			expectedErrMsg: "503",
		},
		// JSON decode exhausted
		{
			name: "json_decode_exhausts_retries",
			responses: []testResponse{
				{status: 200, body: `invalid json`},
				{status: 200, body: `{"bad":true}`},
				{status: 200, body: `{"also":"bad"}`},
			},
			expectedErr: true,
		},
		// Empty embedding exhausted
		{
			name: "empty_embedding_exhausts_retries",
			responses: []testResponse{
				{status: 200, body: `{"embedding":[]}`},
				{status: 200, body: `{"embedding":[]}`},
				{status: 200, body: `{"embedding":[]}`},
			},
			expectedErr:    true,
			expectedErrMsg: "empty embedding",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &mockTransport{responses: tc.responses}
			e := &Embedder{
				model:   "test-model",
				baseURL: "http://test.localhost",
				ready:   true,
				httpClient: &http.Client{
					Transport: tr,
				},
			}

			emb, err := e.GenerateEmbedding("test text")

			if tc.expectSuccess {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				if len(emb) == 0 {
					t.Fatal("expected non-empty embedding")
				}
			} else if tc.expectedErr {
				if err == nil {
					t.Fatal("expected error, got success")
				}
				if tc.expectedErrMsg != "" && !strings.Contains(err.Error(), tc.expectedErrMsg) {
					t.Fatalf("expected error containing %q, got %v", tc.expectedErrMsg, err)
				}
			}

			if tr.index != len(tc.responses) {
				t.Fatalf("expected %d requests, got %d", len(tc.responses), tr.index)
			}
		})
	}
}

// TestGenerateEmbedding_NotReady verifies the early return when the embedder
// is not ready (ready == false).
func TestGenerateEmbedding_NotReady(t *testing.T) {
	e := &Embedder{ready: false}
	emb, err := e.GenerateEmbedding("test")
	if err == nil {
		t.Fatal("expected error when embedder not ready")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("expected error containing 'not ready', got: %v", err)
	}
	if emb != nil {
		t.Fatalf("expected nil embedding, got: %v", emb)
	}
}
