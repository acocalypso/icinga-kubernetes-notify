package notify

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Client talks to the Icinga Notifications HTTP API as a source.
type Client struct {
	base     string
	user     string
	password string
	webURL   string
	http     *http.Client
}

func NewClient(c NotificationsConfig) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("notifications.ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("notifications.ca_file: no certificates found")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		base:     strings.TrimRight(c.URL, "/"),
		user:     c.Username,
		password: c.Password,
		webURL:   strings.TrimRight(c.IcingaWebURL, "/"),
		http:     &http.Client{Timeout: c.Timeout, Transport: tr},
	}, nil
}

// processEvent is the request body of POST /v1/process-event.
type processEvent struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	URL               string            `json:"url,omitempty"`
	Tags              map[string]string `json:"tags"`
	Incident          bool              `json:"incident"`
	Severity          string            `json:"severity"`
	Close             bool              `json:"close,omitempty"`
	Summary           string            `json:"summary"`
	Message           string            `json:"message,omitempty"`
	CompleteRelations []string          `json:"complete_relations"`
	Relations         map[string]any    `json:"relations"`
}

func (c *Client) buildEvent(e Event) processEvent {
	pe := processEvent{
		ID:       e.ID,
		Name:     e.Key.String(),
		Tags:     e.Key.Tags(),
		Incident: true,
		Severity: e.Severity,
		Summary:  e.Summary,
		Message:  e.Message,
		// "object" is what event rule filters match on, e.g. object.namespace.
		CompleteRelations: []string{"object"},
		Relations: map[string]any{"object": map[string]string{
			"cluster":   e.Key.Cluster,
			"kind":      e.Key.Kind,
			"namespace": e.Key.Namespace,
			"name":      e.Key.Name,
		}},
	}
	if e.Severity == SevNone {
		pe.Severity = "ok"
		pe.Close = true
	}
	if c.webURL != "" {
		if info, ok := kinds[e.Key.Kind]; ok && e.UUID != "" {
			pe.URL = c.webURL + "/kubernetes/" + info.webPath + "?id=" + url.QueryEscape(e.UUID)
		} else {
			pe.URL = c.webURL + "/kubernetes/dashboard"
		}
	}
	return pe
}

// Submit sends one event. A nil error means Icinga Notifications accepted it.
func (c *Client) Submit(ctx context.Context, e Event) error {
	body, err := json.Marshal(c.buildEvent(e))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/process-event", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("process-event: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}

// OpenIncident is an incident of this source that is currently open.
type OpenIncident struct {
	Key      Key
	Severity string
}

// OpenIncidents lists the open incidents of this source, used to resume after a restart.
func (c *Client) OpenIncidents(ctx context.Context) ([]OpenIncident, error) {
	filter, _ := json.Marshal(map[string]string{IntegrationTag: IntegrationTagValue})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/incidents?filter="+url.QueryEscape(string(filter)), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// The daemon answers 202 and streams one JSON object per incident.
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("incidents: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out []OpenIncident
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row struct {
			Status string `json:"status"`
			Result struct {
				Error      string            `json:"error"`
				ObjectTags map[string]string `json:"object_tags"`
				Severity   string            `json:"severity"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, fmt.Errorf("incidents: decode: %w", err)
		}
		if row.Status != "success" {
			return nil, fmt.Errorf("incidents: %s", row.Result.Error)
		}
		if k, ok := KeyFromTags(row.Result.ObjectTags); ok {
			out = append(out, OpenIncident{Key: k, Severity: row.Result.Severity})
		}
	}
	return out, sc.Err()
}
