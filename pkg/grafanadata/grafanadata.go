package grafanadata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var _ GrafanaClient = (*Client)(nil)
var _ Logger = (*slog.Logger)(nil)

// Logger interface defines the logging methods used by the Grafana client.
// Compatible with the standard library's slog package.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Debug(msg string, args ...any)
}

type timeRange struct {
	Start time.Time
	End   time.Time
}

// PanelOption defines options for retrieving panel data.
type PanelOption func(*panelOptions)

type panelOptions struct {
	timerange timeRange
	variables map[string]string
}

// WithTimeRange sets the time range for the panel data query.
func WithTimeRange(start, end time.Time) func(*panelOptions) {
	return func(o *panelOptions) {
		o.timerange = timeRange{
			Start: start,
			End:   end,
		}
	}
}

// WithVariables sets the variables for the panel query.
func WithVariables(vars map[string]string) func(*panelOptions) {
	return func(o *panelOptions) {
		o.variables = vars
	}
}

// GrafanaClient interface defines the methods that our Client will implement.
type GrafanaClient interface {
	NewRequest(method, endpoint string, body io.Reader) (*http.Request, error)
	Do(req *http.Request) (*http.Response, error)
	GetDashboard(uid string) (DashboardResponse, error)
	GetPanelDataFromID(uid string, panelID int, opts ...PanelOption) (Results, error)
	FetchDashboards() ([]DashboardSearch, error)
	FetchPanelsFromDashboard(dashboard DashboardResponse) []PanelSearch
	GetHost() string
}

// HTTPClient needed for unit tests
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// ClientOption defines a function that modifies the Grafana Client.
type ClientOption func(*Client)

// WithHTTPClient allows setting a custom HTTP client for the Grafana Client.
func WithHTTPClient(c HTTPClient) ClientOption {
	return func(client *Client) {
		client.client = c
	}
}

// WithToken allows setting a custom API token for the Grafana Client.
func WithToken(token string) ClientOption {
	return func(client *Client) {
		client.token = token
	}
}

// Client represents a Grafana client that can interact with the Grafana API.
type Client struct {
	baseURL *url.URL
	token   string
	client  HTTPClient
	log     Logger
}

// NewGrafanaClient creates a new Grafana Client with an API token and returns the GrafanaClient interface
func NewGrafanaClient(urlstr string, token string, opts ...ClientOption) (*Client, error) {
	parsed, err := url.Parse(urlstr)
	if err != nil {
		return nil, fmt.Errorf("failed to create GrafanaClient. %v", err)
	}

	client := Client{
		baseURL: parsed,
		token:   token,
		client:  &http.Client{},
		log:     slog.Default(),
	}

	for _, opt := range opts {
		opt(&client)
	}

	return &client, nil
}

