package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultAddr = "0.0.0.0:8080"
const switchBotHost = "https://api.switch-bot.com"

func main() {
	loadEnvFiles("switchbot-panel.env", ".env")

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(2)
	}

	client := &SwitchBotClient{
		Token:      cfg.Token,
		Secret:     cfg.Secret,
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
	}
	app := &App{
		client:     client,
		password:   cfg.Password,
		cacheTTL:   cfg.CacheTTL,
		statusTTL:  cfg.StatusTTL,
		csrfToken:  randomNonce(),
		startedAt:  time.Now(),
		httpClient: client.HTTPClient,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.withAuth(app.handleIndex))
	mux.HandleFunc("GET /panel.js", app.withAuth(app.handleScript))
	mux.HandleFunc("/refresh", app.withAuth(app.handleRefresh))
	mux.HandleFunc("/devices/", app.withAuth(app.handleDevice))
	mux.HandleFunc("/healthz", app.handleHealth)

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           secureHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("switchbot-panel listening", "addr", cfg.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

//go:embed panel.js
var panelJS string

func (a *App) handleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = io.WriteString(w, panelJS)
}

func indexURL(query, deviceID string) string {
	u := url.URL{Path: "/"}
	if query != "" {
		u.RawQuery = url.Values{"q": {query}}.Encode()
	}
	if deviceID != "" {
		u.Fragment = "device-" + deviceID
	}
	return u.String()
}

func detailsURL(deviceID, query string) string {
	u := url.URL{Path: "/devices/" + deviceID + "/status"}
	if query != "" {
		u.RawQuery = url.Values{"q": {query}}.Encode()
	}
	return u.String()
}

func actionSuccess(w http.ResponseWriter, r *http.Request, back string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "操作を受け付けました"})
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

type Config struct {
	Addr      string
	Token     string
	Secret    string
	Password  string
	CacheTTL  time.Duration
	StatusTTL time.Duration
}

func loadConfig() (Config, error) {
	cfg := Config{
		Addr:      getenv("SWITCHBOT_PANEL_ADDR", defaultAddr),
		Token:     os.Getenv("SWITCHBOT_TOKEN"),
		Secret:    os.Getenv("SWITCHBOT_SECRET"),
		Password:  os.Getenv("SWITCHBOT_PANEL_PASSWORD"),
		CacheTTL:  durationEnv("SWITCHBOT_DEVICE_CACHE_TTL", 10*time.Minute),
		StatusTTL: durationEnv("SWITCHBOT_STATUS_CACHE_TTL", 20*time.Second),
	}
	if cfg.Token == "" {
		return cfg, errors.New("SWITCHBOT_TOKEN is required")
	}
	if cfg.Secret == "" {
		return cfg, errors.New("SWITCHBOT_SECRET is required")
	}
	return cfg, nil
}

func loadEnvFiles(paths ...string) {
	for _, path := range paths {
		if err := loadEnvFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("failed to load env file", "path", path, "error", err)
		}
	}
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, unquoteEnvValue(value))
	}
	return scanner.Err()
}

func unquoteEnvValue(value string) string {
	if len(value) >= 2 {
		if value[0] == '\'' && value[len(value)-1] == '\'' {
			return value[1 : len(value)-1]
		}
		if value[0] == '"' && value[len(value)-1] == '"' {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted
			}
			return value[1 : len(value)-1]
		}
	}
	return value
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env; using fallback", "key", key, "value", v, "fallback", fallback)
		return fallback
	}
	return d
}

func sortDevices(devices []Device) {
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].DeviceType == devices[j].DeviceType {
			return devices[i].DeviceName < devices[j].DeviceName
		}
		return devices[i].DeviceType < devices[j].DeviceType
	})
}

type App struct {
	client     *SwitchBotClient
	password   string
	cacheTTL   time.Duration
	statusTTL  time.Duration
	csrfToken  string
	startedAt  time.Time
	httpClient *http.Client

	mu          sync.Mutex
	devices     []Device
	devicesAt   time.Time
	statuses    map[string]CachedStatus
	lastRefresh time.Time
}

type CachedStatus struct {
	Status    map[string]any
	FetchedAt time.Time
	Error     string
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	devices, err := a.getDevices(r.Context(), false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	devices = filterDevices(devices, "")
	sortDevices(devices)
	filtered := filterDevices(devices, q)
	for i := range filtered {
		filtered[i].Actions = actionsForDevice(filtered[i])
	}
	a.loadDeviceReadings(r.Context(), filtered)
	a.mu.Lock()
	lastRefresh := a.lastRefresh
	a.mu.Unlock()

	data := PageData{
		Query:       q,
		Devices:     filtered,
		AllDevices:  devices,
		Count:       len(filtered),
		Total:       len(devices),
		LastRefresh: lastRefresh,
		CacheTTL:    a.cacheTTL,
		StatusTTL:   a.statusTTL,
		CSRFToken:   a.csrfToken,
		StartedAt:   a.startedAt,
	}
	if err := pageTemplate.Execute(w, data); err != nil {
		slog.Error("render failed", "error", err)
	}
}

func (a *App) loadDeviceReadings(ctx context.Context, devices []Device) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i := range devices {
		d := &devices[i]
		kind := strings.ToLower(d.DeviceType)
		d.Sensor = !d.InfraredRemote && (strings.Contains(kind, "meter") || strings.Contains(kind, "hub 2") || strings.Contains(kind, "hub 3") || strings.Contains(kind, "outdoor thermo"))
		if d.InfraredRemote || (!d.Sensor && !isLightDevice(kind) && !strings.Contains(kind, "curtain") && !strings.Contains(kind, "shade") && !strings.Contains(kind, "blind tilt")) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				if d.Sensor {
					d.SensorError = "温湿度を取得できませんでした"
				}
				return
			}
			cached, err := a.getStatus(ctx, d.DeviceID, false)
			if err != nil || cached.Error != "" {
				if d.Sensor {
					d.SensorError = "温湿度を取得できませんでした"
				}
				return
			}
			for i := range d.Actions {
				d.Actions[i].loadStatus(cached.Status)
			}
			if v, ok := cached.Status["temperature"]; ok && v != nil {
				d.Temperature = fmt.Sprint(v)
			}
			if v, ok := cached.Status["humidity"]; ok && v != nil {
				d.Humidity = fmt.Sprint(v)
			}
		}()
	}
	wg.Wait()
}

func (a *App) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.validCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	_, err := a.getDevices(r.Context(), true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	actionSuccess(w, r, indexURL(r.FormValue("q"), ""))
}

