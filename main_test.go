package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type aircraftPhotoTransport struct {
	fallback  http.RoundTripper
	png       []byte
	requested []string
}

func (transport *aircraftPhotoTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host == "upload.wikimedia.org" {
		transport.requested = append(transport.requested, request.URL.Path)
		status := http.StatusOK
		body := transport.png
		if request.URL.Path == "/bad.png" {
			status = http.StatusNotFound
			body = []byte("not found")
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	}
	return transport.fallback.RoundTrip(request)
}

func TestDashboardListenerAcceptsConnections(t *testing.T) {
	listener, err := newDashboardListener("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("dashboard ready"))
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()

	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard HTTP status = %d, want 200", response.StatusCode)
	}
}

func TestHaversineSamePoint(t *testing.T) {
	if got := haversineKM(45.6833, 0.2167, 45.6833, 0.2167); got != 0 {
		t.Fatalf("same-point distance = %v, want 0", got)
	}
}

func TestHaversineKnownDistance(t *testing.T) {
	got := haversineKM(48.8566, 2.3522, 45.6833, 0.2167)
	if got < 380 || got > 395 {
		t.Fatalf("Paris to Ruelle-sur-Touvre distance = %.1f km, want about 388 km", got)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	data := `{"home":{"lat":45.6,"lon":0.2},"kobo":{},"netatmo":{},"opensky":{}}`
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	config, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Kobo.Width != 600 || config.Kobo.Height != 800 ||
		config.Kobo.RefreshSeconds != 20 {
		t.Fatalf("unexpected defaults: %+v", config.Kobo)
	}
}

func TestLoadConfigRejectsInvalidCoordinates(t *testing.T) {
	dir := t.TempDir()
	data := `{"home":{"lat":91,"lon":0},"kobo":{},"netatmo":{},"opensky":{}}`
	if err := os.WriteFile(filepath.Join(dir, configFile), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldDir)

	if _, err := loadConfig(); err == nil {
		t.Fatal("expected out-of-range coordinates to fail")
	}
}

func TestRenderScreenProducesGrayscalePNG(t *testing.T) {
	config := AppConfig{}
	config.Kobo.Width = 600
	config.Kobo.Height = 800
	img := renderScreen(config, nil, nil, time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "screen.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoded, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Dx() != 600 || decoded.Bounds().Dy() != 800 {
		t.Fatalf("unexpected image bounds: %v", decoded.Bounds())
	}
	r, g, b, _ := decoded.At(0, 0).RGBA()
	if r != g || g != b {
		t.Fatalf("expected grayscale output, got RGB(%d, %d, %d)", r, g, b)
	}
}

func TestRawFloatHandlesNull(t *testing.T) {
	if rawFloat(json.RawMessage("null")) != nil {
		t.Fatal("null JSON value should not become a number")
	}
	value := rawFloat(json.RawMessage("12.5"))
	if value == nil || *value != 12.5 {
		t.Fatalf("parsed float = %v, want 12.5", value)
	}
}

func TestAircraftDetailsFormatting(t *testing.T) {
	if got := aircraftModel("Airbus", "A320-251N"); got != "Airbus A320-251N NEO" {
		t.Fatalf("aircraft model = %q, want Airbus A320-251N NEO", got)
	}
	for _, test := range []struct {
		model string
		want  string
	}{
		{"Airbus A320-251N NEO", "Airbus A320neo aircraft"},
		{"Boeing 737 MAX 8/W", "Boeing 737 MAX aircraft"},
		{"Boeing B38M", "Boeing 737 MAX aircraft"},
		{"Cessna 172S Skyhawk", "Cessna 172 aircraft"},
		{"Embraer ERJ 190-200", "Embraer ERJ 190 aircraft"},
		{"Embraer E175LR", "Embraer E175 aircraft"},
		{"De Havilland Canada DHC-8-400", "De Havilland Canada DHC-8-400 aircraft"},
		{"Boeing 737-800", "Boeing 737-800 aircraft"},
	} {
		t.Run(test.model, func(t *testing.T) {
			terms := commonsAircraftSearchTerms(test.model)
			for _, term := range terms {
				if term == test.want {
					return
				}
			}
			t.Fatalf("Commons search terms = %v, want %q", terms, test.want)
		})
	}
}

func TestNormalizeFlightNumberRemovesCallsignSuffix(t *testing.T) {
	for input, want := range map[string]string{
		"AF77MQ":  "AF77",
		"AF 77":   "AF77",
		"BA123A":  "BA123",
		"AFR77MQ": "",
		"AF":      "",
	} {
		if got := normalizeFlightNumber(input); got != want {
			t.Errorf("normalizeFlightNumber(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestApprovedAircraftPhotoHosts(t *testing.T) {
	for _, host := range []string{"wikimedia.org", "upload.wikimedia.org", "t.plnspttrs.net"} {
		if !approvedPhotoHost(host) {
			t.Errorf("photo host %q should be approved", host)
		}
	}
	for _, host := range []string{"airport-data.com", "image.airport-data.com", "wikimedia.org.example.net", "plnspttrs.net.example.org", "localhost"} {
		if approvedPhotoHost(host) {
			t.Errorf("photo host %q should not be approved", host)
		}
	}
}

func TestPlanespottersAircraftPhotoUsesRegistrationAndAttribution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pub/photos/reg/F-HEPJ" {
			t.Errorf("Planespotters request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "https://github.com/munch01/avionkobo") {
			t.Errorf("User-Agent must contain the application contact URL, got %q", got)
		}
		fmt.Fprint(w, `{"photos":[{"thumbnail":{"src":"https://upload.wikimedia.org/photo.png"},"link":"https://www.planespotters.net/photo/123/example","photographer":"William Verguet"}]}`)
	}))
	defer server.Close()

	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	transport := &aircraftPhotoTransport{
		fallback: client.Transport,
		png:      imageData.Bytes(),
	}
	client.Transport = transport
	app := &Skyboard{
		planespottersBase: server.URL,
		httpClient:        client,
	}

	photo, credit, link, err := app.planespottersAircraftPhoto(" F-HEPJ ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(photo, imageData.Bytes()) {
		t.Fatal("returned photo does not match the API thumbnail")
	}
	if credit != "PLANESPOTTERS | © William Verguet" {
		t.Fatalf("photo credit = %q", credit)
	}
	if link != "https://www.planespotters.net/photo/123/example" {
		t.Fatalf("photo link = %q", link)
	}
	if len(transport.requested) != 1 || transport.requested[0] != "/photo.png" {
		t.Fatalf("thumbnail requests = %v, want the API-provided URL unchanged", transport.requested)
	}
}

func TestCommonsAircraftPhotoSearchHandlesMetadataAndRetriesCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"query":{"pages":{
			"2":{"title":"File:Good aircraft","index":2,"imageinfo":[{"thumburl":"https://upload.wikimedia.org/good.png","extmetadata":{"Artist":{"value":"<a>Photo Author</a>"},"LicenseShortName":{"value":"CC BY-SA 4.0"}}}]},
			"1":{"title":"File:Retry candidate","index":1,"imageinfo":[{"thumburl":"https://upload.wikimedia.org/bad.png","extmetadata":{"Artist":{"value":123},"ExposureTime":{"value":1.5}}}]}
		}}}`)
	}))
	defer server.Close()

	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	transport := &aircraftPhotoTransport{
		fallback: client.Transport,
		png:      imageData.Bytes(),
	}
	client.Transport = transport
	app := &Skyboard{
		commonsBase: server.URL,
		httpClient:  client,
	}

	photo, credit, err := app.commonsAircraftPhotoSearch("Cessna 172 aircraft")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(photo, imageData.Bytes()) {
		t.Fatal("returned photo does not match the valid Commons candidate")
	}
	if credit != "WIKIMEDIA: Photo Author / CC BY-SA 4.0" {
		t.Fatalf("photo credit = %q", credit)
	}
	if len(transport.requested) != 2 ||
		transport.requested[0] != "/bad.png" ||
		transport.requested[1] != "/good.png" {
		t.Fatalf("candidate requests = %v, want bad then good by Commons search order", transport.requested)
	}
}

func TestNearestFlightLoadsRouteAndAircraftDetails(t *testing.T) {
	routeRequests := 0
	aircraftRequests := 0
	photoRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/lat/45.6833/lon/0.2167/dist/44":
			fmt.Fprint(w, `{"ac":[{"hex":"3991e9","flight":"AFR77MQ ","r":"F-HEPJ","t":"A320","lat":45.570451,"lon":-0.657188,"alt_baro":34000,"gs":421.7,"seen":0.1}]}`)
		case "/v0/callsign/AFR77MQ":
			routeRequests++
			fmt.Fprint(w, `{"response":{"flightroute":{"callsign_iata":"AF77MQ","airline":{"name":"Air France"},"origin":{"municipality":"Paris"},"destination":{"municipality":"New York"}}}}`)
		case "/v0/aircraft/F-HEPJ":
			aircraftRequests++
			fmt.Fprint(w, `{"response":{"aircraft":{"type":"A320-251N","icao_type":"A20N","manufacturer":"Airbus"}}}`)
		case "/pub/photos/reg/F-HEPJ":
			photoRequests++
			fmt.Fprint(w, `{"photos":[]}`)
		case "/w/api.php":
			fmt.Fprint(w, `{"query":{"pages":{}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := AppConfig{}
	config.Home.Latitude, config.Home.Longitude = 45.6833, 0.2167
	config.OpenSky.RadiusKM = 80
	app, err := NewSkyboard(config)
	if err != nil {
		t.Fatal(err)
	}
	app.httpClient = server.Client()
	app.adsbBase = server.URL
	app.adsbdbBase = server.URL
	app.commonsBase = server.URL
	app.planespottersBase = server.URL

	for i := 0; i < 2; i++ {
		flight, err := app.nearestFlight()
		if err != nil {
			t.Fatal(err)
		}
		if flight == nil {
			t.Fatal("expected a nearby flight")
		}
		if flight.Airline != "Air France" || flight.FlightNumber != "AF77" {
			t.Fatalf("airline/number = %q/%q, want Air France/AF77", flight.Airline, flight.FlightNumber)
		}
		if flight.AircraftModel != "Airbus A320-251N NEO" {
			t.Fatalf("aircraft model = %q, want Airbus A320-251N NEO", flight.AircraftModel)
		}
		if flight.Origin != "Paris" || flight.Destination != "New York" {
			t.Fatalf("route = %q to %q, want Paris to New York", flight.Origin, flight.Destination)
		}
		if got := flight.Airline + " / " + flight.Registration + " / " + flight.AircraftModel; got != "Air France / F-HEPJ / Airbus A320-251N NEO" {
			t.Fatalf("single-line flight summary = %q", got)
		}
		if flight.AltitudeFeet == nil || *flight.AltitudeFeet != 34000 {
			t.Fatalf("altitude in feet = %v, want 34000", flight.AltitudeFeet)
		}
		if flight.SpeedKMH == nil || math.Abs(*flight.SpeedKMH-780.9884) > 0.001 {
			t.Fatalf("speed in km/h = %v, want 780.9884", flight.SpeedKMH)
		}
	}
	if routeRequests != 1 || aircraftRequests != 1 || photoRequests != 1 {
		t.Fatalf("metadata requests = route %d, aircraft %d, photo %d; want one each due to caching", routeRequests, aircraftRequests, photoRequests)
	}
}

func TestNearestFlightRejectsStaleAircraftPositions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/lat/45.6833/lon/0.2167/dist/44" {
			fmt.Fprint(w, `{"ac":[{"hex":"3991e9","flight":"AFR77MQ","r":"F-HEPJ","t":"A320","lat":45.570451,"lon":-0.657188,"alt_baro":34000,"gs":421.7,"seen":45}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	config := AppConfig{}
	config.Home.Latitude, config.Home.Longitude = 45.6833, 0.2167
	config.OpenSky.RadiusKM = 80
	app, err := NewSkyboard(config)
	if err != nil {
		t.Fatal(err)
	}
	app.httpClient = server.Client()
	app.adsbBase = server.URL
	flight, err := app.nearestFlight()
	if err != nil {
		t.Fatal(err)
	}
	if flight != nil {
		t.Fatalf("stale aircraft position should be ignored, got %+v", flight)
	}
}

func TestNearestWeatherUsesOpenMeteoAndCachesResponse(t *testing.T) {
	weatherRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/unused/netatmo":
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("authorization header = %q", got)
			}
			fmt.Fprint(w, `{"body":{"devices":[{"station_name":"Home","place":{"location":[0.3,45.7]},"dashboard_data":{"Temperature":18.5,"Humidity":52},"modules":[{"module_name":"Outdoor","place":{"location":[0.2167,45.6833]},"dashboard_data":{"Temperature":16.5,"Humidity":61,"Windstrength":12,"Rain":0.4}}]}]}}`)
		case "/v1/forecast":
			weatherRequests++
			if r.URL.Query().Get("latitude") != "45.6833" ||
				r.URL.Query().Get("longitude") != "0.2167" {
				t.Errorf("Open-Meteo coordinates = %s,%s", r.URL.Query().Get("latitude"), r.URL.Query().Get("longitude"))
			}
			fmt.Fprint(w, `{"current":{"temperature_2m":16.5,"relative_humidity_2m":61,"precipitation":0.4,"wind_speed_10m":12}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := AppConfig{}
	config.Home.Latitude, config.Home.Longitude = 45.6833, 0.2167
	config.Home.Label = "Ma ville"
	app, err := NewSkyboard(config)
	if err != nil {
		t.Fatal(err)
	}
	app.httpClient = server.Client()
	app.openMeteoBase = server.URL

	for i := 0; i < 2; i++ {
		weather, err := app.nearestWeather()
		if err != nil {
			t.Fatal(err)
		}
		if weather == nil || weather.Name != "Ma ville" {
			t.Fatalf("public weather = %+v, want Ma ville", weather)
		}
		if weather.Temperature == nil || *weather.Temperature != 16.5 {
			t.Fatalf("temperature = %v, want 16.5", weather.Temperature)
		}
		if weather.WindKMH == nil || *weather.WindKMH != 12 {
			t.Fatalf("wind = %v, want 12", weather.WindKMH)
		}
		if weather.RainMM == nil || *weather.RainMM != 0.4 {
			t.Fatalf("rain = %v, want 0.4", weather.RainMM)
		}
		if weather.Humidity == nil || *weather.Humidity != 61 {
			t.Fatalf("humidity = %v, want 61", weather.Humidity)
		}
	}
	if weatherRequests != 1 {
		t.Fatalf("weather requests = %d, want one request within cache lifetime", weatherRequests)
	}
}

func TestRequestJSONReturnsStatusWithoutResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sensitive response text", http.StatusUnauthorized)
	}))
	defer server.Close()
	app, err := NewSkyboard(AppConfig{})
	if err != nil {
		t.Fatal(err)
	}
	app.httpClient = server.Client()
	var response map[string]any
	err = app.requestJSON(http.MethodGet, server.URL, nil, nil, &response)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unexpected HTTP error: %v", err)
	}
}

