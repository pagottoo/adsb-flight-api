// flight-api — micro-serviço que responde "qual avião está no céu agora".
//
//  1. Servidor HTTP (porta 8890): /overhead, /healthz (liveness) e /ready
//     (readiness: 200 só quando o WebSocket com o HA está conectado).
//  2. Announcer opcional: se HA_TOKEN estiver setado, abre um WebSocket com o
//     Home Assistant, cria o gatilho input_boolean.perguntar_voo, escuta, e
//     quando ele liga (via rotina do Google Home) calcula o voo e manda o HA
//     falar a resposta no media_player configurado — depois desliga o gatilho.
//
// Dados: lê o aircraft.json do readsb/tar1090 e enriquece a rota via adsbdb.com.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ── Config ───────────────────────────────────────────────────────────────────
const (
	adsbdbURL   = "https://api.adsbdb.com/v0/callsign/%s"
	minAltFt    = 150.0
	ftToM       = 0.3048
	httpTimeout = 3 * time.Second
)

var (
	port        = getenv("PORT", "8890")
	aircraftURL = getenv("AIRCRAFT_URL", "http://localhost:8080/data/aircraft.json")
	haURL       = strings.TrimRight(getenv("HA_URL", ""), "/")
	haToken     = getenv("HA_TOKEN", "")
	mediaPlayer = getenv("HA_MEDIA_PLAYER", "media_player.casa_toda")
	ttsEntity   = getenv("HA_TTS_ENTITY", "tts.google_translate_en_com")
	ttsLang     = getenv("HA_TTS_LANG", "") // vazio = não envia language (Piper dá 500 se receber)
	ttsVoice    = getenv("HA_TTS_VOICE", "") // ex.: pt_BR-faber-medium (voz do Piper)
	triggerBool = getenv("HA_TRIGGER_BOOLEAN", "input_boolean.perguntar_voo")
	triggerName = getenv("HA_TRIGGER_NAME", "Perguntar Voo")

	// Priorização "janela": só aviões perto do azimute da varanda e acima de uma
	// elevação mínima (evita anunciar tráfego decolando, baixo no horizonte).
	announceMode = getenv("ANNOUNCE_MODE", "auto")
	windowAz     = getenvFloat("WINDOW_AZIMUTH", 167.0)
	windowTol    = getenvFloat("WINDOW_TOLERANCE", 90.0)
	windowMaxKm  = getenvFloat("WINDOW_MAX_KM", 4.0)      // proximidade: o avião que você vê está perto
	windowMaxClimb = getenvFloat("WINDOW_MAX_CLIMB", 1000) // ft/min acima disso = decolando -> ignora
	obsAltM      = getenvFloat("OBSERVER_ALT_M", 850.0)
)