func (a *App) handleDevice(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/devices/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	deviceID := parts[0]
	action := parts[1]

	switch action {
	case "status":
		a.handleStatus(w, r, deviceID)
	case "command":
		a.handleCommand(w, r, deviceID)
	default:
		http.NotFound(w, r)
	}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status, err := a.getStatus(r.Context(), deviceID, r.URL.Query().Get("refresh") == "1")
	device, deviceErr := a.findDevice(r.Context(), deviceID)
	if deviceErr != nil {
		device = Device{DeviceID: deviceID}
	}
	query := r.URL.Query().Get("q")
	refreshURL, _ := url.Parse(detailsURL(deviceID, query))
	refreshQuery := refreshURL.Query()
	refreshQuery.Set("refresh", "1")
	refreshURL.RawQuery = refreshQuery.Encode()
	data := StatusPageData{Device: device, DeviceID: deviceID, Status: status.Status, FetchedAt: status.FetchedAt, Error: status.Error, BackURL: indexURL(query, deviceID), RefreshURL: refreshURL.String()}
	if err != nil {
		data.Error = err.Error()
	}
	if err := statusTemplate.Execute(w, data); err != nil {
		slog.Error("render status failed", "error", err)
	}
}

func (a *App) handleCommand(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.validCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	device, err := a.findDevice(r.Context(), deviceID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	action, ok := findAction(device, strings.TrimSpace(r.FormValue("action")))
	if !ok {
		http.Error(w, "command is not allowed for this device", http.StatusBadRequest)
		return
	}
	command, err := action.CommandRequest(r.Form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := a.client.Command(r.Context(), deviceID, command)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	a.mu.Lock()
	if a.statuses != nil {
		delete(a.statuses, deviceID)
	}
	a.mu.Unlock()

	slog.Info("command sent", "deviceID", deviceID, "command", command.Command, "statusCode", resp.StatusCode, "message", resp.Message)
	actionSuccess(w, r, indexURL(r.FormValue("q"), deviceID))
}

func (a *App) findDevice(ctx context.Context, deviceID string) (Device, error) {
	devices, err := a.getDevices(ctx, false)
	if err != nil {
		return Device{}, err
	}
	for _, device := range devices {
		if device.DeviceID == deviceID {
			return device, nil
		}
	}
	return Device{}, fmt.Errorf("device not found: %s", deviceID)
}

func (a *App) getDevices(ctx context.Context, force bool) ([]Device, error) {
	a.mu.Lock()
	if !force && len(a.devices) > 0 && time.Since(a.devicesAt) < a.cacheTTL {
		devices := append([]Device(nil), a.devices...)
		a.mu.Unlock()
		return devices, nil
	}
	a.mu.Unlock()

	devices, err := a.client.Devices(ctx)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	a.devices = append([]Device(nil), devices...)
	a.devicesAt = time.Now()
	a.lastRefresh = a.devicesAt
	if a.statuses == nil {
		a.statuses = map[string]CachedStatus{}
	}
	a.mu.Unlock()
	return devices, nil
}

func (a *App) getStatus(ctx context.Context, deviceID string, force bool) (CachedStatus, error) {
	a.mu.Lock()
	if a.statuses == nil {
		a.statuses = map[string]CachedStatus{}
	}
	if cached, ok := a.statuses[deviceID]; ok && !force && time.Since(cached.FetchedAt) < a.statusTTL {
		a.mu.Unlock()
		return cached, nil
	}
	a.mu.Unlock()

	status, err := a.client.Status(ctx, deviceID)
	cached := CachedStatus{Status: status, FetchedAt: time.Now()}
	if err != nil {
		cached.Error = err.Error()
	}

	a.mu.Lock()
	a.statuses[deviceID] = cached
	a.mu.Unlock()
	return cached, err
}

func (a *App) validCSRF(r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	got := r.FormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.csrfToken)) == 1
}

