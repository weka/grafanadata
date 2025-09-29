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

func (o *panelOptions) applyVariables(s string) string {
	for k, v := range o.variables {
		s = strings.ReplaceAll(s, "$"+k, v)
	}

	return s
}

func newPanelOptions(opts ...PanelOption) panelOptions {
	var options panelOptions
	for _, opt := range opts {
		opt(&options)
	}
	return options
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
	GetDashboardVariables(response DashboardResponse, opts ...PanelOption) (map[string][]string, error)
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

// WithLogger allows setting a custom logger for the Grafana Client.
func WithLogger(logger Logger) ClientOption {
	return func(client *Client) {
		client.log = logger
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
func NewGrafanaClient(urlstr string, opts ...ClientOption) (*Client, error) {
	parsed, err := url.Parse(urlstr)
	if err != nil {
		return nil, fmt.Errorf("failed to create GrafanaClient. %v", err)
	}

	client := Client{
		baseURL: parsed,
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
	var result Results

	options := newPanelOptions(opts...)

	datasource, err := c.getDefaultDatasource()
	if err != nil {
		return result, fmt.Errorf("failed to get default datasource: %w", err)
	}

	panel := dashboard.GetPanelByID(panelID)
	if panel == nil {
		return result, fmt.Errorf("failed to find panel %v in dashboard %v", panelID, dashboard.Dashboard.ID)
	}

	c.log.Debug("got panel", "id", panelID, "panel", panel)

	legends := map[string]string{}
	for i := range panel.Targets {
		t := panel.Targets[i].(map[string]any)
		if _, ok := t["datasource"]; !ok {
			// if the target has no datasource, use the panel's datasource
			if panel.Datasource.UID == "" {
				c.log.Debug("panel has no datasource, using default datasource", "panelID", panelID, "panel", panel)
				panel.Datasource = datasource
			}
			c.log.Debug("target has no datasource, using panel datasource", "panelID", panelID, "target", t)
			t["datasource"] = panel.Datasource
		}
		if expr, ok := t["expr"].(string); ok {
			c.log.Debug("applying variables for target", "panelID", panelID,
				"target", t, "expr", expr, "variables", options.variables)
			t["expr"] = options.applyVariables(expr)
		}
		if legend, ok := t["legendFormat"].(string); ok && legend != "__auto" {
			if ref, ok := t["refId"].(string); ok {
				c.log.Debug("adding legend for target", "panelID", panelID, "target", t, "legend", legend)
				legends[ref] = options.applyVariables(legend)
			} else {
				c.log.Warn("target has no refId, cannot set legend", "panelID", panelID,
					"target", t, "legend", legend)
			}
		}
	}

	request := GrafanaDataQueryRequest{
		Queries: panel.Targets,
	}

	if options.timerange.Start.IsZero() {
		// use the dashboard's time range if not set
		c.log.Debug("using dashboard time range for query", "dashboardID", dashboard.Dashboard.ID)
		request.From = dashboard.Dashboard.Time.From
	} else {
		c.log.Debug("setting start time for query", "start", options.timerange.Start)
		request.From = strconv.FormatInt(options.timerange.Start.Unix()*int64(1000), 10)
	}

	if options.timerange.End.IsZero() {
		request.To = "now"
	} else {
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

func (c *Client) GetDashboardVariables(response DashboardResponse, opts ...PanelOption) (map[string][]string, error) {
	var result = make(map[string][]string)

	datasource, err := c.getDefaultDatasource()
	if err != nil {
		return nil, fmt.Errorf("failed to get default datasource: %w", err)
	}

	options := newPanelOptions(opts...)

	for _, tpl := range response.Dashboard.Templating.List {
		if tpl.Type != "query" {
			continue
		}

		queryMap, ok := tpl.Query.(map[string]any)
		if !ok {
			c.log.Warn("failed to convert query to map", "tpl", tpl)
			continue
		}

		query, ok := queryMap["query"].(string)
		if !ok {
			c.log.Warn("failed to get query", "queryMap", queryMap)
			continue
		}
		if tpl.Datasource.UID == "" {
			c.log.Debug("template has no datasource, using default datasource", "template", tpl)
			tpl.Datasource = datasource
		}
		if strings.HasPrefix(query, "label_values(") {
			// Handle label_values queries by calling Grafana's API
			values, err := c.getLabelValues(tpl.Datasource.UID, query, options)
			if err != nil {
				return nil, fmt.Errorf("failed to get label values for variable %s: %w", tpl.Name, err)
			}
			result[tpl.Name] = values
			// for each value add new variable so that it can be used in queries, if not set
			if options.variables[tpl.Name] == "" {
				options.variables[tpl.Name] = strings.Join(values, "|")
			}
		} else {
			// For other query types, you might want to handle them differently
			c.log.Warn("unhandled query type", "tpl", tpl)
		}
	}

	return result, nil
}

// getLabelValues queries Grafana's label values API for label_values() queries
func (c *Client) getLabelValues(ds, query string, options panelOptions) ([]string, error) {
	// Extract metric and label from label_values(metric, label) format
	query = strings.TrimPrefix(query, "label_values(")
	query = strings.TrimSuffix(query, ")")

	parts := strings.Split(query, ",")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid label_values query format: %s", query)
	}
	if len(parts) != 2 {
		// Handle case where metric contains commas
		// Join all but the last part as the metric
		parts = []string{strings.Join(parts[:len(parts)-1], ","), parts[len(parts)-1]}
	}

	metric := options.applyVariables(strings.TrimSpace(parts[0]))
	label := strings.TrimSpace(parts[1])

	host := strings.TrimSuffix(c.baseURL.String(), "/")
	endpoint := fmt.Sprintf("%s/api/datasources/uid/%s/resources/"+
		"api/v1/label/%s/values?match[]=%s&start=%d",
		host, ds, label, url.QueryEscape(metric), options.timerange.Start.Unix())
	if !options.timerange.End.IsZero() {
		endpoint += fmt.Sprintf("&end=%d", options.timerange.End.Unix())
	}

	c.log.Debug("getting label values", "endpoint", endpoint, "metric", metric, "label", label)

	req, err := c.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grafana returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var labelResponse struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}

	if err := json.Unmarshal(body, &labelResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if labelResponse.Status != "success" {
		return nil, fmt.Errorf("grafana API returned status: %s", labelResponse.Status)
	}

	return labelResponse.Data, nil
}

func (c *Client) getDefaultDatasource() (Datasource, error) {
	var datasource Datasource

	// fetch default datasource using api
	host := strings.TrimSuffix(c.baseURL.String(), "/")
	query := fmt.Sprintf("%v/api/datasources", host)

	c.log.Debug("getting default datasource", "host", host, "query", query)

	req, err := c.NewRequest(http.MethodGet, query, nil)
	if err != nil {
		return datasource, fmt.Errorf("failed to get datasources with error %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return datasource, fmt.Errorf("failed to get datasources with error %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return datasource, fmt.Errorf("could not read response body with error %w", err)
	}

	c.log.Debug("got datasources response", "status", resp.StatusCode, "body", string(b))

	if resp.StatusCode != http.StatusOK {
		return datasource, fmt.Errorf("grafana returned status %v; body: %s", resp.StatusCode, string(b))
	}

	var datasources []Datasource
	err = json.Unmarshal(b, &datasources)
	if err != nil {
		return datasource, fmt.Errorf("could not unmarshal response %w", err)
	}

	// Find the default datasource
	for _, ds := range datasources {
		if ds.IsDefault {
			return ds, nil
		}
	}

	return datasource, nil
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
