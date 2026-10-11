package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	configFile             = "config.json"
	screenImagePath        = "output/kobo_sky.png"
	maxAircraftPhotoBytes  = 4 << 20
	maxAircraftPhotoPixels = 8_000_000
)

//go:embed cacert.pem
var bundledCAPEM []byte

type AppConfig struct {
	Home struct {
		Latitude  float64 `json:"lat"`
		Longitude float64 `json:"lon"`
		Label     string  `json:"label"`
	} `json:"home"`
	Kobo struct {
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		RefreshSeconds int    `json:"refresh_seconds"`
		DisplayCommand string `json:"display_command"`
	} `json:"kobo"`
	OpenSky struct {
		RadiusKM   float64 `json:"radius_km"`
		MaxFlights int     `json:"max_flights"`
	} `json:"opensky"`
}

type Flight struct {
	Callsign      string
	FlightNumber  string
	Airline       string
	AircraftType  string
	AircraftModel string
	Registration  string
	PhotoLink     string
	Origin        string
	Destination   string
	Photo         []byte
	PhotoCredit   string
	AltitudeFeet  *float64
	SpeedKMH      *float64
	DistanceKM    float64
}

type Weather struct {
	Name        string
	DistanceKM  float64
	Temperature *float64
	WindKMH     *float64
	RainMM      *float64
	Humidity    *float64
}

type Skyboard struct {
	config            AppConfig
	httpClient        *http.Client
	openSkyBase       string
	openMeteoBase     string
	adsbBase          string
	adsbdbBase        string
	commonsBase       string
	planespottersBase string
	weatherMu         sync.Mutex
	cachedWeather     *Weather
	weatherExpires    time.Time
	metadataMu        sync.Mutex
	metadataKey       string
	metadata          flightMetadataCache
	lastFlightMu      sync.RWMutex
	lastFlight        *Flight
}

type flightMetadataCache struct {
	expires       time.Time
	flightNumber  string
	airline       string
	aircraftType  string
	aircraftModel string
	origin        string
	destination   string
	photo         []byte
	photoCredit   string
	photoLink     string
}