func (a *App) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.password == "" {
			next(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="switchbot-panel"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func filterDevices(devices []Device, q string) []Device {
	q = strings.ToLower(q)
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		if strings.EqualFold(strings.TrimSpace(remoteKind(d)), "Air Conditioner") {
			continue
		}
		haystack := strings.ToLower(d.DeviceName + " " + d.DeviceType + " " + d.DeviceID)
		if strings.Contains(haystack, q) {
			out = append(out, d)
		}
	}
	return out
}

func allowedCommand(cmd string) bool {
	return commandCatalog[cmd]
}

var commandCatalog = map[string]bool{
	"turnOn":                  true,
	"turnOff":                 true,
	"toggle":                  true,
	"press":                   true,
	"lock":                    true,
	"unlock":                  true,
	"nightLatchUnlock":        true,
	"pause":                   true,
	"dock":                    true,
	"startClean":              true,
	"setBrightness":           true,
	"setColorTemperature":     true,
	"setColor":                true,
	"setPosition":             true,
	"fullyOpen":               true,
	"closeUp":                 true,
	"closeDown":               true,
	"volumeAdd":               true,
	"volumeSub":               true,
	"channelAdd":              true,
	"channelSub":              true,
	"setMute":                 true,
	"swing":                   true,
	"lowSpeed":                true,
	"middleSpeed":             true,
	"highSpeed":               true,
	"brightnessUp":            true,
	"brightnessDown":          true,
	"turnOnMainLight":         true,
	"turnOffMainLight":        true,
	"turnOnColorLight":        true,
	"turnOffColorLight":       true,
	"setMainLightBrightness":  true,
	"setMainLightColorTemp":   true,
	"setColorLightBrightness": true,
	"setColorLightRGB":        true,
}

type CommandAction struct {
	CurrentValue     string
	CurrentDirection string
	ID               string
	Label            string
	Command          string
	CommandType      string
	ParamKind        string
	ParamValue       string
	Min              int
	Max              int
	Step             int
	Placeholder      string
	Options          []ParamOption
}

type ParamOption struct {
	Value string
	Label string
}

// Presentation stays separate from the API command names.
func (a CommandAction) Title() string {
	titles := map[string]string{
		"press": "押す", "on": "オン", "off": "オフ", "toggle": "切替", "lock": "施錠", "unlock": "解錠",
		"open": "開く", "close": "閉じる", "pause": "停止", "position": "位置を設定", "fully_open": "全開", "close_up": "上向きに閉じる", "close_down": "下向きに閉じる",
		"main_on": "主灯オン", "main_off": "主灯オフ", "color_on": "間接灯オン", "color_off": "間接灯オフ",
		"brightness": "明るさ", "main_brightness": "主灯の明るさ", "color_brightness": "間接灯の明るさ", "temperature": "色温度", "main_temp": "主灯の色温度", "rgb": "光の色",
		"vol_up": "音量＋", "vol_down": "音量−", "ch_up": "CH＋", "ch_down": "CH−", "mute": "消音", "swing": "首振り", "low": "弱", "middle": "中", "high": "強", "dock": "充電台へ", "brightness_up": "明るく", "brightness_down": "暗く",
	}
	if title, ok := titles[a.ID]; ok {
		return title
	}
	return a.Label
}

func (a CommandAction) Icon() string {
	icons := map[string]string{
		"press": "☝", "on": "⏻", "off": "○", "toggle": "⇄", "lock": "🔒", "unlock": "🔓", "open": "↔", "close": "⇥⇤", "pause": "Ⅱ", "position": "↔", "fully_open": "↔", "close_up": "↑", "close_down": "↓", "main_on": "☀", "main_off": "○", "color_on": "◉", "color_off": "○", "brightness": "☀", "main_brightness": "☀", "color_brightness": "☀", "temperature": "◐", "main_temp": "◐", "rgb": "🎨", "vol_up": "🔊", "vol_down": "🔉", "ch_up": "＋", "ch_down": "−", "mute": "🔇", "swing": "↔", "low": "▂", "middle": "▂▄", "high": "▂▄▆", "dock": "⌂", "brightness_up": "☀", "brightness_down": "◐",
	}
	if icon, ok := icons[a.ID]; ok {
		return icon
	}
	return "●"
}

// Keep API status fields and command parameter formats separate.
func (a *CommandAction) loadStatus(status map[string]any) {
	key := map[string]string{
		"setBrightness": "brightness", "setColorTemperature": "colorTemperature",
		"setMainLightBrightness": "mainLightBrightness", "setMainLightColorTemp": "mainLightColorTemp",
		"setColorLightBrightness": "colorLightBrightness", "setColor": "color", "setColorLightRGB": "colorLightRGB",
		"setPosition": "slidePosition",
	}[a.Command]
	if key == "" {
		return
	}
	raw, ok := status[key]
	if !ok || raw == nil {
		return
	}
	value := fmt.Sprint(raw)
	if a.ParamKind == "color" {
		if validRGB(value) {
			parts := strings.Split(value, ":")
			r, _ := strconv.Atoi(parts[0])
			g, _ := strconv.Atoi(parts[1])
			b, _ := strconv.Atoi(parts[2])
			a.CurrentValue = fmt.Sprintf("#%02x%02x%02x", r, g, b)
		}
		return
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < a.Min || n > a.Max {
		return
	}
	if a.ParamKind == "blind_position" {
		if n%2 != 0 {
			return
		}
		direction, _ := status["direction"].(string)
		if direction == "up" || direction == "down" {
			a.CurrentDirection = direction
		}
	}
	a.CurrentValue = strconv.Itoa(n)
}

func (a CommandAction) Ready() bool {
	if a.ParamKind == "" {
		return true
	}
	return a.CurrentValue != "" && (a.ParamKind != "blind_position" || a.CurrentDirection != "")
}

func (a CommandAction) DisplayValue() string {
	if a.CurrentValue == "" || a.Unit() != "K" {
		return a.CurrentValue
	}
	n, err := strconv.Atoi(a.CurrentValue)
	if err != nil {
		return a.CurrentValue
	}
	rounded := ((n + 50) / 100) * 100
	if rounded != n {
		return fmt.Sprintf("約%d", rounded)
	}
	return strconv.Itoa(n)
}

func (a CommandAction) ColorPreview() string {
	// Zero RGB channels do not describe a visible emitted color.
	if a.CurrentValue == "" || a.CurrentValue == "#000000" {
		return "transparent"
	}
	return a.CurrentValue
}

func (a CommandAction) InitialValue() string {
	if a.CurrentValue != "" {
		return a.CurrentValue
	}
	if a.ParamKind == "color" {
		return "#000000"
	}
	return strconv.Itoa((a.Min + a.Max) / 2)
}

func (a CommandAction) Unit() string {
	if strings.Contains(strings.ToLower(a.Command), "temp") {
		return "K"
	}
	return "%"
}

func (d Device) Icon() string {
	kind := strings.ToLower(strings.TrimSpace(d.DeviceType + " " + d.RemoteType))
	switch {
	case strings.Contains(kind, "meter"), strings.Contains(kind, "thermo"):
		return "🌡"
	case strings.Contains(kind, "lock"):
		return "🔒"
	case strings.Contains(kind, "curtain"), strings.Contains(kind, "shade"), strings.Contains(kind, "blind"):
		return "▥"
	case isLightDevice(kind), kind == "light":
		return "💡"
	case isPlugOrSwitch(kind):
		return "🔌"
	case strings.Contains(kind, "tv"), strings.Contains(kind, "streamer"):
		return "📺"
	case strings.Contains(kind, "speaker"), strings.Contains(kind, "dvd"):
		return "♫"
	case strings.Contains(kind, "hub"):
		return "⌂"
	case strings.Contains(kind, "motion"), strings.Contains(kind, "presence"):
		return "◉"
	case strings.Contains(kind, "contact"):
		return "🚪"
	case strings.Contains(kind, "water"), strings.Contains(kind, "humidifier"):
		return "💧"
	case isRobotVacuum(kind):
		return "◉"
	case isClimateControl(kind):
		return "≋"
	case kind == "bot":
		return "☝"
	default:
		return "▣"
	}
}

func (a CommandAction) CommandRequest(form url.Values) (CommandRequest, error) {
	param := a.ParamValue
	if param == "" {
		param = "default"
	}

	switch a.ParamKind {
	case "":
	case "number":
		value, err := parseBoundedInt(form.Get("parameter"), a.Min, a.Max)
		if err != nil {
			return CommandRequest{}, err
		}
		param = strconv.Itoa(value)
	case "color":
		param = strings.TrimSpace(form.Get("parameter"))
		if strings.HasPrefix(param, "#") && len(param) == 7 {
			if rgb, err := hex.DecodeString(param[1:]); err == nil {
				param = fmt.Sprintf("%d:%d:%d", rgb[0], rgb[1], rgb[2])
			}
		}
		if !validRGB(param) {
			return CommandRequest{}, errors.New("RGB must be in r:g:b format with each value 0-255")
		}
	case "select":
		param = strings.TrimSpace(form.Get("parameter"))
		if !optionAllowed(a.Options, param) {
			return CommandRequest{}, errors.New("invalid option")
		}
	case "curtain_position":
		value, err := parseBoundedInt(form.Get("position"), 0, 100)
		if err != nil {
			return CommandRequest{}, err
		}
		param = fmt.Sprintf("0,ff,%d", value)
	case "blind_position":
		value, err := parseBoundedInt(form.Get("position"), 0, 100)
		if err != nil {
			return CommandRequest{}, err
		}
		if value%2 != 0 {
			return CommandRequest{}, errors.New("blind position must be an even number")
		}
		direction := strings.TrimSpace(form.Get("direction"))
		if direction != "up" && direction != "down" {
			return CommandRequest{}, errors.New("blind direction must be up or down")
		}
		param = fmt.Sprintf("%s;%d", direction, value)
	default:
		return CommandRequest{}, errors.New("unsupported parameter kind")
	}

	commandType := a.CommandType
	if commandType == "" {
		commandType = "command"
	}
	if !allowedCommand(a.Command) {
		return CommandRequest{}, errors.New("command is not in server allowlist")
	}
	return CommandRequest{Command: a.Command, Parameter: param, CommandType: commandType}, nil
}

func parseBoundedInt(value string, min, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, errors.New("parameter must be a number")
	}
	if n < min || n > max {
		return 0, fmt.Errorf("parameter must be between %d and %d", min, max)
	}
	return n, nil
}

func validRGB(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return false
		}
	}
	return true
}

