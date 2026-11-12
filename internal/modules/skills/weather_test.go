package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// WeatherForecastToolTests: the weather_forecast tool against a stubbed
// Open-Meteo. No network: every request goes through stubHTTP. The cases that
// need the stored fix and the Location consent row are in db_test.go.

// stubReply is one queued answer: a body with 200, a bare status, or a failure.
type stubReply struct {
	status int
	body   string
	fail   bool
}

func okReply(body string) stubReply { return stubReply{status: http.StatusOK, body: body} }

// stubHTTP answers every request with the next queued reply (500 when none is
// left) and records the URLs asked for.
type stubHTTP struct {
	mu      sync.Mutex
	replies []stubReply
	urls    []string
}

func (s *stubHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.urls = append(s.urls, req.URL.String())
	reply := stubReply{status: http.StatusInternalServerError}
	if len(s.replies) > 0 {
		reply, s.replies = s.replies[0], s.replies[1:]
	}
	if reply.fail {
		return nil, errors.New("not connected to internet")
	}
	return &http.Response{StatusCode: reply.status, Body: io.NopCloser(strings.NewReader(reply.body)), Header: http.Header{}, Request: req}, nil
}

func (s *stubHTTP) requested() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.urls...)
}

func (s *stubHTTP) client() *http.Client { return &http.Client{Transport: s} }

// forecastJSON is five dry days, then rain.
const forecastJSON = `{"latitude":38.72,"longitude":-9.14,"daily_units":{"precipitation_sum":"mm"},
 "daily":{"time":["2026-09-23","2026-09-24","2026-09-25","2026-09-26","2026-09-27","2026-09-28","2026-09-29"],
          "precipitation_sum":[0.0,0.2,0.0,0.9,0.0,6.4,1.2],
          "weathercode":[0,1,2,3,0,63,80]}}`

func toolJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m
}

// WeatherForecastToolTests: "asks Open-Meteo for 7 days of daily rain and weather code".
func TestWeatherAsksOpenMeteoForSevenDaysOfRainAndWeatherCode(t *testing.T) {
	u := openMeteoURL(38.72, -9.14)
	require.True(t, strings.HasPrefix(u, "https://api.open-meteo.com/v1/forecast?"), u)
	require.Contains(t, u, "daily=precipitation_sum,weathercode")
	require.Contains(t, u, "forecast_days=7")
	require.Contains(t, u, "latitude=38.7200")
	require.Contains(t, u, "longitude=-9.1400")
	require.Contains(t, u, "timezone=auto")
	require.NotContains(t, u, "apikey")
}

// WeatherForecastToolTests: "a missing rain figure is never counted as dry".
func TestWeatherMissingRainIsNeverDry(t *testing.T) {
	zero := 0.0
	days := []forecastDay{{date: "a", precipitation: &zero}, {date: "b"}, {date: "c", precipitation: &zero}}
	out := toolJSON(t, renderForecast(days, locationFix{lat: 1, lng: 2}, "device"))
	require.InDelta(t, 1, out["dry_streak_days"], 0)
}

// WeatherForecastToolTests: "parses the renamed weather_code key too".
func TestWeatherParsesTheRenamedWeatherCodeKey(t *testing.T) {
	stub := &stubHTTP{replies: []stubReply{okReply(`{"daily":{"time":["2026-09-23"],"precipitation_sum":[2.5],"weather_code":[61]}}`)}}
	days, err := dailyForecast(context.Background(), stub.client(), 38.72, -9.14)
	require.NoError(t, err)
	rain, code := 2.5, 61
	require.Equal(t, []forecastDay{{date: "2026-09-23", precipitation: &rain, code: &code}}, days)
}

// WeatherForecastToolTests: "reads the phone's location item with string or number coordinates".
func TestWeatherReadsThePhonesLocationItem(t *testing.T) {
	result := `{"status":"ok","items":"[{\"lat\":\"38.72\",\"lng\":-9.14,\"place\":\"Lisbon\",\"at\":\"2026-09-23T06:59:00Z\"}]"}`
	fix := fixFromDeviceRead(result, time.Now())
	require.NotNil(t, fix)
	require.InDelta(t, 38.72, fix.lat, 0)
	require.InDelta(t, -9.14, fix.lng, 0)
	require.Equal(t, "Lisbon", *fix.place)
	require.Equal(t, time.Date(2026, 9, 23, 6, 59, 0, 0, time.UTC), fix.capturedAt.UTC())
}

// WeatherForecastToolTests: "an error or an out-of-range fix yields nothing".
func TestWeatherAnErrorOrOutOfRangeFixYieldsNothing(t *testing.T) {
	for _, result := range []string{
		`{"status":"error","reason":"device did not respond"}`,
		`{"status":"ok","items":"[{\"lat\":123,\"lng\":0}]"}`,
		`{"status":"ok","items":"[]"}`,
	} {
		require.Nil(t, fixFromDeviceRead(result, time.Now()), result)
	}
}
