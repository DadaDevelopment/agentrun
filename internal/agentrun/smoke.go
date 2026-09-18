package agentrun

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Smoke sends one real A2A turn and reports whether the agent said anything.
// A transport 200 with an empty artifact is a failure: the question is whether
// the agent answered, not whether the server accepted the request.
func Smoke(port int, text string, timeout time.Duration, out io.Writer) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      randomID(),
		"method":  "message/send",
		"params": map[string]any{
			"message": map[string]any{
				"role":      "user",
				"messageId": randomID(),
				"parts":     []map[string]string{{"kind": "text", "text": text}},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	started := time.Now()
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/", port), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("smoke: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(out, "agent A2A message/send -> HTTP %d in %.1fs\n", resp.StatusCode, time.Since(started).Seconds())

	var answer struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Artifacts []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"artifacts"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		fmt.Fprintf(out, "  %s\n", truncate(string(raw), 400))
		return fmt.Errorf("smoke: response is not JSON")
	}
	if len(answer.Error) > 0 {
		fmt.Fprintf(out, "  error: %s\n", truncate(string(answer.Error), 400))
	}
	var text2 string
	for _, artifact := range answer.Result.Artifacts {
		for _, part := range artifact.Parts {
			text2 += part.Text
		}
	}
	if resp.StatusCode != http.StatusOK || text2 == "" {
		fmt.Fprintf(out, "  %s\nsmoke: FAIL\n", truncate(string(raw), 400))
		return fmt.Errorf("the agent produced no text")
	}
	fmt.Fprintf(out, "  %s\nsmoke: OK\n", truncate(text2, 600))
	return nil
}

func randomID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
