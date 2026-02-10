package grafanadata

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type MockHTTPClient struct {
	DoFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return m.DoFunc(req)
}

func CreateMockClient(t *testing.T, file string, expectedCode int) *MockHTTPClient {
	path := fmt.Sprintf("./test/%v", file)
	return &MockHTTPClient{
		DoFunc: func(req *http.Request) (*http.Response, error) {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: expectedCode,
				Body:       io.NopCloser(file),
			}, nil
		},
	}
}

func CreateMockGrafanaClient(t *testing.T, mockClient *MockHTTPClient) *Client {
	return &Client{
		baseURL: &url.URL{
			Scheme: "http",
			Host:   "example.com",
		},
		token:  "test_token",
		client: mockClient,
		log:    slog.Default(),
	}

}

func TestCreateGrafanaClient(t *testing.T) {
	_, err := NewGrafanaClient("foo", WithToken("bar"))
	if err != nil {
		t.Fatalf("creating new grafana Client error %v", err)
	}
}

func TestGetDashboard(t *testing.T) {
	client := CreateMockClient(t, "dashboard.json", http.StatusOK)

	g := CreateMockGrafanaClient(t, client)

	dashboard, err := g.getDashboard("foo")
	if err != nil {
		t.Fatal(err)
	}

	panels := len(dashboard.Dashboard.Panels)
	if panels != 2 {
		t.Fatalf("wanted 2 panels. got %v", panels)
	}

	client = CreateMockClient(t, "dashboard.json", http.StatusNotFound)

	g = CreateMockGrafanaClient(t, client)
	_, err = g.getDashboard("foo")
	if err == nil {
		t.Fatal("wanted error but was nil")
	}
}

func TestGetPanelData(t *testing.T) {
	// loading in the dashboard
	client := CreateMockClient(t, "dashboard.json", http.StatusOK)

	g := CreateMockGrafanaClient(t, client)

	dashboard, err := g.getDashboard("foo")
	if err != nil {
		t.Fatal(err)
	}

	// loading in the panel
	client = CreateMockClient(t, "data.json", http.StatusOK)

	g = CreateMockGrafanaClient(t, client)

	data, err := g.getPanelData(2, dashboard, WithTimeRange(time.Now(), time.Time{}))
	if err != nil {
		t.Fatal(err)
	}

	lenRes := len(data.Results)
	if lenRes != 2 {
		t.Fatalf("wanted len 2 but was %v", lenRes)
	}

}

func TestExtractArgs(t *testing.T) {
	u := "https://grafana.com/d/foobar/fizz?viewPanel=4"
	uid, id := ExtractArgs(u)
	if uid != "foobar" {
		t.Fatalf("wanted foobar. was %v", uid)
	}

	if id != 4 {
		t.Fatalf("wanted 4. was %v", id)
	}

}

func TestGetDashboards(t *testing.T) {
	client := CreateMockClient(t, "search.json", http.StatusOK)

	g := CreateMockGrafanaClient(t, client)

	dashboards, err := g.FetchDashboards()
	if err != nil {
		t.Fatal(err)
	}

	count := len(dashboards)
	if count != 2 {
		t.Fatalf("expected 2 dashboards. got %v", count)
	}

	uid := "bebca380-068d-463d-9c9c-1bb19cb8d2b3"
	if dashboards[0].UID != uid {
		t.Fatalf("wanted %v. was %v", uid, dashboards[0].UID)
	}
	title := "New dashboard"
	if dashboards[0].Title != title {
		t.Fatalf("wanted %v. was %v", uid, dashboards[0].Title)
	}

	client = CreateMockClient(t, "dashboard.json", http.StatusNotFound)

	g = CreateMockGrafanaClient(t, client)
	_, err = g.getDashboard("foo")
	if err == nil {
		t.Fatal("wanted error but was nil")
	}
}

func TestVariousGrafanaUrls(t *testing.T) {
	urls := []string{
		"http://grafana:3000",
		"http://grafana:3000/",
		"http://grafana/",
		"http://grafana",
		"https://host/grafana/",
		"https://host/grafana",
	}

	for _, url := range urls {
		client, err := NewGrafanaClient(url, WithToken("foo"))
		if err != nil {
			t.Fatalf("could not created GrafanaClient from %v: %v", url, err)
		}

		host := client.GetHost()
		if host != strings.TrimSuffix(url, "/") {
			t.Errorf("expected %v. got %v", url, host)
		}
	}
}

func TestParseIntervalMs(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"1m", 60000},
		{"5m", 300000},
		{"2m", 120000},
		{"30s", 30000},
		{"1h", 3600000},
		{"1d", 86400000},
		{"", 0},                           // empty → no interval configured, return 0
		{"invalid", defaultIntervalMs},    // non-empty but unparseable → fallback default
		{"m", defaultIntervalMs},          // too short, non-empty → fallback default
		{"0m", 0},                         // zero value parses correctly to 0ms
		{"3x", defaultIntervalMs},         // unknown unit → fallback default
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := parseIntervalMs(tt.input)
			if result != tt.expected {
				t.Errorf("parseIntervalMs(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestGetPanelDataInjectsMaxDataPoints(t *testing.T) {
	// loading in the dashboard
	client := CreateMockClient(t, "dashboard.json", http.StatusOK)
	g := CreateMockGrafanaClient(t, client)

	dashboard, err := g.getDashboard("foo")
	if err != nil {
		t.Fatal(err)
	}

	// Set panel interval to verify it gets parsed
	dashboard.Dashboard.Panels[1].Interval = "2m"

	// Use a mock that captures the request body
	var capturedBody []byte
	g.client = &MockHTTPClient{
		DoFunc: func(req *http.Request) (*http.Response, error) {
			capturedBody, _ = io.ReadAll(req.Body)

			file, err := os.Open("./test/data.json")
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(file),
			}, nil
		},
	}

	_, err = g.getPanelData(2, dashboard, WithTimeRange(time.Now(), time.Time{}))
	if err != nil {
		t.Fatal(err)
	}

	// Verify that maxDataPoints and intervalMs were injected
	bodyStr := string(capturedBody)
	if !strings.Contains(bodyStr, "\"maxDataPoints\"") {
		t.Error("expected maxDataPoints to be injected into query targets")
	}
	if !strings.Contains(bodyStr, "\"intervalMs\"") {
		t.Error("expected intervalMs to be injected into query targets")
	}
}
