package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jodoherty/virtweb/internal/virt"
	"github.com/jodoherty/virtweb/internal/ws"
)

// addTestServer registers a server and returns its ID.
func addTestServer(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	res, err := http.Post(ts.URL+"/api/servers", "application/json",
		jsonBody(virt.Server{Name: "alpha", User: "u", Host: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("add status = %d", res.StatusCode)
	}
	var srv virt.Server
	json.NewDecoder(res.Body).Decode(&srv)
	return srv.ID
}

// wsURL rewrites an http(s) endpoint URL to its ws(s) form.
func wsURL(httpURL string) string {
	if strings.HasPrefix(httpURL, "https://") {
		return "wss://" + httpURL[len("https://"):]
	}
	return "ws://" + httpURL[len("http://"):]
}

// dialStream dials a stream session at url (an http(s) endpoint URL).
// A pre-upgrade rejection (404/409/429/400/503) surfaces as the Dial
// error; a successful dial returns an open session.
func dialStream(t *testing.T, httpURL string, opts ...ws.Option) *ws.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := ws.Dial(ctx, wsURL(httpURL), opts...)
	if err != nil {
		t.Fatalf("dial %s: %v", httpURL, err)
	}
	return s
}

// preflight does the browser's HEAD preflight against a stream URL and
// returns the status code (200 open/ok, 409 in use, 429 capped, ...).
func preflight(t *testing.T, httpURL string) int {
	t.Helper()
	res, err := http.Head(httpURL)
	if err != nil {
		t.Fatalf("preflight %s: %v", httpURL, err)
	}
	defer res.Body.Close()
	return res.StatusCode
}
