package main

import (
	"bytes"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCommandResponsesPreservePageContext(t *testing.T) {
	for _, accept := range []string{"text/html", "application/json"} {
		t.Run(accept, func(t *testing.T) {
			calls := 0
			app := &App{
				csrfToken: "csrf", cacheTTL: time.Minute, devicesAt: time.Now(),
				devices:  []Device{{DeviceID: "light", DeviceType: "Ceiling Light"}},
				statuses: map[string]CachedStatus{"light": {Status: map[string]any{"brightness": 80}}},
				client: &SwitchBotClient{HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var command CommandRequest
					if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
						t.Fatal(err)
					}
					if r.Method != "POST" || r.URL.Path != "/v1.1/devices/light/commands" || command.Command != "setBrightness" || command.Parameter != "37" {
						t.Fatalf("unexpected command: %s %#v", r.URL, command)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"statusCode":100,"message":"success"}`)), Header: make(http.Header)}, nil
				})}},
			}
			form := url.Values{"csrf": {"csrf"}, "action": {"brightness"}, "parameter": {"37"}, "q": {"Desk & Light"}}
			r := httptest.NewRequest(http.MethodPost, "/devices/light/command", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", accept)
			w := httptest.NewRecorder()
			app.handleCommand(w, r, "light")
			if accept == "application/json" {
				if w.Code != 200 || w.Header().Get("Location") != "" || !json.Valid(w.Body.Bytes()) {
					t.Fatalf("JSON command navigates: %d %s", w.Code, w.Body.String())
				}
			} else {
				back, err := url.Parse(w.Header().Get("Location"))
				if err != nil || w.Code != 303 || back.Path != "/" || back.Query().Get("q") != "Desk & Light" || back.Fragment != "device-light" {
					t.Fatalf("lost context: %d %s", w.Code, w.Header().Get("Location"))
				}
			}
			if calls != 1 {
				t.Fatalf("command sent %d times", calls)
			}
			if _, ok := app.statuses["light"]; ok {
				t.Fatal("old status not invalidated")
			}
		})
	}
}

func TestRefreshAndDetailsPreserveSearch(t *testing.T) {
	query := "Desk & Light"
	app := &App{
		csrfToken: "csrf", cacheTTL: time.Minute, statusTTL: time.Minute, devicesAt: time.Now(),
		devices:  []Device{{DeviceID: "light", DeviceType: "Ceiling Light"}},
		statuses: map[string]CachedStatus{"light": {FetchedAt: time.Now(), Status: map[string]any{"brightness": 37}}},
		client: &SwitchBotClient{HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"statusCode":100,"body":{"deviceList":[{"deviceId":"light","deviceType":"Ceiling Light"}]}}`)), Header: make(http.Header)}, nil
		})}},
	}
	for _, accept := range []string{"text/html", "application/json"} {
		form := url.Values{"csrf": {"csrf"}, "q": {query}}
		r := httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", accept)
		w := httptest.NewRecorder()
		app.handleRefresh(w, r)
		if accept == "text/html" {
			back, _ := url.Parse(w.Header().Get("Location"))
			if w.Code != 303 || back.Query().Get("q") != query {
				t.Fatal("refresh lost query")
			}
		} else if w.Code != 200 || w.Header().Get("Location") != "" || !json.Valid(w.Body.Bytes()) {
			t.Fatal("JSON refresh navigates")
		}
	}
	w := httptest.NewRecorder()
	app.handleStatus(w, httptest.NewRequest(http.MethodGet, detailsURL("light", query), nil), "light")
	for _, want := range []string{`href="/?q=Desk+%26+Light#device-light"`, `href="/devices/light/status?q=Desk+%26+Light&refresh=1"`} {
		if !strings.Contains(html.UnescapeString(w.Body.String()), want) {
			t.Errorf("detail page missing context: %s", want)
		}
	}
}