func optionAllowed(options []ParamOption, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func findAction(device Device, id string) (CommandAction, bool) {
	for _, action := range actionsForDevice(device) {
		if action.ID == id {
			return action, true
		}
	}
	return CommandAction{}, false
}

func actionsForDevice(device Device) []CommandAction {
	if device.InfraredRemote {
		return infraredActions(remoteKind(device))
	}

	deviceType := strings.ToLower(device.DeviceType)
	switch {
	case deviceType == "bot":
		return []CommandAction{
			buttonAction("press", "Press", "press"),
			buttonAction("on", "On", "turnOn"),
			buttonAction("off", "Off", "turnOff"),
		}
	case strings.Contains(deviceType, "lock"):
		return []CommandAction{
			buttonAction("lock", "Lock", "lock"),
			buttonAction("unlock", "Unlock", "unlock"),
		}
	case strings.Contains(deviceType, "curtain") || strings.Contains(deviceType, "roller shade"):
		return []CommandAction{
			buttonAction("open", "Open", "turnOn"),
			buttonAction("close", "Close", "turnOff"),
			buttonAction("pause", "Pause", "pause"),
			{ID: "position", Label: "Move", Command: "setPosition", ParamKind: "curtain_position", Min: 0, Max: 100, Step: 5},
		}
	case strings.Contains(deviceType, "blind tilt"):
		return []CommandAction{
			buttonAction("fully_open", "Open", "fullyOpen"),
			buttonAction("close_up", "Close Up", "closeUp"),
			buttonAction("close_down", "Close Down", "closeDown"),
			{ID: "position", Label: "Move", Command: "setPosition", ParamKind: "blind_position", Min: 0, Max: 100, Step: 2},
		}
	case isLightDevice(deviceType):
		return lightActions(deviceType)
	case isPlugOrSwitch(deviceType):
		return []CommandAction{
			buttonAction("on", "On", "turnOn"),
			buttonAction("off", "Off", "turnOff"),
			buttonAction("toggle", "Toggle", "toggle"),
		}
	case isRobotVacuum(deviceType):
		return []CommandAction{
			buttonAction("pause", "Pause", "pause"),
			buttonAction("dock", "Dock", "dock"),
		}
	case isClimateControl(deviceType):
		return []CommandAction{
			buttonAction("on", "On", "turnOn"),
			buttonAction("off", "Off", "turnOff"),
		}
	default:
		return nil
	}
}

func buttonAction(id, label, command string) CommandAction {
	return CommandAction{ID: id, Label: label, Command: command, ParamValue: "default"}
}

func lightActions(deviceType string) []CommandAction {
	actions := []CommandAction{
		buttonAction("on", "On", "turnOn"),
		buttonAction("off", "Off", "turnOff"),
		buttonAction("toggle", "Toggle", "toggle"),
	}
	if strings.Contains(deviceType, "rgbicww ceiling") {
		return append(actions,
			buttonAction("main_on", "Main On", "turnOnMainLight"),
			buttonAction("main_off", "Main Off", "turnOffMainLight"),
			buttonAction("color_on", "Color On", "turnOnColorLight"),
			buttonAction("color_off", "Color Off", "turnOffColorLight"),
			CommandAction{ID: "main_brightness", Label: "Main Brightness", Command: "setMainLightBrightness", ParamKind: "number", Min: 1, Max: 100, Step: 5},
			CommandAction{ID: "main_temp", Label: "Main Temp", Command: "setMainLightColorTemp", ParamKind: "number", Min: 2700, Max: 6500, Step: 100},
			CommandAction{ID: "color_brightness", Label: "Color Brightness", Command: "setColorLightBrightness", ParamKind: "number", Min: 1, Max: 100, Step: 5},
			CommandAction{ID: "rgb", Label: "RGB", Command: "setColorLightRGB", ParamKind: "color", Placeholder: "255:180:80"},
		)
	}
	actions = append(actions,
		CommandAction{ID: "brightness", Label: "Brightness", Command: "setBrightness", ParamKind: "number", Min: 1, Max: 100, Step: 5},
	)
	if strings.Contains(deviceType, "bulb") || strings.Contains(deviceType, "strip") || strings.Contains(deviceType, "lamp") || strings.Contains(deviceType, "rgbic") {
		actions = append(actions, CommandAction{ID: "rgb", Label: "RGB", Command: "setColor", ParamKind: "color", Placeholder: "255:180:80"})
	}
	if !strings.Contains(deviceType, "candle") {
		actions = append(actions, CommandAction{ID: "temperature", Label: "Color Temp", Command: "setColorTemperature", ParamKind: "number", Min: 2700, Max: 6500, Step: 100})
	}
	return actions
}

func infraredActions(kind string) []CommandAction {
	actions := []CommandAction{
		buttonAction("on", "On", "turnOn"),
		buttonAction("off", "Off", "turnOff"),
	}
	switch strings.ToLower(kind) {
	case "air conditioner":
		return nil
	case "tv", "iptv/streamer", "set top box":
		return append(actions,
			buttonAction("vol_up", "Vol +", "volumeAdd"),
			buttonAction("vol_down", "Vol -", "volumeSub"),
			buttonAction("ch_up", "Ch +", "channelAdd"),
			buttonAction("ch_down", "Ch -", "channelSub"),
		)
	case "dvd", "speaker":
		return append(actions,
			buttonAction("mute", "Mute", "setMute"),
			buttonAction("vol_up", "Vol +", "volumeAdd"),
			buttonAction("vol_down", "Vol -", "volumeSub"),
		)
	case "fan":
		return append(actions,
			buttonAction("swing", "Swing", "swing"),
			buttonAction("low", "Low", "lowSpeed"),
			buttonAction("middle", "Mid", "middleSpeed"),
			buttonAction("high", "High", "highSpeed"),
		)
	case "light":
		return append(actions,
			buttonAction("brightness_up", "Bright +", "brightnessUp"),
			buttonAction("brightness_down", "Bright -", "brightnessDown"),
		)
	default:
		return actions
	}
}

func remoteKind(device Device) string {
	if device.RemoteType != "" {
		return device.RemoteType
	}
	return device.DeviceType
}

func isLightDevice(deviceType string) bool {
	return strings.Contains(deviceType, "light") ||
		strings.Contains(deviceType, "bulb") ||
		strings.Contains(deviceType, "lamp")
}

func isPlugOrSwitch(deviceType string) bool {
	return strings.Contains(deviceType, "plug") ||
		strings.Contains(deviceType, "relay switch")
}

func isRobotVacuum(deviceType string) bool {
	return strings.Contains(deviceType, "vacuum") ||
		strings.Contains(deviceType, "cleaning robot") ||
		strings.Contains(deviceType, "k10") ||
		strings.Contains(deviceType, "k20") ||
		strings.Contains(deviceType, "s10") ||
		strings.Contains(deviceType, "s20")
}

func isClimateControl(deviceType string) bool {
	return strings.Contains(deviceType, "fan") ||
		strings.Contains(deviceType, "humidifier") ||
		strings.Contains(deviceType, "air purifier") ||
		strings.Contains(deviceType, "radiator thermostat")
}

type SwitchBotClient struct {
	Token      string
	Secret     string
	HTTPClient *http.Client
}

type APIResponse[T any] struct {
	StatusCode int    `json:"statusCode"`
	Message    string `json:"message"`
	Body       T      `json:"body"`
}

type DeviceListBody struct {
	DeviceList           []Device `json:"deviceList"`
	InfraredRemoteList   []Device `json:"infraredRemoteList"`
	VirtualRemoteDevices []Device `json:"virtualRemoteList"`
}

type Device struct {
	DeviceID            string `json:"deviceId"`
	DeviceName          string `json:"deviceName"`
	DeviceType          string `json:"deviceType"`
	EnableCloudService  bool   `json:"enableCloudService"`
	HubDeviceID         string `json:"hubDeviceId"`
	RemoteType          string `json:"remoteType"`
	ParentDeviceID      string `json:"parentDeviceId"`
	ParentDeviceType    string `json:"parentDeviceType"`
	ConnectionType      string `json:"connectionType"`
	Controllable        bool   `json:"-"`
	InfraredRemote      bool   `json:"-"`
	CloudServiceDisplay string `json:"-"`
	Actions             []CommandAction
	Sensor              bool   `json:"-"`
	Temperature         string `json:"-"`
	Humidity            string `json:"-"`
	SensorError         string `json:"-"`
}

type CommandRequest struct {
	Command     string `json:"command"`
	Parameter   string `json:"parameter"`
	CommandType string `json:"commandType"`
}

func (c *SwitchBotClient) Devices(ctx context.Context) ([]Device, error) {
	var resp APIResponse[DeviceListBody]
	if err := c.do(ctx, http.MethodGet, "/v1.1/devices", nil, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 100 {
		return nil, fmt.Errorf("switchbot devices failed: %d %s", resp.StatusCode, resp.Message)
	}
	devices := make([]Device, 0, len(resp.Body.DeviceList)+len(resp.Body.InfraredRemoteList)+len(resp.Body.VirtualRemoteDevices))
	for _, d := range resp.Body.DeviceList {
		d.Controllable = true
		d.CloudServiceDisplay = cloudService(d.EnableCloudService)
		d.Actions = actionsForDevice(d)
		devices = append(devices, d)
	}
	for _, d := range resp.Body.InfraredRemoteList {
		d.InfraredRemote = true
		d.Controllable = true
		d.CloudServiceDisplay = "-"
		d.Actions = actionsForDevice(d)
		devices = append(devices, d)
	}
	for _, d := range resp.Body.VirtualRemoteDevices {
		d.InfraredRemote = true
		d.Controllable = true
		d.CloudServiceDisplay = "-"
		d.Actions = actionsForDevice(d)
		devices = append(devices, d)
	}
	return devices, nil
}

func (c *SwitchBotClient) Status(ctx context.Context, deviceID string) (map[string]any, error) {
	var resp APIResponse[map[string]any]
	if err := c.do(ctx, http.MethodGet, "/v1.1/devices/"+deviceID+"/status", nil, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCode != 100 {
		return resp.Body, fmt.Errorf("switchbot status failed: %d %s", resp.StatusCode, resp.Message)
	}
	return resp.Body, nil
}

func (c *SwitchBotClient) Command(ctx context.Context, deviceID string, cmd CommandRequest) (APIResponse[json.RawMessage], error) {
	var resp APIResponse[json.RawMessage]
	if err := c.do(ctx, http.MethodPost, "/v1.1/devices/"+deviceID+"/commands", cmd, &resp); err != nil {
		return resp, err
	}
	if resp.StatusCode != 100 {
		return resp, fmt.Errorf("switchbot command failed: %d %s", resp.StatusCode, resp.Message)
	}
	return resp, nil
}

func (c *SwitchBotClient) do(ctx context.Context, method, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		payload = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, switchBotHost+path, payload)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf8")
	}
	c.sign(req)

	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("switchbot http error: %s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return err
	}
	return nil
}

func (c *SwitchBotClient) sign(req *http.Request) {
	nonce := randomNonce()
	t := fmt.Sprintf("%d", time.Now().UnixMilli())
	message := c.Token + t + nonce
	mac := hmac.New(sha256.New, []byte(c.Secret))
	_, _ = mac.Write([]byte(message))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("Authorization", c.Token)
	req.Header.Set("sign", sign)
	req.Header.Set("t", t)
	req.Header.Set("nonce", nonce)
}

func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func cloudService(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

type PageData struct {
	Query       string
	Devices     []Device
	AllDevices  []Device
	Count       int
	Total       int
	LastRefresh time.Time
	CacheTTL    time.Duration
	StatusTTL   time.Duration
	CSRFToken   string
	StartedAt   time.Time
}

type StatusPageData struct {
	BackURL    string
	RefreshURL string
	Device     Device
	DeviceID   string
	Status     map[string]any
	FetchedAt  time.Time
	Error      string
}

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"detailsURL": detailsURL,
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return time.Since(t).Round(time.Second).String() + " ago"
	},
}).Parse(pageHTML))

