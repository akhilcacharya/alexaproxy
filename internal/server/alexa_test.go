package server

import (
	"errors"
	"testing"

	"github.com/akhilcacharya/alexaproxy/internal/api"
	"github.com/akhilcacharya/alexaproxy/internal/store"
)

func TestFixMojibake(t *testing.T) {
	for in, want := range map[string]string{
		"Samâ\u0080\u0099s QC35": "Sam’s QC35",
		"Kitchen":                "Kitchen",
		"Café":                   "Café", // Latin-1 range but not valid UTF-8 once narrowed
		"Sam’s":                  "Sam’s",
	} {
		if got := fixMojibake(in); got != want {
			t.Errorf("fixMojibake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindDevice(t *testing.T) {
	devs := []api.Device{
		{AccountName: "Bedroom Echo Show 8", SerialNumber: "S8"},
		{AccountName: "Kitchen Echo Show 5", SerialNumber: "S5"},
		{AccountName: "Samâ\u0080\u0099s QC35", SerialNumber: "QC"},
		{AccountName: "Echo Buds", SerialNumber: "B1"},
		{AccountName: "Echo Buds", SerialNumber: "B2"},
	}
	for q, want := range map[string]string{
		"show 8":     "S8", // case-insensitive substring
		"S5":         "S5", // serial
		"Sam’s QC35": "QC", // repaired name, exact
		"qc35":       "QC",
		"B2":         "B2", // duplicates are reachable by serial
	} {
		d, err := findDevice(devs, q)
		if err != nil || d.SerialNumber != want {
			t.Errorf("findDevice(%q) = %v, %v; want %s", q, d, err, want)
		}
	}
	for q, status := range map[string]int{"show": 409, "echo buds": 409, "garage": 404} {
		_, err := findDevice(devs, q)
		var he *httpError
		if !errors.As(err, &he) || he.status != status {
			t.Errorf("findDevice(%q) error = %v, want HTTP %d", q, err, status)
		}
	}
}

func TestLoginProblemClassification(t *testing.T) {
	for err, want := range map[error]bool{
		store.ErrNotLoggedIn: true,
		errors.New("authentication failed: token exchange failed with status 400"): true,
		errors.New("API error 401: unauthorized"):                                  true,
		errors.New("dial tcp: i/o timeout"):                                        false,
		&httpError{404, "no device matches"}:                                       false,
	} {
		if got := isLoginProblem(err); got != want {
			t.Errorf("isLoginProblem(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestAuthStateTransitions(t *testing.T) {
	a := &Alexa{Store: &store.Store{Dir: t.TempDir()}}
	if got := a.Auth().State; got != AuthNotLoggedIn {
		t.Fatalf("fresh state = %s", got)
	}
	a.Store.Save(&store.Credentials{RefreshToken: "Atnr|x"})
	if got := a.Auth().State; got != AuthUnchecked {
		t.Fatalf("with creds, before any call = %s", got)
	}
	a.record(errors.New("API error 403: forbidden"))
	if got := a.Auth(); got.State != AuthInvalid || got.OK() || got.LastError == "" {
		t.Fatalf("after auth error = %+v", got)
	}
	a.record(errors.New("network down")) // not about credentials: no change
	if got := a.Auth().State; got != AuthInvalid {
		t.Fatalf("network errors must not change state, got %s", got)
	}
	a.record(nil)
	if got := a.Auth(); got.State != AuthOK || !got.OK() {
		t.Fatalf("after success = %+v", got)
	}
	if err := a.Logout(); err != nil {
		t.Fatal(err)
	}
	if got := a.Auth().State; got != AuthNotLoggedIn {
		t.Fatalf("after logout = %s", got)
	}
}