func (c *Client) getDashboard(uid string) (DashboardResponse, error) {
	var response DashboardResponse

	host := strings.TrimSuffix(c.baseURL.String(), "/")
	query := fmt.Sprintf("%v/api/dashboards/uid/%v", host, uid)

	c.log.Debug("getting dashboard", "host", host, "query", query)

	req, err := c.NewRequest(http.MethodGet, query, nil)
	if err != nil {
		return response, fmt.Errorf("failed to get dashboard %v with error %w", uid, err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return response, fmt.Errorf("failed to get dashboard %v with error %w", uid, err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return response, fmt.Errorf("could not read response body with error %w", err)
	}

	c.log.Debug("got dashboard response", "status", resp.StatusCode, "body", string(b))

	if resp.StatusCode != http.StatusOK {
		return response, fmt.Errorf("grafana returned status %v; body: %s", resp.StatusCode,
			string(b))
	}

	err = json.Unmarshal(b, &response)
	if err != nil {
		return response, fmt.Errorf("could not unmarshal response %w", err)
	}

	return response, nil
}

// retrieves the data for a panel in a dashboard.
func (c *Client) getPanelData(panelID int, dashboard DashboardResponse, opts ...PanelOption) (Results, error) {
	var (
		result  Results
		options panelOptions
	)

	for _, opt := range opts {
		opt(&options)
	}

	panel := dashboard.GetPanelByID(panelID)
	if panel == nil {
		return result, fmt.Errorf("failed to find panel %v in dashboard %v", panelID, dashboard.Dashboard.ID)
	}

	c.log.Debug("got panel", "id", panelID, "panel", panel)

	var legends map[string]string
	for i := range panel.Targets {
		t := panel.Targets[i].(map[string]any)
		if _, ok := t["datasource"]; !ok {
			c.log.Debug("target has no datasource, using panel datasource", "panelID", panelID, "target", t)
			t["datasource"] = panel.Datasource
		}
		if expr, ok := t["expr"].(string); ok {
			c.log.Debug("applying variables for target", "panelID", panelID,
				"target", t, "expr", expr, "variables", options.variables)
			for k, v := range options.variables {
				expr = strings.ReplaceAll(expr, "$"+k, v)
			}
			t["expr"] = expr
		}
		if legend, ok := t["legendFormat"].(string); ok && legend != "__auto" {
			if ref, ok := t["refId"].(string); ok {
				c.log.Debug("adding legend for target", "panelID", panelID, "target", t, "legend", legend)
				legends[ref] = legend
			} else {
				c.log.Warn("target has no refId, cannot set legend", "panelID", panelID,
					"target", t, "legend", legend)
			}
		}
	}

	request := GrafanaDataQueryRequest{
		Queries: panel.Targets,
	}

	if !options.timerange.Start.IsZero() {
		c.log.Debug("setting start time for query", "start", options.timerange.Start)
		request.From = strconv.FormatInt(options.timerange.Start.Unix()*int64(1000), 10)
	}

	if !options.timerange.End.IsZero() {
		c.log.Debug("setting end time for query", "end", options.timerange.End)
		request.To = strconv.FormatInt(options.timerange.End.Unix()*int64(1000), 10)
	}

	b, err := json.Marshal(&request)
	if err != nil {
		return result, fmt.Errorf("failed to build request object: %w", err)
	}

	host := strings.TrimSuffix(c.baseURL.String(), "/")
	query := fmt.Sprintf("%v/api/ds/query", host)
	req, err := c.NewRequest(http.MethodPost, query, bytes.NewBuffer(b))
	if err != nil {
		return result, fmt.Errorf("failed to build request %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return result, fmt.Errorf("failed to read response body with error %w", err)
	}

	c.log.Debug("got panel data response", "status", resp.StatusCode, "body", string(b))

	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("grafana returned status %v; body: %s", resp.StatusCode, string(b))
	}

	err = json.Unmarshal(b, &result)

	result.Legends = legends
	result.c = c

	return result, err
}

// GetDashboard retrieves a dashboard object from a uid
func (c *Client) GetDashboard(uid string) (DashboardResponse, error) {
	return c.getDashboard(uid)
}

// GetPanelDataFromID retrieves the panel data from an id
func (c *Client) GetPanelDataFromID(uid string, panelID int, opts ...PanelOption) (Results, error) {
	var result Results

	dashboard, err := c.getDashboard(uid)
	if err != nil {
		return result, err
	}

	result, err = c.getPanelData(panelID, dashboard, opts...)

	return result, err
}

// GetPanelDataFromTitle retrieves the panel data from title
func (c *Client) GetPanelDataFromTitle(uid string, title string, opts ...PanelOption) (Results, error) {
	var result Results

	dashboard, err := c.getDashboard(uid)
	if err != nil {
		return result, err
	}

	for i := range dashboard.Dashboard.Panels {
		p := dashboard.Dashboard.Panels[i]
		if p.Title != title {
			continue
		}
		result, err = c.getPanelData(p.ID, dashboard, opts...)

		return result, err
	}

	return result, fmt.Errorf("failed to find panel %v", title)
}

// ExtractArgs returns the uid and panel id from a url
func ExtractArgs(urlStr string) (string, int) {
	parsedUrl, err := url.Parse(urlStr)
	if err != nil {
		return "", 0
	}

	segs := strings.Split(parsedUrl.Path, "/")
	var uid string
	if len(segs) >= 3 {
		uid = segs[2]
	} else {
		return "", 0
	}

	viewPanel := parsedUrl.Query().Get("viewPanel")
	if viewPanel == "" {
		return "", 0
	}

	id, err := strconv.ParseInt(viewPanel, 0, 0)
	if err != nil {
		return "", 0
	}

	return uid, int(id)
}

// GetHost returns the base URL of the Grafana client without trailing slash.
func (c *Client) GetHost() string {
	return strings.TrimSuffix(c.baseURL.String(), "/")
}