func loadConfig() (AppConfig, error) {
	var config AppConfig
	data, err := os.ReadFile(configFile)
	if err != nil {
		return config, fmt.Errorf("read %s: %w", configFile, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parse %s: %w", configFile, err)
	}
	if config.Home.Latitude < -90 || config.Home.Latitude > 90 ||
		config.Home.Longitude < -180 || config.Home.Longitude > 180 {
		return config, errors.New("home latitude or longitude is out of range")
	}
	if config.Kobo.Width <= 0 {
		config.Kobo.Width = 600
	}
	if config.Kobo.Height <= 0 {
		config.Kobo.Height = 800
	}
	if config.Kobo.RefreshSeconds <= 0 {
		config.Kobo.RefreshSeconds = 20
	}
	if strings.TrimSpace(config.Kobo.DisplayCommand) == "" {
		config.Kobo.DisplayCommand = "./fbink"
	}
	if config.OpenSky.RadiusKM <= 0 {
		config.OpenSky.RadiusKM = 80
	}
	return config, nil
}

func NewSkyboard(config AppConfig) (*Skyboard, error) {
	if strings.TrimSpace(config.Kobo.DisplayCommand) == "" {
		config.Kobo.DisplayCommand = "./fbink"
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundledCAPEM) {
		return nil, errors.New("load bundled CA certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
	}
	return &Skyboard{
		config: config,
		httpClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		},
		openMeteoBase:     "https://api.open-meteo.com",
		adsbBase:          "https://api.adsb.lol",
		adsbdbBase:        "https://api.adsbdb.com",
		commonsBase:       "https://commons.wikimedia.org",
		planespottersBase: "https://api.planespotters.net",
	}, nil
}

func (app *Skyboard) requestJSON(method, endpoint string, form url.Values, headers map[string]string, target any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return fmt.Errorf("create API request: %w", err)
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.Header.Set("User-Agent", "KoboSkyboard/1.0 (+https://github.com/munch01/avionkobo)")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := app.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var responseError struct {
			Code             json.RawMessage `json:"error"`
			ErrorDescription string          `json:"error_description"`
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr == nil {
			_ = json.Unmarshal(body, &responseError)
		}
		details := []string{}
		code := rawString(responseError.Code)
		if code == "" {
			var nestedError struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(responseError.Code, &nestedError)
			code = nestedError.Code
		}
		if len(code) <= 64 && apiErrorCodePattern.MatchString(code) {
			details = append(details, "API error "+code)
		}
		if category := safeAPIErrorCategory(responseError.ErrorDescription); category != "" {
			details = append(details, category)
		}
		contentType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
		if contentType != "" && contentType != "application/json" {
			details = append(details, "content type "+contentType)
		}
		server := strings.TrimSpace(response.Header.Get("Server"))
		if server != "" && apiErrorCodePattern.MatchString(server) {
			details = append(details, "server "+server)
		}
		if len(details) != 0 {
			return fmt.Errorf("request %s returned HTTP %d (%s)", endpoint, response.StatusCode, strings.Join(details, ", "))
		}
		return fmt.Errorf("request %s returned HTTP %d", endpoint, response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(target); err != nil {
		return fmt.Errorf("decode response from %s: %w", endpoint, err)
	}
	return nil
}

func rawFloat(raw json.RawMessage) *float64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return &value
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func rawText(raw json.RawMessage) string {
	return strings.TrimSpace(strings.Trim(string(raw), `"`))
}

func haversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusKM = 6371
	radians := math.Pi / 180
	dLat := (lat2 - lat1) * radians
	dLon := (lon2 - lon1) * radians
	p1 := lat1 * radians
	p2 := lat2 * radians
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKM * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func (app *Skyboard) nearestFlight() (*Flight, error) {
	var response struct {
		Aircraft []struct {
			Hex       string          `json:"hex"`
			Callsign  string          `json:"flight"`
			Reg       string          `json:"r"`
			Type      string          `json:"t"`
			Latitude  *float64        `json:"lat"`
			Longitude *float64        `json:"lon"`
			Altitude  json.RawMessage `json:"alt_baro"`
			Speed     *float64        `json:"gs"`
			Seen      *float64        `json:"seen"`
		} `json:"ac"`
	}
	radiusNM := int(math.Ceil(app.config.OpenSky.RadiusKM / 1.852))
	if radiusNM < 1 {
		radiusNM = 1
	}
	endpoint := fmt.Sprintf("%s/v2/lat/%.4f/lon/%.4f/dist/%d",
		app.adsbBase,
		app.config.Home.Latitude,
		app.config.Home.Longitude,
		radiusNM,
	)
	if err := app.requestJSON(http.MethodGet, endpoint, nil, nil, &response); err != nil {
		return app.previousFlight(), err
	}

	var best *Flight
	for _, aircraft := range response.Aircraft {
		if aircraft.Latitude == nil || aircraft.Longitude == nil ||
			aircraft.Seen != nil && *aircraft.Seen > 30 {
			continue
		}
		distance := haversineKM(
			app.config.Home.Latitude,
			app.config.Home.Longitude,
			*aircraft.Latitude,
			*aircraft.Longitude,
		)
		if distance > app.config.OpenSky.RadiusKM || best != nil && distance >= best.DistanceKM {
			continue
		}

		var altitudeFeet, speedKMH *float64
		if altitude := rawFloat(aircraft.Altitude); altitude != nil {
			altitudeFeet = altitude
		} else if strings.EqualFold(rawText(aircraft.Altitude), "ground") {
			zero := float64(0)
			altitudeFeet = &zero
		}
		if aircraft.Speed != nil {
			kmh := *aircraft.Speed * 1.852
			speedKMH = &kmh
		}
		best = &Flight{
			Callsign:     strings.TrimSpace(aircraft.Callsign),
			AircraftType: aircraft.Type,
			Registration: strings.TrimSpace(aircraft.Reg),
			AltitudeFeet: altitudeFeet,
			SpeedKMH:     speedKMH,
			DistanceKM:   distance,
		}
		if aircraft.Hex != "" {
			best.Registration = strings.TrimSpace(aircraft.Reg)
		}
		if best.Callsign == "" {
			best.Callsign = "UNKNOWN"
		}
	}
	if best != nil {
		app.enrichFlight(best)
		app.lastFlightMu.Lock()
		app.lastFlight = best
		app.lastFlightMu.Unlock()
		return best, nil
	}
	return app.previousFlight(), nil
}

func (app *Skyboard) previousFlight() *Flight {
	app.lastFlightMu.RLock()
	defer app.lastFlightMu.RUnlock()
	if app.lastFlight == nil {
		return nil
	}
	flight := *app.lastFlight
	flight.Photo = append([]byte(nil), flight.Photo...)
	return &flight
}

func (app *Skyboard) enrichFlight(flight *Flight) {
	key := flight.Registration + "|" + flight.Callsign
	app.metadataMu.Lock()
	defer app.metadataMu.Unlock()

	if app.metadataKey == key && time.Now().Before(app.metadata.expires) {
		cached := app.metadata
		flight.FlightNumber = cached.flightNumber
		flight.Airline = cached.airline
		flight.AircraftType = cached.aircraftType
		flight.AircraftModel = cached.aircraftModel
		flight.Origin = cached.origin
		flight.Destination = cached.destination
		flight.Photo = cached.photo
		flight.PhotoCredit = cached.photoCredit
		flight.PhotoLink = cached.photoLink
		return
	}

	info := flightMetadataCache{expires: time.Now().Add(30 * time.Minute)}
	if flight.Callsign != "" && flight.Callsign != "UNKNOWN" {
		var response struct {
			Response struct {
				Route struct {
					CallsignIATA string `json:"callsign_iata"`
					Airline      struct {
						Name string `json:"name"`
					} `json:"airline"`
					Origin struct {
						Municipality string `json:"municipality"`
						Name         string `json:"name"`
					} `json:"origin"`
					Destination struct {
						Municipality string `json:"municipality"`
						Name         string `json:"name"`
					} `json:"destination"`
				} `json:"flightroute"`
			} `json:"response"`
		}
		endpoint := app.adsbdbBase + "/v0/callsign/" + url.PathEscape(flight.Callsign)
		if err := app.requestJSON(http.MethodGet, endpoint, nil, nil, &response); err != nil {
			log.Printf("flight route lookup unavailable: %v", err)
		} else {
			info.flightNumber = normalizeFlightNumber(response.Response.Route.CallsignIATA)
			info.airline = strings.TrimSpace(response.Response.Route.Airline.Name)
			info.origin = firstNonEmpty(
				response.Response.Route.Origin.Municipality,
				response.Response.Route.Origin.Name,
			)
			info.destination = firstNonEmpty(
				response.Response.Route.Destination.Municipality,
				response.Response.Route.Destination.Name,
			)
		}
	}

	if flight.Registration != "" {
		var response struct {
			Response struct {
				Aircraft struct {
					Type         string `json:"type"`
					ICAOType     string `json:"icao_type"`
					Manufacturer string `json:"manufacturer"`
					Owner        string `json:"registered_owner"`
				} `json:"aircraft"`
			} `json:"response"`
		}
		endpoint := app.adsbdbBase + "/v0/aircraft/" + url.PathEscape(flight.Registration)
		if err := app.requestJSON(http.MethodGet, endpoint, nil, nil, &response); err != nil {
			log.Printf("aircraft details lookup unavailable: %v", err)
		} else {
			info.aircraftType = strings.TrimSpace(response.Response.Aircraft.ICAOType)
			info.airline = firstNonEmpty(info.airline, response.Response.Aircraft.Owner)
			info.aircraftModel = aircraftModel(
				response.Response.Aircraft.Manufacturer,
				response.Response.Aircraft.Type,
			)
		}
	}
	if info.aircraftModel == "" && flight.AircraftType != "" {
		info.aircraftModel = aircraftModel("", flight.AircraftType)
	}
	if flight.Registration != "" {
		var err error
		info.photo, info.photoCredit, info.photoLink, err = app.planespottersAircraftPhoto(flight.Registration)
		if err != nil {
			log.Printf("registration photo lookup unavailable: %v", err)
		}
	}
	if len(info.photo) == 0 && info.aircraftModel != "" {
		var err error
		info.photo, info.photoCredit, err = app.commonsAircraftPhoto(info.aircraftModel)
		if err != nil {
			log.Printf("free aircraft photo lookup unavailable: %v", err)
		}
	}

	flight.FlightNumber = info.flightNumber
	flight.Airline = info.airline
	flight.AircraftType = firstNonEmpty(info.aircraftType, flight.AircraftType)
	flight.AircraftModel = info.aircraftModel
	flight.Origin = info.origin
	flight.Destination = info.destination
	flight.Photo = info.photo
	flight.PhotoCredit = info.photoCredit
	flight.PhotoLink = info.photoLink
	app.metadataKey = key
	if len(info.photo) == 0 {
		info.expires = time.Now().Add(5 * time.Minute)
	}
	app.metadata = info
}

var (
	apiErrorCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	flightNumberPattern = regexp.MustCompile(`^([A-Z0-9]{2}[0-9]{1,4})[A-Z]*$`)
)

func safeAPIErrorCategory(description string) string {
	description = strings.ToLower(description)
	switch {
	case strings.Contains(description, "invalid client"),
		strings.Contains(description, "client authentication"):
		return "client credentials rejected"
	case strings.Contains(description, "invalid grant"),
		strings.Contains(description, "invalid username"),
		strings.Contains(description, "invalid password"),
		strings.Contains(description, "bad credentials"):
		return "account credentials or grant rejected"
	case strings.Contains(description, "invalid scope"),
		strings.Contains(description, "scope is invalid"),
		strings.Contains(description, "not authorized"):
		return "requested permission rejected"
	case strings.Contains(description, "unsupported grant"):
		return "password grant not accepted"
	default:
		return ""
	}
}

func normalizeFlightNumber(value string) string {
	value = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(value), " ", ""))
	match := flightNumberPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func aircraftModel(manufacturer, model string) string {
	manufacturer = strings.TrimSpace(manufacturer)
	model = strings.TrimSpace(model)
	if model == "" {
		return manufacturer
	}
	if manufacturer != "" && !strings.Contains(strings.ToLower(model), strings.ToLower(manufacturer)) {
		model = manufacturer + " " + model
	}
	upperModel := strings.ToUpper(model)
	if strings.Contains(upperModel, "A320") &&
		(strings.HasSuffix(upperModel, "N") || strings.Contains(upperModel, "NEO")) &&
		!strings.Contains(upperModel, "NEO") {
		model += " NEO"
	}
	return model
}

func (app *Skyboard) downloadAircraftPhoto(photoURL string) ([]byte, error) {
	parsed, err := url.Parse(photoURL)
	if err != nil || parsed.Scheme != "https" ||
		!approvedPhotoHost(parsed.Hostname()) {
		return nil, errors.New("aircraft photo URL is not from an approved HTTPS host")
	}
	request, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create aircraft photo request: %w", err)
	}
	request.Header.Set("User-Agent", "KoboSkyboard/1.0 (+https://github.com/munch01/avionkobo)")
	response, err := app.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download aircraft photo: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("download aircraft photo returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAircraftPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read aircraft photo: %w", err)
	}
	if len(data) > maxAircraftPhotoBytes {
		return nil, errors.New("aircraft photo exceeds size limit")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode aircraft photo: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxAircraftPhotoPixels {
		return nil, errors.New("aircraft photo dimensions exceed the supported limit")
	}
	return data, nil
}

func approvedPhotoHost(host string) bool {
	host = strings.ToLower(host)
	for _, domain := range []string{"wikimedia.org", "plnspttrs.net"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func (app *Skyboard) planespottersAircraftPhoto(registration string) ([]byte, string, string, error) {
	endpoint := app.planespottersBase + "/pub/photos/reg/" + url.PathEscape(strings.TrimSpace(registration))
	var response struct {
		Photos []struct {
			Thumbnail struct {
				Source string `json:"src"`
			} `json:"thumbnail"`
			Link         string `json:"link"`
			Photographer string `json:"photographer"`
		} `json:"photos"`
	}
	headers := map[string]string{
		"User-Agent": "KoboSkyboard/1.0 (+https://github.com/munch01/avionkobo)",
	}
	if err := app.requestJSON(http.MethodGet, endpoint, nil, headers, &response); err != nil {
		return nil, "", "", err
	}
	if len(response.Photos) == 0 {
		return nil, "", "", fmt.Errorf("no Planespotters photo found for registration %q", registration)
	}
	photo := response.Photos[0]
	photographer := strings.TrimSpace(photo.Photographer)
	if photographer == "" {
		return nil, "", "", errors.New("Planespotters photo has no photographer attribution")
	}
	link, err := url.Parse(photo.Link)
	if err != nil || link.Scheme != "https" ||
		!strings.EqualFold(link.Hostname(), "www.planespotters.net") ||
		!strings.HasPrefix(link.Path, "/photo/") {
		return nil, "", "", errors.New("Planespotters photo link is not a valid photo page")
	}
	imageData, err := app.downloadAircraftPhoto(photo.Thumbnail.Source)
	if err != nil {
		return nil, "", "", err
	}
	return imageData, "PLANESPOTTERS | © " + photographer, photo.Link, nil
}

func (app *Skyboard) commonsAircraftPhoto(model string) ([]byte, string, error) {
	var lastErr error
	for _, searchTerm := range commonsAircraftSearchTerms(model) {
		data, credit, err := app.commonsAircraftPhotoSearch(searchTerm)
		if err == nil {
			return data, credit, nil
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func commonsAircraftSearchTerms(model string) []string {
	fields := strings.Fields(model)
	if len(fields) == 0 {
		return nil
	}
	manufacturerFields := 1
	for _, candidate := range []string{
		"De Havilland Canada", "De Havilland", "British Aerospace",
		"McDonnell Douglas", "Lockheed Martin", "Aero Commander",
	} {
		candidateFields := strings.Fields(candidate)
		if len(candidateFields) <= len(fields) && strings.EqualFold(
			strings.Join(fields[:len(candidateFields)], " "),
			candidate,
		) && len(candidateFields) > manufacturerFields {
			manufacturerFields = len(candidateFields)
		}
	}
	manufacturer := strings.Join(fields[:manufacturerFields], " ")
	modelFields := fields[manufacturerFields:]
	var terms []string
	addTerm := func(term string) {
		term = strings.TrimSpace(term)
		if term == "" {
			return
		}
		for _, existing := range terms {
			if strings.EqualFold(existing, term) {
				return
			}
		}
		terms = append(terms, term)
	}
	if family := aircraftFamilyName(modelFields); family != "" {
		addTerm(manufacturer + " " + family + " aircraft")
		if strings.Contains(strings.ToUpper(family), "NEO") {
			addTerm(manufacturer + " " + strings.Split(strings.ToUpper(family), "NEO")[0] + " aircraft")
		}
	}
	addTerm(model + " aircraft")
	addTerm(manufacturer + " aircraft")
	return terms
}

var aircraftFamilyPattern = regexp.MustCompile(`^([A-Za-z]*[0-9]+(-[0-9]+)?)`)

func aircraftFamilyName(modelFields []string) string {
	for i, field := range modelFields {
		upper := strings.ToUpper(strings.Trim(field, "(),"))
		if upper == "" {
			continue
		}
		switch upper {
		case "A19N":
			return "A319neo"
		case "A20N":
			return "A320neo"
		case "A21N":
			return "A321neo"
		case "B38M", "B39M":
			return "737 MAX"
		case "B738":
			return "737-800"
		}
		if strings.HasPrefix(upper, "737") && strings.Contains(upper, "NG") &&
			i+1 < len(modelFields) {
			if series := leadingDigits(modelFields[i+1]); series != "" {
				return "737-" + series
			}
		}
		if upper == "737" && i+1 < len(modelFields) &&
			strings.EqualFold(modelFields[i+1], "MAX") {
			return "737 MAX"
		}
		if len(upper) >= 4 && upper[0] == 'A' &&
			upper[1] >= '0' && upper[1] <= '9' &&
			upper[2] >= '0' && upper[2] <= '9' &&
			upper[3] >= '0' && upper[3] <= '9' {
			family := upper[:4]
			if strings.Contains(upper, "NEO") || strings.HasSuffix(upper, "N") {
				return family + "neo"
			}
			return family
		}
		match := aircraftFamilyPattern.FindString(upper)
		if match == "" {
			continue
		}
		if i > 0 && strings.EqualFold(modelFields[i-1], "ERJ") {
			return "ERJ " + leadingDigits(match)
		}
		return strings.TrimRight(match, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	}
	return ""
}

func leadingDigits(value string) string {
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	return value[:end]
}

type commonsMetadata struct {
	Value json.RawMessage `json:"value"`
}

type commonsImageInfo struct {
	ThumbURL   string                     `json:"thumburl"`
	Attributes map[string]commonsMetadata `json:"extmetadata"`
}

type commonsImagePage struct {
	Title     string             `json:"title"`
	Index     int                `json:"index"`
	ImageInfo []commonsImageInfo `json:"imageinfo"`
}

func commonsMetadataText(value json.RawMessage) string {
	var text string
	if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &text) != nil {
		return ""
	}
	return text
}

func (app *Skyboard) commonsAircraftPhotoSearch(searchTerm string) ([]byte, string, error) {
	query := url.Values{
		"action":       {"query"},
		"generator":    {"search"},
		"gsrsearch":    {searchTerm},
		"gsrnamespace": {"6"},
		"gsrlimit":     {"5"},
		"prop":         {"imageinfo"},
		"iiprop":       {"url|extmetadata"},
		"iiurlwidth":   {"600"},
		"format":       {"json"},
	}
	endpoint := app.commonsBase + "/w/api.php?" + query.Encode()
	var response struct {
		Query struct {
			Pages map[string]commonsImagePage `json:"pages"`
		} `json:"query"`
	}
	if err := app.requestJSON(http.MethodGet, endpoint, nil, nil, &response); err != nil {
		return nil, "", err
	}
	pages := make([]commonsImagePage, 0, len(response.Query.Pages))
	for _, page := range response.Query.Pages {
		pages = append(pages, page)
	}
	sort.Slice(pages, func(i, j int) bool {
		return pages[i].Index < pages[j].Index
	})
	var lastErr error
	for _, page := range pages {
		if len(page.ImageInfo) == 0 {
			continue
		}
		imageInfo := page.ImageInfo[0]
		if imageInfo.ThumbURL == "" {
			continue
		}
		data, err := app.downloadAircraftPhoto(imageInfo.ThumbURL)
		if err != nil {
			lastErr = err
			continue
		}
		artist := strings.TrimSpace(stripHTML(commonsMetadataText(imageInfo.Attributes["Artist"].Value)))
		license := strings.TrimSpace(stripHTML(commonsMetadataText(imageInfo.Attributes["LicenseShortName"].Value)))
		credit := "WIKIMEDIA: " + firstNonEmpty(artist, page.Title)
		if license != "" {
			credit += " / " + license
		}
		return data, credit, nil
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", fmt.Errorf("no matching freely licensed aircraft photo found for %q", searchTerm)
}

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

func stripHTML(value string) string {
	return html.UnescapeString(htmlTagPattern.ReplaceAllString(value, ""))
}

func (app *Skyboard) nearestWeather() (*Weather, error) {
	app.weatherMu.Lock()
	defer app.weatherMu.Unlock()
	if app.cachedWeather != nil && time.Now().Before(app.weatherExpires) {
		cached := *app.cachedWeather
		return &cached, nil
	}
	query := url.Values{
		"latitude":  {strconv.FormatFloat(app.config.Home.Latitude, 'f', 4, 64)},
		"longitude": {strconv.FormatFloat(app.config.Home.Longitude, 'f', 4, 64)},
		"current":   {"temperature_2m,relative_humidity_2m,precipitation,wind_speed_10m"},
		"timezone":  {"auto"},
	}
	var response struct {
		Current struct {
			Temperature      json.RawMessage `json:"temperature_2m"`
			RelativeHumidity json.RawMessage `json:"relative_humidity_2m"`
			Precipitation    json.RawMessage `json:"precipitation"`
			WindSpeed        json.RawMessage `json:"wind_speed_10m"`
		} `json:"current"`
	}
	endpoint := app.openMeteoBase + "/v1/forecast?" + query.Encode()
	if err := app.requestJSON(http.MethodGet, endpoint, nil, nil, &response); err != nil {
		return nil, err
	}
	label := strings.TrimSpace(app.config.Home.Label)
	if label == "" {
		label = "METEO PUBLIQUE"
	}
	weather := &Weather{
		Name:        label,
		Temperature: rawFloat(response.Current.Temperature),
		WindKMH:     rawFloat(response.Current.WindSpeed),
		RainMM:      rawFloat(response.Current.Precipitation),
		Humidity:    rawFloat(response.Current.RelativeHumidity),
	}
	app.cachedWeather = weather
	app.weatherExpires = time.Now().Add(10 * time.Minute)
	cached := *weather
	return &cached, nil
}

func gray(value uint8) color.Gray {
	return color.Gray{Y: value}
}

func fillRect(img *image.Gray, x1, y1, x2, y2 int, shade uint8) {
	draw.Draw(img, image.Rect(x1, y1, x2, y2), image.NewUniform(gray(shade)), image.Point{}, draw.Src)
}

func plot(img *image.Gray, x, y int, shade uint8) {
	if image.Pt(x, y).In(img.Bounds()) {
		img.SetGray(x, y, gray(shade))
	}
}

func line(img *image.Gray, x1, y1, x2, y2, thickness int, shade uint8) {
	dx := int(math.Abs(float64(x2 - x1)))
	sx := -1
	if x1 < x2 {
		sx = 1
	}
	dy := -int(math.Abs(float64(y2 - y1)))
	sy := -1
	if y1 < y2 {
		sy = 1
	}
	err := dx + dy
	radius := (thickness - 1) / 2
	for {
		for oy := -radius; oy <= radius; oy++ {
			for ox := -radius; ox <= radius; ox++ {
				plot(img, x1+ox, y1+oy, shade)
			}
		}
		if x1 == x2 && y1 == y2 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x1 += sx
		}
		if e2 <= dx {
			err += dx
			y1 += sy
		}
	}
}

func disk(img *image.Gray, cx, cy, radius int, shade uint8) {
	for y := -radius; y <= radius; y++ {
		for x := -radius; x <= radius; x++ {
			if x*x+y*y <= radius*radius {
				plot(img, cx+x, cy+y, shade)
			}
		}
	}
}

var glyphs = map[rune][7]string{
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C': {"01111", "10000", "10000", "10000", "10000", "10000", "01111"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F': {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'G': {"01111", "10000", "10000", "10111", "10001", "10001", "01111"},
	'H': {"10001", "10001", "10001", "11111", "10001", "10001", "10001"},
	'I': {"11111", "00100", "00100", "00100", "00100", "00100", "11111"},
	'J': {"00111", "00010", "00010", "00010", "10010", "10010", "01100"},
	'K': {"10001", "10010", "10100", "11000", "10100", "10010", "10001"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'M': {"10001", "11011", "10101", "10101", "10001", "10001", "10001"},
	'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'Q': {"01110", "10001", "10001", "10001", "10101", "10010", "01101"},
	'R': {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'S': {"01111", "10000", "10000", "01110", "00001", "00001", "11110"},
	'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'U': {"10001", "10001", "10001", "10001", "10001", "10001", "01110"},
	'V': {"10001", "10001", "10001", "10001", "10001", "01010", "00100"},
	'W': {"10001", "10001", "10001", "10101", "10101", "10101", "01010"},
	'X': {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
	'Y': {"10001", "10001", "01010", "00100", "00100", "00100", "00100"},
	'Z': {"11111", "00001", "00010", "00100", "01000", "10000", "11111"},
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6': {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	':': {"00000", "00100", "00100", "00000", "00100", "00100", "00000"},
	'.': {"00000", "00000", "00000", "00000", "00000", "00110", "00110"},
	',': {"00000", "00000", "00000", "00000", "00110", "00110", "00100"},
	'-': {"00000", "00000", "00000", "11111", "00000", "00000", "00000"},
	'/': {"00001", "00010", "00010", "00100", "01000", "01000", "10000"},
	'%': {"11001", "11010", "00100", "01000", "10110", "00110", "00000"},
	'(': {"00010", "00100", "01000", "01000", "01000", "00100", "00010"},
	')': {"01000", "00100", "00010", "00010", "00010", "00100", "01000"},
	'>': {"10000", "01000", "00100", "00010", "00100", "01000", "10000"},
	'?': {"01110", "10001", "00001", "00010", "00100", "00000", "00100"},
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
}

func text(img *image.Gray, x, y, scale int, value string, shade uint8) {
	for _, char := range strings.ToUpper(value) {
		glyph, exists := glyphs[char]
		if !exists {
			glyph = glyphs['?']
		}
		for row, pattern := range glyph {
			for col, pixel := range pattern {
				if pixel != '1' {
					continue
				}
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						plot(img, x+col*scale+sx, y+row*scale+sy, shade)
					}
				}
			}
		}
		x += 6 * scale
	}
}

func drawPlane(img *image.Gray, width, middleY int) {
	left, right := 40, width-35
	for _, cloud := range [][3]int{{90, 70, 38}, {450, 90, 42}, {200, 150, 46}, {360, 150, 52}} {
		disk(img, cloud[0], cloud[1], cloud[2], 245)
	}
	for _, segment := range [][4]int{
		{left, middleY + 18, right - 40, middleY + 18},
		{left + 80, middleY + 18, left + 120, middleY + 2},
		{left + 120, middleY + 2, left + 240, middleY + 2},
		{left + 240, middleY + 2, right - 60, middleY + 18},
		{right - 65, middleY + 18, right, middleY + 18},
		{left + 25, middleY + 18, left + 42, middleY + 50},
		{right - 2, middleY + 18, right + 20, middleY + 50},
		{left + 130, middleY + 18, left + 160, middleY - 35},
		{left + 160, middleY - 35, left + 240, middleY + 18},
		{left + 210, middleY + 18, left + 210, middleY - 50},
		{right - 120, middleY + 18, right - 85, middleY - 26},
		{right - 52, middleY + 18, right - 36, middleY + 8},
	} {
		thickness := 2
		if segment[1] == middleY+18 && segment[3] == middleY+18 {
			thickness = 3
		}
		line(img, segment[0], segment[1], segment[2], segment[3], thickness, 0)
	}
}

func drawPhoto(img *image.Gray, photo []byte, x1, y1, x2, y2 int) bool {
	if len(photo) == 0 || x2 <= x1 || y2 <= y1 {
		return false
	}
	source, _, err := image.Decode(bytes.NewReader(photo))
	if err != nil {
		return false
	}
	sourceBounds := source.Bounds()
	sourceWidth, sourceHeight := sourceBounds.Dx(), sourceBounds.Dy()
	targetWidth, targetHeight := x2-x1, y2-y1
	if sourceWidth <= 0 || sourceHeight <= 0 {
		return false
	}
	cropWidth, cropHeight := sourceWidth, sourceHeight
	if sourceWidth*targetHeight > sourceHeight*targetWidth {
		cropWidth = sourceHeight * targetWidth / targetHeight
	} else {
		cropHeight = sourceWidth * targetHeight / targetWidth
	}
	cropX := sourceBounds.Min.X + (sourceWidth-cropWidth)/2
	cropY := sourceBounds.Min.Y + (sourceHeight-cropHeight)/2
	for y := 0; y < targetHeight; y++ {
		sourceY := cropY + y*cropHeight/targetHeight
		for x := 0; x < targetWidth; x++ {
			sourceX := cropX + x*cropWidth/targetWidth
			pixel := color.GrayModel.Convert(source.At(sourceX, sourceY)).(color.Gray)
			img.SetGray(x1+x, y1+y, pixel)
		}
	}
	line(img, x1, y1, x2-1, y1, 1, 0)
	line(img, x1, y2-1, x2-1, y2-1, 1, 0)
	line(img, x1, y1, x1, y2-1, 1, 0)
	line(img, x2-1, y1, x2-1, y2-1, 1, 0)
	return true
}

func number(value *float64, decimals int) string {
	if value == nil {
		return "N/A"
	}
	return strconv.FormatFloat(*value, 'f', decimals, 64)
}

func shortened(value string, maxCharacters int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxCharacters || maxCharacters < 2 {
		return value
	}
	return string(runes[:maxCharacters-1]) + "?"
}

func drawWeatherIcon(img *image.Gray, kind string, centerX, topY int) {
	switch kind {
	case "temperature":
		line(img, centerX, topY+3, centerX, topY+24, 3, 0)
		line(img, centerX-4, topY+3, centerX+4, topY+3, 2, 0)
		line(img, centerX-4, topY+3, centerX-4, topY+20, 2, 0)
		line(img, centerX+4, topY+3, centerX+4, topY+20, 2, 0)
		disk(img, centerX, topY+25, 6, 0)
	case "wind":
		line(img, centerX-17, topY+7, centerX+14, topY+7, 2, 0)
		line(img, centerX+14, topY+7, centerX+19, topY+3, 2, 0)
		line(img, centerX-12, topY+15, centerX+17, topY+15, 2, 0)
		line(img, centerX-17, topY+23, centerX+8, topY+23, 2, 0)
		line(img, centerX+8, topY+23, centerX+13, topY+19, 2, 0)
	case "humidity":
		line(img, centerX, topY+1, centerX-11, topY+17, 2, 0)
		line(img, centerX, topY+1, centerX+11, topY+17, 2, 0)
		line(img, centerX-11, topY+17, centerX-7, topY+25, 2, 0)
		line(img, centerX+11, topY+17, centerX+7, topY+25, 2, 0)
		line(img, centerX-7, topY+25, centerX+7, topY+25, 2, 0)
		disk(img, centerX, topY+17, 7, 255)
	case "rain":
		line(img, centerX-13, topY+8, centerX+13, topY+8, 2, 0)
		line(img, centerX-13, topY+8, centerX-9, topY+3, 2, 0)
		line(img, centerX-9, topY+3, centerX-3, topY+3, 2, 0)
		line(img, centerX-3, topY+3, centerX, topY, 2, 0)
		line(img, centerX, topY, centerX+7, topY, 2, 0)
		line(img, centerX+7, topY, centerX+13, topY+8, 2, 0)
		for _, x := range []int{centerX - 8, centerX, centerX + 8} {
			line(img, x, topY+14, x-3, topY+22, 2, 0)
		}
	}
}

func drawFlightIcon(img *image.Gray, kind string, centerX, topY int) {
	switch kind {
	case "altitude":
		line(img, centerX, topY+23, centerX, topY+3, 2, 0)
		line(img, centerX, topY+3, centerX-8, topY+11, 2, 0)
		line(img, centerX, topY+3, centerX+8, topY+11, 2, 0)
		line(img, centerX-16, topY+26, centerX+16, topY+26, 2, 0)
		line(img, centerX-12, topY+22, centerX-12, topY+26, 2, 0)
		line(img, centerX+12, topY+22, centerX+12, topY+26, 2, 0)
	case "speed":
		line(img, centerX-17, topY+23, centerX-17, topY+15, 2, 0)
		line(img, centerX-17, topY+15, centerX-11, topY+6, 2, 0)
		line(img, centerX-11, topY+6, centerX, topY+2, 2, 0)
		line(img, centerX, topY+2, centerX+11, topY+6, 2, 0)
		line(img, centerX+11, topY+6, centerX+17, topY+15, 2, 0)
		line(img, centerX+17, topY+15, centerX+17, topY+23, 2, 0)
		line(img, centerX-17, topY+23, centerX+17, topY+23, 2, 0)
		line(img, centerX, topY+20, centerX+8, topY+10, 2, 0)
		disk(img, centerX, topY+20, 3, 0)
	}
}

func centeredText(img *image.Gray, centerX, y, scale int, value string, shade uint8) {
	text(img, centerX-len([]rune(value))*6*scale/2, y, scale, value, shade)
}

func flightDisplayLines(flight *Flight) (string, string) {
	callsign := strings.ReplaceAll(flight.Callsign, " ", "")
	company := firstNonEmpty(flight.Airline, "COMPAGNIE INCONNUE")
	flightNumber := firstNonEmpty(flight.FlightNumber, callsign)
	registration := firstNonEmpty(flight.Registration, callsign)
	model := firstNonEmpty(flight.AircraftModel, flight.AircraftType, "TYPE INCONNU")
	return company + " / " + registration + " / " + flightNumber, model
}

func renderScreen(config AppConfig, flight *Flight, weather *Weather, _ time.Time) *image.Gray {
	width, height := config.Kobo.Width, config.Kobo.Height
	img := image.NewGray(image.Rect(0, 0, width, height))
	fillRect(img, 0, 0, width, height, 255)
	fillRect(img, 0, 0, width, 230, 220)
	if flight == nil || !drawPhoto(img, flight.Photo, 0, 0, width, 230) {
		drawPlane(img, width, 145)
	}
	line(img, 0, 230, width-1, 230, 2, 0)
	if flight != nil && flight.PhotoCredit != "" {
		fillRect(img, 0, 210, width, 230, 255)
		text(img, 12, 216, 1, shortened(flight.PhotoCredit, (width-24)/6), 0)
	}

	fillRect(img, 18, 245, width-18, 565, 235)
	line(img, 18, 245, width-19, 245, 2, 0)
	line(img, 18, 564, width-19, 564, 2, 0)
	if flight != nil {
		flightSummary, model := flightDisplayLines(flight)
		route := "TRAJET INCONNU"
		if flight.Origin != "" && flight.Destination != "" {
			route = flight.Origin + " > " + flight.Destination
		}
		centeredText(img, width/2, 270, 2, shortened(flightSummary, (width-72)/12), 0)
		centeredText(img, width/2, 300, 2, shortened(model, (width-72)/12), 0)
		centeredText(img, width/2, 338, 3, shortened(route, (width-72)/18), 0)
		metricCenters := []int{width / 3, 2 * width / 3}
		metricIcons := []string{"altitude", "speed"}
		metricValues := []string{
			number(flight.AltitudeFeet, 0) + " FT",
			number(flight.SpeedKMH, 0) + " KM/H",
		}
		for i, centerX := range metricCenters {
			drawFlightIcon(img, metricIcons[i], centerX, 390)
			centeredText(img, centerX, 438, 2, metricValues[i], 0)
		}
		centeredText(img, width/2, 510, 2,
			"DIST "+strconv.FormatFloat(flight.DistanceKM, 'f', 1, 64)+" KM", 0)
	} else {
		text(img, 36, 360, 3, "AUCUN AVION DETECTE", 0)
	}

	fillRect(img, 18, 580, width-18, height-18, 230)
	line(img, 18, 580, width-19, 580, 2, 0)
	if weather != nil {
		centers := []int{width / 8, 3 * width / 8, 5 * width / 8, 7 * width / 8}
		icons := []string{"temperature", "wind", "humidity", "rain"}
		values := []string{
			number(weather.Temperature, 1) + " C",
			number(weather.WindKMH, 0) + " KM/H",
			number(weather.Humidity, 0) + "%",
			number(weather.RainMM, 1) + " MM",
		}
		for i, centerX := range centers {
			drawWeatherIcon(img, icons[i], centerX, 600)
			centeredText(img, centerX, 668, 2, values[i], 0)
		}
	} else {
		text(img, 36, 620, 3, "METEO INDISPONIBLE", 0)
	}
	return img
}

func (app *Skyboard) showScreen(flight *Flight, weather *Weather) error {
	img := renderScreen(app.config, flight, weather, time.Now())
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		return fmt.Errorf("encode screen image: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(screenImagePath), 0755); err != nil {
		return fmt.Errorf("create screen image directory: %w", err)
	}
	if err := os.WriteFile(screenImagePath, encoded.Bytes(), 0644); err != nil {
		return fmt.Errorf("write screen image: %w", err)
	}
	imagePath, err := filepath.Abs(screenImagePath)
	if err != nil {
		return fmt.Errorf("resolve screen image path: %w", err)
	}
	displayCommand := app.config.Kobo.DisplayCommand
	displayArgs := []string{"-f", "-g", imagePath}
	if strings.EqualFold(filepath.Base(displayCommand), "fbink") {
		displayArgs = []string{"-f", "-i", imagePath}
	}
	if _, err := exec.LookPath(displayCommand); err != nil {
		if !errors.Is(err, exec.ErrNotFound) ||
			!strings.EqualFold(filepath.Base(displayCommand), "eips") {
			return fmt.Errorf("find screen display command %q: %w", displayCommand, err)
		}
		displayCommand = "./fbink"
		displayArgs = []string{"-f", "-i", imagePath}
		log.Printf("eips is unavailable; falling back to FBInk in the application directory")
	}
	command := exec.Command(displayCommand, displayArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("display screen with %q: %w: %s",
			displayCommand, err, strings.TrimSpace(string(output)))
	}
	if flight == nil || len(flight.Photo) == 0 {
		app.metadataMu.Lock()
		app.metadata.photo = nil
		app.metadata.photoLink = ""
		app.metadataKey = ""
		app.metadataMu.Unlock()
	}
	return nil
}

func (app *Skyboard) run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(app.config.Kobo.RefreshSeconds) * time.Second)
	defer ticker.Stop()
	for {
		flight, err := app.nearestFlight()
		if err != nil {
			log.Printf("OpenSky flight lookup failed: %v", err)
		}
		weather, err := app.nearestWeather()
		if err != nil {
			log.Printf("Open-Meteo weather lookup failed: %v", err)
			weather = nil
		}
		if err := app.showScreen(flight, weather); err != nil {
			log.Printf("screen update failed: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Print("SkyBoard stopped.")
			return
		case <-ticker.C:
		}
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	log.Print("SkyBoard starting")
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	log.Print("SkyBoard configuration loaded")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := NewSkyboard(config)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.showScreen(nil, nil); err != nil {
		log.Fatal(err)
	}
	log.Print("Initial Kobo screen rendered")
	app.run(ctx)
}
