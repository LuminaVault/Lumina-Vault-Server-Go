package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// cohereEmbed stands in for Cohere's POST /v2/embed, so both servers run their
// real adapter against one deterministic upstream. It refuses a request of the
// wrong shape with a 418, which a server reports as an upstream rejection: a
// shape only one server gets wrong shows up as a parity difference.
//
// The decoded image bytes choose the answer:
//
//	cohere-400      400
//	cohere-429      429
//	cohere-500      500
//	cohere-garbage  200 with a body that is not JSON
//	cohere-empty    200 with an empty vector
//	cohere-null     200 with a null inside the vector
//	cohere-nokey    200 without `embeddings`
//	cohere-hangup   closes the connection without a response
//	cohere-long     1600 dimensions, wider than memories.embedding
//	anything else   a short vector whose values print differently across languages
func cohereEmbed(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model          string   `json:"model"`
		InputType      string   `json:"input_type"`
		EmbeddingTypes []string `json:"embedding_types"`
		Images         []string `json:"images"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		r.Header.Get("Authorization") != "Bearer parity-cohere-key" ||
		r.Header.Get("Content-Type") != "application/json" ||
		req.Model != "embed-image-v3.0" || req.InputType != "image" ||
		len(req.EmbeddingTypes) != 1 || req.EmbeddingTypes[0] != "float" || len(req.Images) != 1 {
		http.Error(w, "bad embed request", http.StatusTeapot)
		return
	}
	_, encoded, ok := strings.Cut(req.Images[0], ";base64,")
	image, err := base64.StdEncoding.DecodeString(encoded)
	if !ok || !strings.HasPrefix(req.Images[0], "data:image/") || err != nil {
		http.Error(w, "bad data uri", http.StatusTeapot)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch string(image) {
	case "cohere-400":
		http.Error(w, "invalid image", http.StatusBadRequest)
	case "cohere-429":
		http.Error(w, "slow down", http.StatusTooManyRequests)
	case "cohere-500":
		http.Error(w, "upstream down", http.StatusInternalServerError)
	case "cohere-garbage":
		_, _ = w.Write([]byte("not json"))
	case "cohere-empty":
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[]]}}`))
	case "cohere-null":
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.5,null]]}}`))
	case "cohere-nokey":
		_, _ = w.Write([]byte(`{"id":"x"}`))
	case "cohere-hangup":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	case "cohere-long":
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.5` + strings.Repeat(",0.25", 1599) + `]]}}`))
	default:
		_, _ = w.Write([]byte(`{"id":"x","embeddings":{"float":[[0.1,-0.25,1.5e-05,0.0001,0.00009,1,0]]},"texts":[]}`))
	}
}