func TestCommandErrorsDoNotRedirectOrDiscardCachedState(t *testing.T) {
	for _, tc := range []struct {
		name, csrf, action, parameter string
		status                        int
	}{
		{"csrf", "invalid", "brightness", "37", http.StatusForbidden},
		{"action", "csrf", "invalid", "37", http.StatusBadRequest},
		{"parameter", "csrf", "brightness", "bad", http.StatusBadRequest},
		{"upstream", "csrf", "brightness", "37", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{
				csrfToken: "csrf", cacheTTL: time.Minute, devicesAt: time.Now(), devices: []Device{{DeviceID: "light", DeviceType: "Ceiling Light"}},
				statuses: map[string]CachedStatus{"light": {Status: map[string]any{"brightness": 80}}},
				client: &SwitchBotClient{HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					if tc.name != "upstream" {
						t.Fatal("invalid command sent to upstream")
					}
					return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
				})}},
			}
			values := url.Values{"csrf": {tc.csrf}, "action": {tc.action}, "parameter": {tc.parameter}}
			r := httptest.NewRequest(http.MethodPost, "/devices/light/command", strings.NewReader(values.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", "application/json")
			w := httptest.NewRecorder()
			app.handleCommand(w, r, "light")
			if w.Code != tc.status || w.Header().Get("Location") != "" || w.Body.Len() == 0 {
				t.Fatalf("error response: %d %s", w.Code, w.Body.String())
			}
			if _, ok := app.statuses["light"]; !ok {
				t.Fatal("failed command discarded cached status")
			}
		})
	}
}

func TestDeviceControlsRendering(t *testing.T) {
	devices := []Device{}
	for _, kind := range []string{"Bot", "Lock Pro", "Curtain", "Blind Tilt", "Color Bulb", "RGBICWW Ceiling Light", "Plug Mini", "Robot Vacuum", "Humidifier", "Meter", "Contact Sensor"} {
		devices = append(devices, Device{DeviceID: kind, DeviceName: kind, DeviceType: kind})
	}
	for _, kind := range []string{"TV", "Speaker", "Fan", "Light"} {
		devices = append(devices, Device{DeviceID: "ir-" + kind, DeviceName: kind, RemoteType: kind, InfraredRemote: true})
	}
	for i := range devices {
		devices[i].Actions = actionsForDevice(devices[i])
	}
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, PageData{Devices: devices}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`type="color"`, `type="range"`, "施錠", "解錠", "音量＋", "首振り", "主灯の明るさ", "ブラインドの角度", `class="details-link"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("controls missing %q", want)
		}
	}
}

func TestColorPickerCommand(t *testing.T) {
	action, ok := findAction(Device{DeviceType: "Color Bulb"}, "rgb")
	if !ok {
		t.Fatal("color action not found")
	}
	for _, value := range []string{"#ffb450", "255:180:80"} {
		req, err := action.CommandRequest(mapValues("parameter", value))
		if err != nil || req.Parameter != "255:180:80" {
			t.Fatalf("color %q: request=%#v, error=%v", value, req, err)
		}
	}
	for _, value := range []string{"#xxxxxx", "#fff", "256:0:0"} {
		if _, err := action.CommandRequest(mapValues("parameter", value)); err == nil {
			t.Errorf("invalid color %q accepted", value)
		}
	}
}

func TestIndexUsesDeviceStatusForControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   string
		status map[string]any
		want   []string
	}{
		{"ceiling", "Ceiling Light", map[string]any{"brightness": 37.0, "colorTemperature": 3500.0}, []string{`<output>37 %</output>`, `value="37" data-unit="%"`, `<output>3500 K</output>`}},
		{"ceiling-rounded", "Ceiling Light Pro", map[string]any{"colorTemperature": 2701.0}, []string{`<output>約2700 K</output>`, `value="2701" data-unit="K"`}},
		{"rgbicww", "RGBICWW Ceiling Light", map[string]any{"mainLightBrightness": 42.0, "mainLightColorTemp": 4000.0, "colorLightBrightness": 18.0}, []string{`<output>42 %</output>`, `<output>4000 K</output>`, `<output>18 %</output>`}},
		{"missing", "Ceiling Light", map[string]any{}, []string{`<output>未取得</output>`, `type="submit" disabled`}},
		{"color", "Color Bulb", map[string]any{"color": "12:34:56"}, []string{`--picked-color:#0c2238`, `type="color" name="parameter" value="#0c2238"`}},
		{"color-black", "Color Bulb", map[string]any{"color": "0:0:0"}, []string{`--picked-color:transparent`, `type="color" name="parameter" value="#000000"`}},
		{"curtain", "Curtain", map[string]any{"slidePosition": "37"}, []string{`<output>37 %</output>`, `step="1" value="37" data-unit="%"`}},
		{"curtain-open", "Curtain", map[string]any{"slidePosition": 0.0}, []string{`<output>0 %</output>`, `step="1" value="0" data-unit="%"`}},
		{"blind", "Blind Tilt", map[string]any{"slidePosition": 42.0, "direction": "down"}, []string{`<output>42 %</output>`, `value="down" selected`, `step="2" value="42"`}},
		{"blind-missing", "Blind Tilt", map[string]any{}, []string{`<output>未取得</output>`, `value="" disabled selected`, `type="submit" disabled`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			app := &App{
				cacheTTL: time.Minute, statusTTL: time.Minute, devicesAt: now,
				devices:  []Device{{DeviceID: "light", DeviceName: "書斎照明", DeviceType: tc.kind}},
				statuses: map[string]CachedStatus{"light": {FetchedAt: now, Status: tc.status}},
			}
			// Reloading must keep using the cached reading, without modifying the device cache.
			for i := 0; i < 2; i++ {
				w := httptest.NewRecorder()
				app.handleIndex(w, httptest.NewRequest(http.MethodGet, "/", nil))
				for _, want := range tc.want {
					if !strings.Contains(w.Body.String(), want) {
						t.Errorf("reload %d: missing %q", i, want)
					}
				}
				if strings.Contains(w.Body.String(), `<output>100 %</output>`) {
					t.Fatal("fixed 100% reading returned")
				}
			}
		})
	}
}

