package pveapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueryStorageRejectsInvalidStatus(t *testing.T) {
	const valid = `{"type":"dir","content":"images","enabled":1,"active":1,"total":100,"used":0,"avail":0}`
	for _, tc := range []struct {
		name, data string
		status     int
	}{
		{"forbidden", `"` + fakeSecret + `"`, http.StatusForbidden},
		{"wrong shape", `[]`, http.StatusOK},
		{"missing type", strings.Replace(valid, `"type":"dir",`, "", 1), http.StatusOK},
		{"blank type", strings.Replace(valid, `"dir"`, `" "`, 1), http.StatusOK},
		{"missing enabled", strings.Replace(valid, `"enabled":1,`, "", 1), http.StatusOK},
		{"missing active", strings.Replace(valid, `"active":1,`, "", 1), http.StatusOK},
		{"invalid enabled", strings.Replace(valid, `"enabled":1`, `"enabled":2`, 1), http.StatusOK},
		{"invalid active", strings.Replace(valid, `"active":1`, `"active":-1`, 1), http.StatusOK},
		{"disabled zero", strings.Replace(valid, `"enabled":1`, `"enabled":0`, 1), http.StatusOK},
		{"inactive zero", strings.Replace(valid, `"active":1`, `"active":0`, 1), http.StatusOK},
		{"missing total", strings.Replace(valid, `"total":100,`, "", 1), http.StatusOK},
		{"missing used", strings.Replace(valid, `"used":0,`, "", 1), http.StatusOK},
		{"missing available", strings.Replace(valid, `,"avail":0`, "", 1), http.StatusOK},
		{"null total", strings.Replace(valid, `"total":100`, `"total":null`, 1), http.StatusOK},
		{"negative total", strings.Replace(valid, `"total":100`, `"total":-1`, 1), http.StatusOK},
		{"negative used", strings.Replace(valid, `"used":0`, `"used":-1`, 1), http.StatusOK},
		{"negative available", strings.Replace(valid, `"avail":0`, `"avail":-1`, 1), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"data":%s}`, tc.data)
			}))
			defer server.Close()
			cfg := testConfig(server.URL)
			api := &Client{httpClient: server.Client(), config: cfg}
			_, err := api.Storage(context.Background(), "worker-pve", "fast_pool")
			if err == nil {
				t.Fatal("invalid storage status accepted")
			}
			if strings.Contains(err.Error(), fakeSecret) {
				t.Error("error contains token secret")
			}
		})
	}
}

func TestQueryStorageAcceptsZeroCapacityAndOtherBackend(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"type":"dir","content":"images","enabled":1,"active":1,"total":100,"used":0,"avail":0}}`)
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	api := &Client{httpClient: server.Client(), config: cfg}
	got, err := api.Storage(context.Background(), "worker-pve", "fast_pool")
	if err != nil {
		t.Fatal(err)
	}
	want := StorageInfo{Node: "worker-pve", Storage: "fast_pool", Type: "dir", Content: "images", Enabled: true, Active: true, Total: 100, Used: 0, Avail: 0}
	if got != want {
		t.Errorf("storage = %+v, want %+v", got, want)
	}
}