func TestRequestJSONReportsOnlySafeAPIErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"private response detail"}`)
	}))
	defer server.Close()
	app, err := NewSkyboard(AppConfig{})
	if err != nil {
		t.Fatal(err)
	}
	app.httpClient = server.Client()
	var response map[string]any
	err = app.requestJSON(http.MethodGet, server.URL, nil, nil, &response)
	if err == nil || !strings.Contains(err.Error(), "API error invalid_grant") ||
		strings.Contains(err.Error(), "private response detail") {
		t.Fatalf("unexpected sanitized API error: %v", err)
	}
}

func TestRequestJSONClassifiesSafeAPIErrorsWithoutLeakingDescription(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		contentType string
		server      string
		want        string
	}{
		{
			name:        "invalid credentials",
			body:        `{"error":"invalid_grant","error_description":"Invalid username or password for private-user@example.com"}`,
			contentType: "application/json",
			want:        "account credentials or grant rejected",
		},
		{
			name:        "client rejected",
			body:        `{"error":{"code":"unauthorized_client"},"error_description":"Client authentication failed for secret-value"}`,
			contentType: "application/json; charset=utf-8",
			want:        "client credentials rejected",
		},
		{
			name:        "HTML access denied",
			body:        "<html>private network information</html>",
			contentType: "text/html; charset=utf-8",
			server:      "cloudflare",
			want:        "server cloudflare",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.server != "" {
					w.Header().Set("Server", test.server)
				}
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			app, err := NewSkyboard(AppConfig{})
			if err != nil {
				t.Fatal(err)
			}
			app.httpClient = server.Client()
			var response map[string]any
			err = app.requestJSON(http.MethodGet, server.URL, nil, nil, &response)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want safe category %q", err, test.want)
			}
			if strings.Contains(err.Error(), "private-user") ||
				strings.Contains(err.Error(), "secret-value") ||
				strings.Contains(err.Error(), "private network information") {
				t.Fatalf("error exposes response contents: %v", err)
			}
		})
	}
}

func TestDashboardServesHTMLAndLatestPNG(t *testing.T) {
	config := AppConfig{}
	config.Kobo.Width = 600
	config.Kobo.Height = 800
	app, err := NewSkyboard(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.showScreen(nil, nil); err != nil {
		t.Fatal(err)
	}
	handler := app.dashboardHandler()

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), dashboardImage) {
		t.Fatalf("dashboard response status=%d body=%q", page.Code, page.Body.String())
	}

	imageResponse := httptest.NewRecorder()
	handler.ServeHTTP(imageResponse, httptest.NewRequest(http.MethodGet, dashboardImage, nil))
	if imageResponse.Code != http.StatusOK || imageResponse.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("image response status=%d headers=%v", imageResponse.Code, imageResponse.Header())
	}
	decoded, err := png.Decode(bytes.NewReader(imageResponse.Body.Bytes()))
	if err != nil {
		t.Fatalf("decode served dashboard image: %v", err)
	}
	if decoded.Bounds().Dx() != 600 || decoded.Bounds().Dy() != 800 {
		t.Fatalf("served image bounds = %v", decoded.Bounds())
	}
}

func TestDashboardPhotoLinkTracksDisplayedPhoto(t *testing.T) {
	config := AppConfig{}
	config.Kobo.Width = 600
	config.Kobo.Height = 800
	app, err := NewSkyboard(config)
	if err != nil {
		t.Fatal(err)
	}
	var photo bytes.Buffer
	if err := png.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := app.showScreen(&Flight{
		Photo:     photo.Bytes(),
		PhotoLink: "https://www.planespotters.net/photo/123/example",
	}, nil); err != nil {
		t.Fatal(err)
	}
	handler := app.dashboardHandler()
	linkResponse := httptest.NewRecorder()
	handler.ServeHTTP(linkResponse, httptest.NewRequest(http.MethodGet, "/photo-link", nil))
	var result struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(linkResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Link != "https://www.planespotters.net/photo/123/example" {
		t.Fatalf("displayed photo link = %q", result.Link)
	}
	if !strings.Contains(string(dashboardHTML), "PHOTO + CRÉDIT") {
		t.Fatal("dashboard does not provide a visible link to the credited photo")
	}

	if err := app.showScreen(nil, nil); err != nil {
		t.Fatal(err)
	}
	linkResponse = httptest.NewRecorder()
	handler.ServeHTTP(linkResponse, httptest.NewRequest(http.MethodGet, "/photo-link", nil))
	if err := json.Unmarshal(linkResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Link != "" {
		t.Fatalf("photo link after photo is no longer displayed = %q, want empty", result.Link)
	}
}