func TestInvalidStatusDoesNotBecomeACommandDefault(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		action string
		status map[string]any
	}{
		{"Curtain", "position", map[string]any{"slidePosition": 101}},
		{"Curtain", "position", map[string]any{"slidePosition": "bad"}},
		{"Color Bulb", "rgb", map[string]any{"color": "256:0:0"}},
		{"Blind Tilt", "position", map[string]any{"slidePosition": 51, "direction": "up"}},
		{"Blind Tilt", "position", map[string]any{"slidePosition": 50, "direction": "unknown"}},
	} {
		action, ok := findAction(Device{DeviceType: tc.kind}, tc.action)
		if !ok {
			t.Fatalf("missing %s action", tc.kind)
		}
		action.loadStatus(tc.status)
		if action.Ready() {
			t.Errorf("%s: invalid status enabled submit: %#v", tc.kind, tc.status)
		}
	}
}

func TestAirConditionerIsUnsupported(t *testing.T) {
	ac := Device{DeviceID: "ac", DeviceName: "エアコン", RemoteType: "Air Conditioner", InfraredRemote: true}
	if len(actionsForDevice(ac)) != 0 {
		t.Fatal("air conditioner still exposes controls")
	}
	if allowedCommand("setAll") {
		t.Fatal("air conditioner command still allowed")
	}
	devices := []Device{ac, {DeviceID: "tv", DeviceName: "テレビ", RemoteType: "TV", InfraredRemote: true}}
	visible := filterDevices(devices, "")
	if len(visible) != 1 || visible[0].DeviceID != "tv" {
		t.Fatalf("unexpected visible devices: %#v", visible)
	}
	if len(filterDevices(devices, "エアコン")) != 0 {
		t.Fatal("search still shows air conditioner")
	}
}

