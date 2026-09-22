package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultEndpoint is the Hugging Face host.
const DefaultEndpoint = "https://huggingface.co"

// HFClient fetches models from a Hugging Face-compatible host.
//
// The endpoint is configurable because huggingface.co is unreachable or very
// slow on some networks, and a user whose 3 GB download fails at 80% has no
// path forward without a mirror.
type HFClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHFClient builds a client, defaulting the endpoint and the transport.
func NewHFClient(endpoint, token string) *HFClient {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &HFClient{
		BaseURL: strings.TrimRight(endpoint, "/"),
		Token:   token,
		HTTP: &http.Client{
			// No overall timeout: a 3 GB download over a slow link is
			// legitimately long. Progress and cancellation are the liveness
			// signals instead.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
	}
}

// HFFile is one file in a repository.
type HFFile struct {
	Name string `json:"rfilename"`
	Size int64  `json:"size,omitempty"`
}

// modelInfo is the subset of the API response this needs.
type modelInfo struct {
	ID       string   `json:"id"`
	Siblings []HFFile `json:"siblings"`
}

// ListFiles asks the API which files a repository contains.
//
// Asking rather than assuming a file list: the set differs between model
// families — distil models omit some files, and a hardcoded list would fail on
// exactly the models a user is most likely to try next.
func (c *HFClient) ListFiles(ctx context.Context, repo string) ([]HFFile, error) {
	url := fmt.Sprintf("%s/api/models/%s", c.BaseURL, repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("models: build request: %w", err)
	}
	c.authorise(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models: reach %s: %w", c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("models: %s does not exist at %s", repo, c.BaseURL)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: %s returned HTTP %d", url, resp.StatusCode)
	}

	var info modelInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("models: decode the file list for %s: %w", repo, err)
	}
	if len(info.Siblings) == 0 {
		return nil, fmt.Errorf("models: %s has no files", repo)
	}

	files := info.Siblings
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// DownloadFile fetches one file, resuming from whatever is already written.
//
// Resumption matters on exactly two occasions — a dropped connection during a
// 3 GB download, and a laptop closed mid-download — and both are infuriating
// without it.
func (c *HFClient) DownloadFile(
	ctx context.Context,
	repo, file, destination string,
	onProgress func(written, total int64),
) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return 0, fmt.Errorf("models: create %s: %w", filepath.Dir(destination), err)
	}

	// A .partial file rather than the destination itself: a half-written
	// model.bin that looks complete is worse than no file at all, because the
	// loader's failure is much further from the cause.
	partial := destination + ".partial"

	var offset int64
	if info, err := os.Stat(partial); err == nil {
		offset = info.Size()
	}

	url := fmt.Sprintf("%s/%s/resolve/main/%s", c.BaseURL, repo, file)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("models: build request: %w", err)
	}
	c.authorise(req)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("models: download %s: %w", file, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// The server ignored the range, so start over rather than appending to
		// the wrong offset and producing a corrupt file.
		offset = 0
	case http.StatusPartialContent:
		// Resuming as intended.
	case http.StatusNotFound:
		return 0, fmt.Errorf("models: %s/%s does not exist", repo, file)
	default:
		return 0, fmt.Errorf("models: %s/%s returned HTTP %d", repo, file, resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	out, err := os.OpenFile(partial, flags, 0o644)
	if err != nil {
		return 0, fmt.Errorf("models: open %s: %w", partial, err)
	}
	defer func() { _ = out.Close() }()

	total := resp.ContentLength
	if total > 0 {
		total += offset
	} else {
		total = -1 // unknown
	}

	written := offset
	buf := make([]byte, 1<<20)

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				return written, fmt.Errorf("models: write to %s: %w", partial, err)
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return written, fmt.Errorf("models: read %s: %w", file, readErr)
		}
	}

	if err := out.Sync(); err != nil {
		return written, fmt.Errorf("models: sync %s: %w", partial, err)
	}
	if err := out.Close(); err != nil {
		return written, fmt.Errorf("models: close %s: %w", partial, err)
	}

	// Renamed only once complete, so a model directory is either whole or
	// absent — never half-populated in a way that looks usable.
	if err := os.Rename(partial, destination); err != nil {
		return written, fmt.Errorf("models: publish %s: %w", destination, err)
	}
	return written, nil
}

func (c *HFClient) authorise(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	// The API rejects requests without a user agent on some endpoints, and the
	// default Go agent identifies nothing useful in a hub operator's logs.
	req.Header.Set("User-Agent", "NikuCooker")
}
