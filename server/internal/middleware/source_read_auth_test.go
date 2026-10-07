package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceReadRouteMatches(t *testing.T) {
	const runtimeID = "b3c7ce64-f43e-456f-a5db-f9858462b212"
	const commandID = "a1ed97f2-7668-4729-a7dd-15afc3c8c721"
	base := "/api/daemon/runtimes/" + runtimeID + "/work-source-commands"
	cases := []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, base, true},
		{http.MethodPost, base + "/" + commandID + "/claim", true},
		{http.MethodPost, base + "/" + commandID + "/result", true},
		{http.MethodHead, base, false},
		{http.MethodPost, base, false},
		{http.MethodGet, base + "/", false},
		{http.MethodGet, base + "/../tasks", false},
		{http.MethodGet, base + "%2f", false},
		{http.MethodGet, "/api/daemon/runtimes/" + commandID + "/work-source-commands", false},
		{http.MethodPost, base + "/" + commandID + "/claim/extra", false},
		{http.MethodPost, base + "/" + commandID + "/claim%2f", false},
		{http.MethodPost, base + "/" + commandID + "/claim/..", false},
		{http.MethodPost, base + "/a1ed97f2-7668-4729-a7dd-15afc3c8c72%31/claim", false},
		{http.MethodPost, base + "/A1ED97F2-7668-4729-A7DD-15AFC3C8C721/claim", false},
		{http.MethodPost, base + "/00000000-0000-0000-0000-000000000000/claim", false},
		{http.MethodGet, "/api/daemon/register", false},
		{http.MethodPost, base + "/" + commandID + "/start", false},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if got := sourceReadRouteMatches(req, runtimeID); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