func TestIndexShowsCachedSensorReadings(t *testing.T) {
	now := time.Now()
	app := &App{
		cacheTTL: time.Minute, statusTTL: time.Minute, devicesAt: now,
		devices: []Device{
			{DeviceID: "meter", DeviceName: "Living Meter", DeviceType: "MeterPlus"},
			{DeviceID: "hub", DeviceType: "Hub 2"},
			{DeviceID: "failed", DeviceType: "Meter"},
			{DeviceID: "plug", DeviceType: "Plug Mini"},
		},
		statuses: map[string]CachedStatus{
			"meter":  {FetchedAt: now, Status: map[string]any{"temperature": 0.0, "humidity": 0.0}},
			"hub":    {FetchedAt: now, Status: map[string]any{"temperature": 23.5}},
			"failed": {FetchedAt: now, Error: "upstream unavailable"},
		},
	}
	w := httptest.NewRecorder()
	app.handleIndex(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"0 °C", "0 %", "23.5 °C", "温湿度を取得できませんでした", `id="device-plug"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if got := strings.Count(body, `class="readings"`); got != 3 {
		t.Errorf("sensor cards = %d, want 3", got)
	}
	// Page-specific readings must not be written into the device-list cache.
	if app.devices[0].Sensor || app.devices[0].Temperature != "" {
		t.Fatal("readings leaked into device cache")
	}
}

func TestAllowedCommand(t *testing.T) {
	for _, cmd := range []string{"turnOn", "turnOff", "press", "lock", "unlock", "setPosition", "pause"} {
		if !allowedCommand(cmd) {
			t.Fatalf("%s should be allowed", cmd)
		}
	}
	if allowedCommand("open") {
		t.Fatal("unexpectedly allowed generic open command")
	}
	if allowedCommand("deleteDevice") {
		t.Fatal("unexpectedly allowed broad command")
	}
}

func TestSignAddsRequiredHeaders(t *testing.T) {
	client := &SwitchBotClient{Token: "token", Secret: "secret"}
	req, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	client.sign(req)
	for _, h := range []string{"Authorization", "sign", "t", "nonce"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("%s header is empty", h)
		}
	}
	if got := req.Header.Get("Authorization"); got != "token" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestFilterDevices(t *testing.T) {
	devices := []Device{
		{DeviceID: "a", DeviceName: "Desk Light", DeviceType: "Color Bulb"},
		{DeviceID: "b", DeviceName: "Door", DeviceType: "Lock"},
	}
	got := filterDevices(devices, "light")
	if len(got) != 1 || got[0].DeviceID != "a" {
		t.Fatalf("unexpected filter result: %#v", got)
	}
}

func TestActionsForDeviceCurtain(t *testing.T) {
	device := Device{DeviceID: "curtain", DeviceType: "Curtain"}
	action, ok := findAction(device, "position")
	if !ok {
		t.Fatal("position action not found")
	}
	req, err := action.CommandRequest(mapValues("position", "75"))
	if err != nil {
		t.Fatal(err)
	}
	if req.Command != "setPosition" || req.Parameter != "0,ff,75" {
		t.Fatalf("unexpected request: %#v", req)
	}
}

func TestActionsForDeviceLock(t *testing.T) {
	device := Device{DeviceID: "lock", DeviceType: "Lock Pro"}
	if _, ok := findAction(device, "press"); ok {
		t.Fatal("lock should not expose press")
	}
	action, ok := findAction(device, "unlock")
	if !ok {
		t.Fatal("unlock action not found")
	}
	req, err := action.CommandRequest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Command != "unlock" {
		t.Fatalf("unexpected command: %#v", req)
	}
}

func TestBlindPositionRequiresEvenNumber(t *testing.T) {
	device := Device{DeviceType: "Blind Tilt"}
	action, ok := findAction(device, "position")
	if !ok {
		t.Fatal("position action not found")
	}
	if _, err := action.CommandRequest(mapValues("direction", "up", "position", "51")); err == nil {
		t.Fatal("expected odd position to fail")
	}
}

func mapValues(values ...string) map[string][]string {
	out := map[string][]string{}
	for i := 0; i+1 < len(values); i += 2 {
		out[values[i]] = []string{values[i+1]}
	}
	return out
}

func TestLoadEnvFile(t *testing.T) {
	t.Setenv("SWITCHBOT_TOKEN", "")
	t.Setenv("SWITCHBOT_SECRET", "")
	path := filepath.Join(t.TempDir(), "switchbot-panel.env")
	content := "SWITCHBOT_TOKEN='abc'\nSWITCHBOT_SECRET=\"def\"\n# comment\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SWITCHBOT_TOKEN"); got != "abc" {
		t.Fatalf("SWITCHBOT_TOKEN = %q", got)
	}
	if got := os.Getenv("SWITCHBOT_SECRET"); got != "def" {
		t.Fatalf("SWITCHBOT_SECRET = %q", got)
	}
}

func TestLoadEnvFileDoesNotOverrideExistingEnv(t *testing.T) {
	t.Setenv("SWITCHBOT_TOKEN", "existing")
	path := filepath.Join(t.TempDir(), "switchbot-panel.env")
	if err := os.WriteFile(path, []byte("SWITCHBOT_TOKEN=file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SWITCHBOT_TOKEN"); got != "existing" {
		t.Fatalf("SWITCHBOT_TOKEN = %q", got)
	}
}
