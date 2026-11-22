package comfyui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/coder/websocket"
)

// client talks to a ComfyUI server's HTTP and WebSocket API.
type client struct {
	base string // e.g. http://127.0.0.1:8188
	http *http.Client
}

// apiError is returned for non-2xx responses.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("comfyui: HTTP %d: %s", e.Status, e.Body) }

func (c *client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *client) postJSON(ctx context.Context, path string, in, out interface{}) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, bytes.NewReader(b), "application/json", out)
}

// ping checks the server is up.
func (c *client) ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/system_stats", nil, "", nil)
}

// uploadInput stores a local file in ComfyUI's input dir under subfolder and
// returns the value to put in a loader node ("subfolder/name").
func (c *client) uploadInput(ctx context.Context, localPath, filename, subfolder string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := mw.WriteField("type", "input"); err != nil {
				return err
			}
			if err := mw.WriteField("subfolder", subfolder); err != nil {
				return err
			}
			if err := mw.WriteField("overwrite", "true"); err != nil {
				return err
			}
			part, err := mw.CreateFormFile("image", filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	var resp struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := c.do(ctx, http.MethodPost, "/upload/image", pr, mw.FormDataContentType(), &resp); err != nil {
		return "", fmt.Errorf("upload input: %w", err)
	}
	if resp.Subfolder == "" {
		return resp.Name, nil
	}
	return resp.Subfolder + "/" + resp.Name, nil
}

type promptResponse struct {
	PromptID   string          `json:"prompt_id"`
	NodeErrors json.RawMessage `json:"node_errors"`
}

// queuePrompt submits a graph; a validation failure is returned as *apiError (400).
func (c *client) queuePrompt(ctx context.Context, g Graph, clientID string) (string, error) {
	var resp promptResponse
	if err := c.postJSON(ctx, "/prompt", map[string]interface{}{"prompt": g, "client_id": clientID}, &resp); err != nil {
		return "", err
	}
	if resp.PromptID == "" {
		return "", fmt.Errorf("comfyui: no prompt_id in response")
	}
	return resp.PromptID, nil
}

// OutputFile is a file listed in a history entry's outputs.
type OutputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type historyEntry struct {
	Outputs map[string]map[string]json.RawMessage `json:"outputs"`
	Status  struct {
		StatusStr string              `json:"status_str"`
		Completed bool                `json:"completed"`
		Messages  [][]json.RawMessage `json:"messages"`
	} `json:"status"`
}

// history returns the history entry for promptID, or nil if not finished yet.
func (c *client) history(ctx context.Context, promptID string) (*historyEntry, error) {
	var resp map[string]*historyEntry
	if err := c.do(ctx, http.MethodGet, "/history/"+url.PathEscape(promptID), nil, "", &resp); err != nil {
		return nil, err
	}
	return resp[promptID], nil
}

// download saves an output file to w.
func (c *client) download(ctx context.Context, f OutputFile, w io.Writer) error {
	q := url.Values{"filename": {f.Filename}, "subfolder": {f.Subfolder}, "type": {f.Type}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/view?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &apiError{Status: resp.StatusCode, Body: "view " + f.Filename}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// cancel removes promptID from the queue and interrupts it if running.
func (c *client) cancel(ctx context.Context, promptID string) {
	_ = c.postJSON(ctx, "/queue", map[string]interface{}{"delete": []string{promptID}}, nil)
	_ = c.postJSON(ctx, "/interrupt", map[string]interface{}{"prompt_id": promptID}, nil)
}

// wsMessage is a JSON message from the ComfyUI websocket.
type wsMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// listen connects to the websocket for clientID and streams JSON messages
// until ctx is done or the connection drops. Binary preview frames are skipped.
func (c *client) listen(ctx context.Context, clientID string) (<-chan wsMessage, error) {
	wsURL := strings.Replace(c.base, "http", "ws", 1) + "/ws?clientId=" + url.QueryEscape(clientID)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: c.http})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	ch := make(chan wsMessage, 64)
	go func() {
		defer close(ch)
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var m wsMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			select {
			case ch <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}