var statusTemplate = template.Must(template.New("status").Parse(statusHTML))

const pageHTML = `<!doctype html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>SwitchBot Panel</title>
  <style>
    :root { color-scheme: light dark; --bg:#f7f5ef; --fg:#1f2933; --muted:#667085; --line:#d9d4c7; --panel:#fffdf7; --accent:#0f766e; --danger:#b42318; }
    @media (prefers-color-scheme: dark) { :root { --bg:#141414; --fg:#f2f4f7; --muted:#a3a8b3; --line:#363636; --panel:#1d1d1d; --accent:#2dd4bf; --danger:#ff8a7a; } }
    * { box-sizing: border-box; }
    body { margin:0; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background:var(--bg); color:var(--fg); }
    header { padding:10px clamp(16px, 4vw, 40px); border-bottom:1px solid var(--line); display:grid; grid-template-columns:auto minmax(160px, 360px) auto; gap:10px 20px; align-items:center; }
    .brand { display:flex; align-items:baseline; gap:10px; min-width:0; }
    h1 { margin:0; font-size:18px; letter-spacing:0; white-space:nowrap; }
    .device-count { color:var(--muted); font-size:12px; white-space:nowrap; }
    main { padding:12px clamp(16px, 4vw, 40px) 48px; }
    .toolbar { display:flex; gap:8px; align-items:center; min-width:0; }
    .toolbar input { min-width:0; width:100%; }
    .toolbar button, .toolbar a { flex-shrink:0; }
    .header-actions { display:flex; justify-self:end; gap:8px; }
    .layout-group + .layout-group { margin-top:24px; }
    .layout-group h2 { margin:0 0 12px; font-size:16px; overflow-wrap:anywhere; }
    .layout-toggle { display:flex; align-items:center; justify-content:space-between; gap:12px; width:100%; padding:8px 12px; text-align:left; }
    .layout-toggle::after { content:'⌄'; flex-shrink:0; }
    .layout-toggle[aria-expanded="true"]::after { transform:rotate(180deg); }
    .device-list.arranged { columns:auto; display:grid; grid-template-columns:repeat(auto-fill, minmax(min(100%, 330px), 1fr)); gap:16px; align-items:start; }
    .device-list.arranged[hidden] { display:none; }
    .arranged .device { margin:0; }
    .layout-tools { position:sticky; top:0; z-index:2; padding:8px 0; background:var(--bg); }
    .layout-tools > div, .layout-group-controls { display:flex; flex-wrap:wrap; align-items:center; gap:8px; }
    .layout-tools p { margin:8px 0 0; font-size:13px; color:var(--muted); }
    .layout-group-controls { margin-bottom:10px; }
    .layout-group-controls input { min-width:0; width:min(240px, 100%); }
    .editing-layout .layout-group { padding:12px; border:1px dashed var(--line); border-radius:12px; }
    .editing-layout .device-list { min-height:52px; }
    .layout-handle { align-self:flex-end; touch-action:none; cursor:grab; }
    .layout-dragging .layout-handle { cursor:grabbing; }
    .layout-dragging { opacity:.55; }
    .layout-drop-group { outline:2px solid var(--accent); }
    .layout-drop-before { box-shadow:0 -4px 0 var(--accent); }
    .layout-drop-after { box-shadow:0 4px 0 var(--accent); }
    .layout-drop-left { box-shadow:-4px 0 0 var(--accent); }
    .layout-drop-right { box-shadow:4px 0 0 var(--accent); }
    #layout-error { color:var(--danger); }
    input, select, button { font:inherit; border:1px solid var(--line); background:var(--panel); color:var(--fg); border-radius:6px; min-height:38px; }
    input { padding:0 12px; min-width:min(320px, 100%); }
    input.compact { min-width:0; width:86px; }
    select.compact { min-width:72px; padding:0 8px; }
    button { padding:0 12px; cursor:pointer; }
    button.primary { background:var(--accent); color:white; border-color:var(--accent); }
    button.danger { color:var(--danger); }
    button:disabled { opacity:0.45; cursor:default; }
    .device-list { columns:330px; column-gap:16px; }
    .device { display:inline-flex; width:100%; break-inside:avoid; margin-bottom:16px; vertical-align:top; flex-direction:column; gap:18px; padding:20px; min-width:0; border:1px solid var(--line); background:var(--panel); border-radius:20px; box-shadow:0 3px 12px #00000005; }
    .device-main { position:relative; min-width:0; width:100%; }
    .device-heading { display:flex; gap:12px; align-items:center; }
    .device-icon { display:grid; place-items:center; width:48px; height:48px; flex-shrink:0; border-radius:14px; font-size:28px; background:color-mix(in srgb, var(--accent) 10%, var(--panel)); }
    .device-title { flex:1; min-width:0; }
    .name-row { display:flex; align-items:baseline; gap:8px; }
    .name-row .name { flex:1; min-width:0; }
    .details-link { flex-shrink:0; font-size:12px; padding:8px 0 8px 8px; }
    .name { overflow-wrap:anywhere; }
    .device-meta { margin-top:4px; display:flex; gap:8px; flex-wrap:wrap; color:var(--muted); font-size:12px; }
    .pill { border:1px solid var(--line); border-radius:999px; padding:2px 7px; }
    .empty { padding:24px; border:1px solid var(--line); background:var(--panel); border-radius:6px; color:var(--muted); }
    .muted { color:var(--muted); }
    .name { font-weight:650; }
    .readings { display:flex; justify-content:space-around; gap:16px; flex-wrap:wrap; margin-top:24px; padding:16px 0; font-size:32px; font-weight:650; }
    .readings > span { display:flex; flex-direction:column; gap:8px; padding:8px; border-radius:8px; }
    .reading-alert { font-size:12px; font-weight:600; }
    .readings [data-alert="red"] { color:#a52319; background:#ffe3df; }
    .readings [data-alert="yellow"] { color:#715000; background:#fff0bc; }
    @media (prefers-color-scheme: dark) {
      .readings [data-alert="red"] { color:#ffb7ae; background:#502521; }
      .readings [data-alert="yellow"] { color:#ffe08b; background:#433715; }
    }
    .device:has(.sensor-thresholds[open]) { position:relative; z-index:1; }
    .sensor-thresholds { flex-shrink:0; font-size:12px; }
    .sensor-thresholds summary { list-style:none; cursor:pointer; color:var(--accent); }
    .sensor-thresholds summary::-webkit-details-marker { display:none; }
    .sensor-thresholds form { position:absolute; right:0; width:min(330px, 100%); z-index:1; display:grid; gap:8px; margin-top:8px; padding:12px; border:1px solid var(--line); border-radius:10px; background:var(--panel); box-shadow:0 6px 20px #0003; }
    .sensor-thresholds fieldset { display:flex; flex-wrap:wrap; gap:8px; margin:0; min-width:0; border:1px solid var(--line); border-radius:8px; }
    .sensor-thresholds label { display:flex; align-items:center; flex-wrap:wrap; gap:6px; }
    .sensor-thresholds input { width:88px; min-width:0; }
    .sensor-thresholds button { justify-self:end; }
    .reading-label { font-size:12px; color:var(--muted); font-weight:400; margin-right:4px; }
    .commands { display:grid; grid-template-columns:repeat(3, minmax(0, 1fr)); gap:8px; width:100%; align-items:start; }
    .commands > form > button { display:flex; flex-direction:column; justify-content:center; align-items:center; gap:8px; width:100%; min-height:70px; padding:10px 4px; font-size:12px; border-radius:12px; }
    .action-icon { font-size:24px; line-height:1; }
    .commands > .setting-control { grid-column:1 / -1; display:grid; grid-template-columns:minmax(0, 1fr) auto; align-items:center; gap:10px; padding:14px; border:1px solid var(--line); border-radius:14px; }
    .setting-control > label { grid-column:1 / -1; display:flex; align-items:center; gap:8px; font-size:13px; }
    .setting-control output { margin-left:auto; font-variant-numeric:tabular-nums; }
    .setting-control input[type=range] { appearance:none; width:100%; min-width:0; padding:0; border:0; background:transparent; cursor:pointer; }
    .setting-control input[type=range]::-webkit-slider-runnable-track { height:6px; border-radius:99px; background:var(--slider-track, var(--line)); }
    .setting-control input[type=range]::-moz-range-track { height:6px; border-radius:99px; background:var(--slider-track, var(--line)); }
    .setting-control input[type=range]::-webkit-slider-thumb { appearance:none; width:18px; height:18px; border-radius:50%; background:var(--accent); margin-top:-6px; }
    .setting-control input[type=range]::-moz-range-thumb { width:18px; height:18px; border:0; border-radius:50%; background:var(--accent); }
    .color-picker { position:relative; display:grid; place-items:center; min-height:44px; border:1px solid var(--line); border-radius:8px; overflow:hidden; background:linear-gradient(var(--picked-color), var(--picked-color)), repeating-conic-gradient(var(--panel) 0% 25%, var(--line) 0% 50%) 0 / 12px 12px; }
    .color-picker span { padding:3px 8px; border-radius:4px; background:var(--panel); color:var(--fg); font-size:12px; }
    .color-picker input[type=color] { position:absolute; inset:0; width:100%; height:100%; min-width:0; opacity:0; cursor:pointer; }
    .setting-control > .color-picker { grid-column:1; display:grid; gap:0; }
    .color-picker:focus-within { outline:2px solid var(--accent); outline-offset:2px; }
    .commands > form > .apply-button { min-height:42px; width:auto; padding:0 14px; justify-self:end; }
    .setting-control select { padding:0 10px; min-width:0; }
    .setting-control input[data-unit="K"] { --slider-track:linear-gradient(90deg, #ffbb59, #f9f5e9, #bfdcff); }
    .commands > .setting-control > select { grid-column:1 / -1; width:100%; }
    .position-ends { display:flex; justify-content:space-between; grid-column:1 / -1; font-size:11px; color:var(--muted); }
    .commands form { display:flex; gap:4px; align-items:center; }
    .commands form:not(.setting-control) { flex-direction:column; align-items:stretch; }
    .feedback { grid-column:1 / -1; margin:4px 0 0; font-size:12px; line-height:1.5; overflow-wrap:anywhere; color:var(--accent); }
    .feedback[data-kind="error"] { color:var(--danger); }
    .meta { display:flex; gap:14px; color:var(--muted); font-size:13px; flex-wrap:wrap; }
    a { color:var(--accent); text-decoration:none; }
    a:hover { text-decoration:underline; }
    @media (max-width: 760px) {
      header { grid-template-columns:minmax(0, 1fr) auto; gap:8px 12px; }
      .toolbar { grid-column:1 / -1; grid-row:2; }
      .header-actions { grid-column:2; grid-row:1; }
      .brand { flex-wrap:wrap; gap:2px 8px; }
      .device { grid-template-columns:1fr; }
      input.compact { width:74px; }
    }
  </style>
</head>
<body>
  <header>
    <div class="brand">
      <h1>SwitchBot Panel</h1>
      <span class="device-count">{{.Count}} / {{.Total}} 台</span>
    </div>
    <form class="toolbar" method="get" action="/">
      <input aria-label="機器を探す" name="q" value="{{.Query}}" placeholder="機器を探す">
      <button type="submit">検索</button>
      {{if .Query}}<a href="/">クリア</a>{{end}}
    </form>
    <div class="header-actions"><button type="button" id="layout-open" hidden>配置</button>
    <form class="refresh" method="post" action="/refresh"><input type="hidden" name="csrf" value="{{.CSRFToken}}"><input type="hidden" name="q" value="{{.Query}}"><button class="primary" type="submit" title="一覧の更新: {{since .LastRefresh}}">↻ 更新</button></form></div>
  </header>
  <main>
    <p id="page-feedback" class="feedback" role="status" aria-live="polite" hidden></p>
    <div id="layout-tools" class="layout-tools" hidden>
      <div>
        <button type="button" id="layout-add">グループを追加</button>
        <button type="button" id="layout-cancel">キャンセル</button>
        <button type="button" id="layout-save" class="primary">保存</button>
      </div>
      <p>移動ハンドルをドラッグしてカードやグループを並べ替え</p>
      <p id="layout-error" role="alert" hidden></p>
    </div>
    <div class="device-board">
      <template class="layout-devices">{{range .AllDevices}}<span data-id="{{.DeviceID}}">{{.DeviceName}}</span>{{end}}</template>
      <div class="device-list">
    {{range .Devices}}
          {{$device := .}}
          <article class="device" id="device-{{.DeviceID}}" data-device-id="{{.DeviceID}}">
            <div class="device-main">
              <div class="device-heading">
                <span class="device-icon" aria-hidden="true">{{.Icon}}</span>
                <div class="device-title">
                  <div class="name-row">
                    <div class="name">{{.DeviceName}}</div>
                    {{if .Sensor}}
                    <details class="sensor-thresholds" hidden>
                <summary>しきい値</summary>
                <form>
                  <fieldset>
                    <legend>温度</legend>
                    <label>下限 <input type="number" name="temperature" step="any" placeholder="未設定"> °C</label>
                    <label>表示色 <select name="temperature-color"><option value="red">赤</option><option value="yellow">黄</option></select></label>
                  </fieldset>
                  <fieldset>
                    <legend>湿度</legend>
                    <label>下限 <input type="number" name="humidity" min="0" max="100" step="any" placeholder="未設定"> %</label>
                    <label>表示色 <select name="humidity-color"><option value="red">赤</option><option value="yellow">黄</option></select></label>
                  </fieldset>
                  <p class="feedback" role="alert" hidden></p>
                  <button type="submit">保存</button>
                </form>
              </details>
                    {{end}}
                    <a class="details-link" aria-label="{{.DeviceName}}の詳細" href="{{detailsURL .DeviceID $.Query}}">詳細 ↗</a>
                  </div>
                </div>
              </div>
              {{if .Sensor}}
              <div class="readings">
                <span data-metric="temperature" data-value="{{.Temperature}}"><span class="reading-label">🌡 温度</span>{{if .Temperature}}{{.Temperature}} °C{{else}}—{{end}}<span class="reading-alert" hidden>下限未満</span></span>
                <span data-metric="humidity" data-value="{{.Humidity}}"><span class="reading-label">💧 湿度</span>{{if .Humidity}}{{.Humidity}} %{{else}}—{{end}}<span class="reading-alert" hidden>下限未満</span></span>
              </div>

              {{if .SensorError}}<div class="muted">{{.SensorError}}</div>{{end}}
              {{end}}

            </div>
            {{if .Actions}}
            <div class="commands">
              {{range .Actions}}
              <form{{if .ParamKind}} class="setting-control"{{end}} method="post" action="/devices/{{$device.DeviceID}}/command">
                <input type="hidden" name="csrf" value="{{$.CSRFToken}}">
                <input type="hidden" name="action" value="{{.ID}}">
                <input type="hidden" name="q" value="{{$.Query}}">
                {{if eq .ParamKind "number"}}
                <label><span aria-hidden="true">{{.Icon}}</span>{{.Title}}<output>{{if .CurrentValue}}{{.DisplayValue}} {{.Unit}}{{else}}未取得{{end}}</output></label>
                <input aria-label="{{.Title}}" type="range" name="parameter" min="{{.Min}}" max="{{.Max}}" step="1" value="{{.InitialValue}}" data-unit="{{.Unit}}">
                {{else if eq .ParamKind "color"}}
                <label><span aria-hidden="true">🎨</span>{{.Title}}{{if not .CurrentValue}}<output>未取得</output>{{end}}</label>
                <label class="color-picker" style="--picked-color:{{.ColorPreview}}"><span aria-hidden="true">色を選ぶ</span><input aria-label="{{.Title}}" type="color" name="parameter" value="{{.InitialValue}}"></label>
                {{else if eq .ParamKind "curtain_position"}}
                <label><span aria-hidden="true">▥</span>閉じ具合<output>{{if .CurrentValue}}{{.CurrentValue}} %{{else}}未取得{{end}}</output></label>
                <input aria-label="閉じ具合" type="range" name="position" min="0" max="100" step="1" value="{{.InitialValue}}" data-unit="%">
                {{else if eq .ParamKind "blind_position"}}
                <label><span aria-hidden="true">▥</span>ブラインドの角度<output>{{if .CurrentValue}}{{.CurrentValue}} %{{else}}未取得{{end}}</output></label>
                <select aria-label="ブラインドの向き" name="direction" required><option value="" disabled{{if not .CurrentDirection}} selected{{end}}>向きを選ぶ</option><option value="up"{{if eq .CurrentDirection "up"}} selected{{end}}>↑ 上向き</option><option value="down"{{if eq .CurrentDirection "down"}} selected{{end}}>↓ 下向き</option></select>
                <input aria-label="ブラインドの角度" type="range" name="position" min="0" max="100" step="2" value="{{.InitialValue}}" data-unit="%">
                {{end}}
                <button{{if .ParamKind}} class="apply-button"{{end}} type="submit"{{if .ParamKind}} disabled{{end}}>{{if .ParamKind}}適用{{else}}<span class="action-icon" aria-hidden="true">{{.Icon}}</span>{{.Title}}{{end}}</button>
              </form>
              {{end}}
            </div>
            {{end}}
          </article>
    {{else}}
    <div class="empty">該当する機器がありません。</div>
    {{end}}
    </div>
    </div>
  </main>
<script src="/panel.js" defer></script>
</body>
</html>`

