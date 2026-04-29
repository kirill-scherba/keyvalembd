// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Default embedding model.
const defaultEmbeddingModel = "embeddinggemma:latest"

// Timeout for Ollama requests.
const ollamaTimeout = 30 * time.Second

// OllamaEmbeddingRequest is the request to Ollama for generating embeddings.
type OllamaEmbeddingRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// OllamaEmbeddingResponse is the response from Ollama.
type OllamaEmbeddingResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embedder handles communication with Ollama for generating embeddings.
type Embedder struct {
	model      string
	baseURL    string
	ready      bool
	httpClient *http.Client
	mu         sync.RWMutex
}

// NewEmbedder creates a new Embedder and checks Ollama availability.
func NewEmbedder(model, ollamaURL string) *Embedder {
	if model == "" {
		model = defaultEmbeddingModel
	}
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}
	e := &Embedder{
		model:   model,
		baseURL: ollamaURL,
		httpClient: &http.Client{
			Timeout: ollamaTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				MaxConnsPerHost:     10,
				MaxIdleConnsPerHost: 10,
			},
		},
	}

	if err := e.checkOllama(); err != nil {
		fmt.Printf("⚠️  Embedding search is not available: %v\n", err)
		e.ready = false
		return e
	}

	fmt.Printf("✅ Embeddings ready (model: %s)\n", model)
	e.ready = true
	return e
}

// Ready returns whether the embedder is ready for use.
func (e *Embedder) Ready() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.ready
}

// Model returns the current model name.
func (e *Embedder) Model() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.model
}

// checkOllama checks if Ollama is running and the model is available.
func (e *Embedder) checkOllama() error {
	e.mu.RLock()
	defer e.mu.RUnlock()

	resp, err := e.httpClient.Get(e.baseURL + "/api/tags")
	if err != nil {
		return fmt.Errorf("Ollama is not running. Start with: ollama serve")
	}
	defer resp.Body.Close()

	var models struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return err
	}

	modelName := strings.SplitN(e.model, ":", 2)[0]
	for _, m := range models.Models {
		if strings.Contains(m.Name, modelName) {
			return nil
		}
	}

	return fmt.Errorf("model %s is not installed. Install with: ollama pull %s",
		e.model, e.model)
}

const maxEmbeddingRetries = 3

func retryDelay(attempt int) time.Duration {
	return time.Duration(1<<(attempt-1)) * time.Second
}

// GenerateEmbedding sends text to Ollama and returns the embedding vector.
func (e *Embedder) GenerateEmbedding(text string) ([]float32, error) {
	e.mu.RLock()
	ready := e.ready
	model := e.model
	e.mu.RUnlock()

	if !ready {
		return nil, fmt.Errorf("embedder is not ready: Ollama is not available")
	}

	reqBody := OllamaEmbeddingRequest{Model: model, Prompt: text}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxEmbeddingRetries; attempt++ {
		resp, err := e.httpClient.Post(e.baseURL+"/api/embeddings",
			"application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("Ollama request failed: %w", err)
			if attempt < maxEmbeddingRetries {
				time.Sleep(retryDelay(attempt))
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("Ollama returned error %d: %s",
				resp.StatusCode, string(respBody))
			if attempt < maxEmbeddingRetries {
				time.Sleep(retryDelay(attempt))
				continue
			}
			return nil, lastErr
		}

		var result OllamaEmbeddingResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			lastErr = err
			if attempt < maxEmbeddingRetries {
				time.Sleep(retryDelay(attempt))
				continue
			}
			return nil, lastErr
		}
		resp.Body.Close()

		if len(result.Embedding) == 0 {
			lastErr = fmt.Errorf("received empty embedding from Ollama")
			if attempt < maxEmbeddingRetries {
				time.Sleep(retryDelay(attempt))
				continue
			}
			return nil, lastErr
		}

		return result.Embedding, nil
	}

	return nil, lastErr
}

// float32SliceToBytes converts a []float32 slice to a byte slice
// (little-endian).
func float32SliceToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, val := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(val))
	}
	return buf
}

// bytesToFloat32Slice converts a byte slice to a []float32 slice
// (little-endian).
func bytesToFloat32Slice(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := range a {
		dotProduct += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}