// Cidades que o adsbdb devolve em inglês -> PT
var cityPtBr = map[string]string{
	"Rome": "Roma", "Lisbon": "Lisboa", "Geneva": "Genebra", "Munich": "Munique",
	"Zurich": "Zurique", "Moscow": "Moscou", "Athens": "Atenas", "Florence": "Florença",
	"Venice": "Veneza", "Cologne": "Colônia", "Istanbul": "Istambul",
	"Mexico City": "Cidade do México", "Cairo": "Cairo", "Buenos Aires": "Buenos Aires",
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// ── Modelos de dados ─────────────────────────────────────────────────────────
type Aircraft struct {
	Flight  string          `json:"flight"`
	R       string          `json:"r"`
	T       string          `json:"t"`
	Desc    string          `json:"desc"`
	AltBaro json.RawMessage `json:"alt_baro"` // int OU a string "ground"
	Lat      *float64       `json:"lat"`
	RDst     *float64       `json:"r_dst"`
	RDir     *float64       `json:"r_dir"`
	BaroRate *float64       `json:"baro_rate"` // ft/min; + sobe (decola), - desce (pousa)
}

func (a *Aircraft) altFeet() (float64, bool) {
	if len(a.AltBaro) == 0 {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(a.AltBaro, &f); err == nil {
		return f, true
	}
	return 0, false // "ground" ou outra string
}

func (a *Aircraft) airborne() bool {
	f, ok := a.altFeet()
	return ok && f >= minAltFt
}

// elevationDeg estima o ângulo de elevação visto do observador (graus).
func (a *Aircraft) elevationDeg() (float64, bool) {
	f, ok := a.altFeet()
	if !ok || a.RDst == nil {
		return 0, false
	}
	slant := *a.RDst * 1000 // km -> m (distância em linha de visada)
	if slant <= 0 {
		return 0, false
	}
	ratio := (f*ftToM - obsAltM) / slant
	if ratio > 1 {
		ratio = 1
	} else if ratio < -1 {
		ratio = -1
	}
	return math.Asin(ratio) * 180 / math.Pi, true
}

type place struct {
	Municipality string `json:"municipality"`
	Name         string `json:"name"`
}

type routeInfo struct{ Airline, Origin, Destination string }

// Answer é o JSON devolvido por /overhead e a base da fala.
type Answer struct {
	Found        bool     `json:"found"`
	Speech       string   `json:"speech"`
	Callsign     string   `json:"callsign,omitempty"`
	Registration string   `json:"registration,omitempty"`
	Type         string   `json:"type,omitempty"`
	TypeDesc     string   `json:"type_desc,omitempty"`
	Airline      string   `json:"airline,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	Destination  string   `json:"destination,omitempty"`
	DistanceKm   *float64 `json:"distance_km,omitempty"`
	BearingDeg   *float64 `json:"bearing_deg,omitempty"`
	AltitudeM    *int     `json:"altitude_m,omitempty"`
	ElevationDeg *float64 `json:"elevation_deg,omitempty"`
	InWindow     bool     `json:"in_window,omitempty"`
	descr        string   // frase descritiva (uso interno p/ fallback; não serializado)
}

// wsConnected reflete se o announcer está com o WebSocket do HA autenticado e
// inscrito no gatilho. É a fonte da verdade do /ready (readiness). Atualizado só
// pelo announcer; lido pelo handler HTTP — atomic evita corrida entre as goroutines.
var wsConnected atomic.Bool

// ── HTTP helpers ─────────────────────────────────────────────────────────────
func getJSON(url string, target any) error {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "flight-api/1.0")
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(target)
}

// ── Lógica de seleção/enriquecimento ─────────────────────────────────────────
func cleanCallsign(raw string) string {
	cs := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(raw), "@"))
	if len([]rune(cs)) < 3 {
		return ""
	}
	for _, r := range cs {
		if unicode.IsLetter(r) {
			return cs
		}
	}
	return ""
}

func angDiff(a *Aircraft) float64 {
	dir := 999.0
	if a.RDir != nil {
		dir = *a.RDir
	}
	return math.Abs(math.Mod(dir-windowAz+180, 360) - 180)
}

func pickAircraft(list []Aircraft, mode string) *Aircraft {
	var cands []*Aircraft
	for i := range list {
		a := &list[i]
		if a.Lat != nil && a.RDst != nil && a.airborne() {
			cands = append(cands, a)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	if mode == "window" {
		var win []*Aircraft
		for _, a := range cands {
			if windowMaxKm > 0 && *a.RDst > windowMaxKm { // proximidade: o sinal principal
				continue
			}
			if angDiff(a) > windowTol { // hemisfério à frente (guarda contra o que está atrás)
				continue
			}
			if a.BaroRate != nil && *a.BaroRate > windowMaxClimb { // subindo forte = decolando -> ignora
				continue
			}
			win = append(win, a)
		}
		if len(win) == 0 {
			return nil // nada na janela; sem fallback off-axis
		}
		best := win[0]
		for _, a := range win[1:] {
			if *a.RDst < *best.RDst {
				best = a
			}
		}
		return best
	}
	best := cands[0]
	for _, a := range cands[1:] {
		if *a.RDst < *best.RDst {
			best = a
		}
	}
	return best
}

func city(p place) string {
	c := p.Municipality
	if c == "" {
		c = p.Name
	}
	if v, ok := cityPtBr[c]; ok {
		return v
	}
	return c
}

func lookupRoute(callsign string) routeInfo {
	var top struct {
		Response json.RawMessage `json:"response"`
	}
	if err := getJSON(fmt.Sprintf(adsbdbURL, callsign), &top); err != nil {
		return routeInfo{}
	}
	var wrap struct {
		FlightRoute struct {
			Airline     struct{ Name string `json:"name"` } `json:"airline"`
			Origin      place                               `json:"origin"`
			Destination place                               `json:"destination"`
		} `json:"flightroute"`
	}
	// Quando desconhecido, Response é a string "unknown callsign" -> Unmarshal falha.
	if err := json.Unmarshal(top.Response, &wrap); err != nil {
		return routeInfo{}
	}
	fr := wrap.FlightRoute
	return routeInfo{Airline: fr.Airline.Name, Origin: city(fr.Origin), Destination: city(fr.Destination)}
}

// isAllUpper imita o critério do str.isupper() do Python.
func isAllUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			hasLetter = true
			if !unicode.IsUpper(r) {
				return false
			}
		}
	}
	return hasLetter
}

// titleCase imita str.title() do Python (primeira letra de cada palavra).
func titleCase(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevLetter {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			prevLetter = true
		} else {
			b.WriteRune(r)
			prevLetter = false
		}
	}
	return b.String()
}

func fmtDist(d float64) string {
	if d < 10 {
		return strings.Replace(fmt.Sprintf("%.1f", d), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f", d)
}

// descriptive monta a frase da aeronave (sem o "Esse é " na frente), reutilizável.
func descriptive(callsign, desc string, route routeInfo, distKm *float64, altM *int) string {
	var parts []string
	switch {
	case route.Airline != "" && callsign != "":
		parts = append(parts, "um voo da "+route.Airline+", um "+desc)
	case callsign != "":
		parts = append(parts, "o voo "+callsign+", um "+desc)
	default:
		parts = append(parts, "um "+desc)
	}
	if route.Origin != "" && route.Destination != "" {
		parts = append(parts, fmt.Sprintf("de %s para %s", route.Origin, route.Destination))
	}
	if distKm != nil {
		parts = append(parts, "a "+fmtDist(*distKm)+" quilômetros")
	}
	if altM != nil {
		parts = append(parts, fmt.Sprintf("e %d metros de altitude", *altM))
	}
	return strings.Join(parts, ", ")
}

// bearingCardinal converte azimute (graus) em ponto cardeal em PT.
func bearingCardinal(deg float64) string {
	dirs := []string{"Norte", "Nordeste", "Leste", "Sudeste", "Sul", "Sudoeste", "Oeste", "Noroeste"}
	i := int(math.Mod(deg/45.0+0.5, 8))
	if i < 0 {
		i += 8
	}
	return dirs[i]
}

func buildAnswer(mode string) Answer {
	// auto: prioriza a janela; se vazia, cai pro mais próximo com ressalva + direção.
	if mode == "auto" {
		if ans := buildAnswer("window"); ans.Found {
			return ans
		}
		near := buildAnswer("nearest")
		if !near.Found {
			return near
		}
		near.InWindow = false
		card := ""
		if near.BearingDeg != nil {
			card = ", a " + bearingCardinal(*near.BearingDeg)
		}
		near.Speech = "Não tem nenhum avião cruzando a sua janela agora. O mais próximo é " + near.descr + card + "."
		return near
	}

	var feed struct {
		Aircraft []Aircraft `json:"aircraft"`
	}
	if err := getJSON(aircraftURL, &feed); err != nil {
		return Answer{Found: false, Speech: "Não consegui ler os dados do receptor agora."}
	}
	a := pickAircraft(feed.Aircraft, mode)
	if a == nil {
		if mode == "window" {
			return Answer{Found: false, Speech: "Não tem nenhum avião cruzando a sua janela agora."}
		}
		return Answer{Found: false, Speech: "Não tem nenhum avião no seu céu neste momento."}
	}

	callsign := cleanCallsign(a.Flight)
	desc := a.Desc
	if desc == "" {
		desc = a.T
	}
	if desc == "" {
		desc = "aeronave"
	}
	if isAllUpper(desc) {
		desc = titleCase(desc)
	}

	var route routeInfo
	if callsign != "" {
		route = lookupRoute(callsign)
	}

	ans := Answer{Found: true, Callsign: callsign, Registration: a.R, Type: a.T, TypeDesc: desc,
		Airline: route.Airline, Origin: route.Origin, Destination: route.Destination}
	if mode == "window" {
		ans.InWindow = true
	}
	if a.RDst != nil {
		dr := math.Round(*a.RDst*10) / 10
		ans.DistanceKm = &dr
	}
	if a.RDir != nil {
		b := math.Round(*a.RDir)
		ans.BearingDeg = &b
	}
	if f, ok := a.altFeet(); ok {
		altM := int(math.Round(f*ftToM/50)) * 50
		ans.AltitudeM = &altM
	}
	if elev, ok := a.elevationDeg(); ok {
		e := math.Round(elev*10) / 10
		ans.ElevationDeg = &e
	}

	ans.descr = descriptive(callsign, desc, route, ans.DistanceKm, ans.AltitudeM)
	ans.Speech = "Esse é " + ans.descr + "."
	return ans
}

// ── Home Assistant: REST + announcer via WebSocket ───────────────────────────
func haCall(domain, service string, payload map[string]any) error {
	if haURL == "" || haToken == "" {
		return nil
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, haURL+"/api/services/"+domain+"/"+service, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+haToken)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

func speakCurrentFlight() {
	ans := buildAnswer(announceMode)
	msg := ans.Speech
	if msg == "" {
		msg = "Não consegui identificar o voo."
	}
	log.Printf("[announcer] falando: %s", msg)
	data := map[string]any{
		"entity_id":              ttsEntity,
		"media_player_entity_id": mediaPlayer,
		"message":                msg,
	}
	if ttsLang != "" { // alguns motores (ex.: Piper) dão 500 se receberem language
		data["language"] = ttsLang
	}
	if ttsVoice != "" {
		data["options"] = map[string]any{"voice": ttsVoice}
	}
	if err := haCall("tts", "speak", data); err != nil {
		log.Printf("[announcer] tts erro: %v", err)
	}
}

func wsURL() string {
	u := strings.Replace(haURL, "http://", "ws://", 1)
	u = strings.Replace(u, "https://", "wss://", 1)
	return u + "/api/websocket"
}

func readTimeout(parent context.Context, c *websocket.Conn, v any, d time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	return wsjson.Read(ctx, c, v)
}

func announceOnce(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer wsConnected.Store(false) // qualquer saída desta função = WS caiu

	c, _, err := websocket.Dial(ctx, wsURL(), nil)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20)

	var m map[string]any
	if err := readTimeout(ctx, c, &m, 15*time.Second); err != nil { // auth_required
		return err
	}
	if err := wsjson.Write(ctx, c, map[string]any{"type": "auth", "access_token": haToken}); err != nil {
		return err
	}
	if err := readTimeout(ctx, c, &m, 15*time.Second); err != nil {
		return err
	}
	if m["type"] != "auth_ok" {
		return fmt.Errorf("auth falhou: %v", m["type"])
	}
	log.Println("[announcer] conectado ao HA")

	// cria o gatilho (ignora erro se já existir)
	if err := wsjson.Write(ctx, c, map[string]any{"id": 1, "type": "input_boolean/create", "name": triggerName}); err != nil {
		return err
	}
	_ = readTimeout(ctx, c, &m, 10*time.Second)

	// assina: input_boolean.perguntar_voo -> on
	if err := wsjson.Write(ctx, c, map[string]any{"id": 2, "type": "subscribe_trigger",
		"trigger": map[string]any{"platform": "state", "entity_id": triggerBool, "to": "on"}}); err != nil {
		return err
	}
	if err := readTimeout(ctx, c, &m, 10*time.Second); err != nil {
		return err
	}
	log.Printf("[announcer] ouvindo %s", triggerBool)
	wsConnected.Store(true) // autenticado e inscrito -> pronto

	// pinger: detecta conexão morta e força reconexão
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				pcancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		var ev map[string]any
		if err := wsjson.Read(ctx, c, &ev); err != nil {
			return err
		}
		if ev["type"] == "event" {
			speakCurrentFlight()
			if err := haCall("input_boolean", "turn_off", map[string]any{"entity_id": triggerBool}); err != nil {
				log.Printf("[announcer] turn_off erro: %v", err)
			}
		}
	}
}

func announcerLoop(ctx context.Context) {
	for {
		if err := announceOnce(ctx); err != nil {
			log.Printf("[announcer] reconectando (%v)", err)
		}
		time.Sleep(5 * time.Second)
	}
}

// ── HTTP server ──────────────────────────────────────────────────────────────
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// healthCheck é usado pelo HEALTHCHECK do Docker (imagem distroless não tem shell).
func healthCheck() {
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://localhost:" + port + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		healthCheck()
	}

	if haToken != "" {
		go announcerLoop(context.Background())
	} else {
		log.Println("[announcer] desativado (HA_TOKEN não setado)")
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "announcer": haToken != ""})
	})
	// /ready = readiness: 200 só quando o WS com o HA está vivo. Pega a
	// desconexão silenciosa do announcer (o /healthz não enxerga isso).
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		connected := wsConnected.Load()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if connected {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 -> uptime-kuma marca DOWN
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(map[string]any{"ready": connected, "ha_connected": connected, "announcer_enabled": haToken != ""})
	})
	http.HandleFunc("/overhead", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "nearest"
		}
		writeJSON(w, buildAnswer(mode))
	})

	log.Printf("flight-api  ->  http://0.0.0.0:%s/overhead", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