const statusHTML = `<!doctype html>
<html lang="ja">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>機器の詳細</title>
  <style>
    :root { color-scheme: light dark; --bg:#f7f5ef; --fg:#1f2933; --muted:#667085; --line:#d9d4c7; --panel:#fffdf7; --accent:#0f766e; --danger:#b42318; }
    @media (prefers-color-scheme: dark) { :root { --bg:#141414; --fg:#f2f4f7; --muted:#a3a8b3; --line:#363636; --panel:#1d1d1d; --accent:#2dd4bf; --danger:#ff8a7a; } }
    body { margin:0; font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background:var(--bg); color:var(--fg); }
    main { padding:24px clamp(16px, 4vw, 40px); }
    a { color:var(--accent); text-decoration:none; }
    a:hover { text-decoration:underline; }
    h1 { margin:12px 0 4px; font-size:24px; letter-spacing:0; }
    .muted { color:var(--muted); }
    .error { color:var(--danger); }
    dl { display:grid; grid-template-columns:minmax(120px, 240px) 1fr; gap:0; border:1px solid var(--line); background:var(--panel); }
    dt, dd { margin:0; padding:10px 12px; border-bottom:1px solid var(--line); overflow-wrap:anywhere; min-width:0; }
    dt { color:var(--muted); }
    pre { white-space:pre-wrap; overflow:auto; border:1px solid var(--line); background:var(--panel); padding:12px; }
  </style>
</head>
<body>
  <main>
    <a href="{{.BackURL}}">← 一覧に戻る</a>
    <h1>{{if .Device.DeviceName}}{{.Device.DeviceName}}{{else}}{{.DeviceID}}{{end}}</h1>
    <h2>機器情報</h2>
    <dl>
      <dt>ID</dt><dd>{{.DeviceID}}</dd>
      <dt>種類</dt><dd>{{.Device.DeviceType}}{{if .Device.InfraredRemote}}{{.Device.RemoteType}} · 赤外線リモコン{{end}}</dd>
      <dt>クラウド連携</dt><dd>{{.Device.CloudServiceDisplay}}</dd>
      {{if .Device.HubDeviceID}}<dt>Hub</dt><dd>{{.Device.HubDeviceID}}</dd>{{end}}
    </dl>
    <h2>状態</h2>
    <p class="muted">取得日時 {{.FetchedAt}}</p>
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
    <dl>
      {{range $k, $v := .Status}}
      <dt>{{$k}}</dt><dd>{{$v}}</dd>
      {{else}}
      <dt>Status</dt><dd class="muted">状態を取得できませんでした。</dd>
      {{end}}
    </dl>
    <p><a href="{{.RefreshURL}}">↻ 状態を更新</a></p>
  </main>
</body>
</html>`
