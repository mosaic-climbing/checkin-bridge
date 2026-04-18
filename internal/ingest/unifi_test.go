package ingest

import (
	"testing"

	"github.com/mosaic-climbing/checkin-bridge/internal/unifi"
)

func TestFirstName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Alice Smith", "Alice"},
		{"Bob", "Bob"},
		{"", ""},
		{"Carol Ann Jones", "Carol"},
	}
	for _, tt := range tests {
		if got := firstName(tt.input); got != tt.want {
			t.Errorf("firstName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLastName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Alice Smith", "Smith"},
		{"Bob", ""},
		{"", ""},
		{"Carol Ann Jones", "Ann Jones"},
	}
	for _, tt := range tests {
		if got := lastName(tt.input); got != tt.want {
			t.Errorf("lastName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseUniFiName(t *testing.T) {
	tests := []struct {
		user        unifi.UniFiUser
		wantFirst   string
		wantLast    string
	}{
		{unifi.UniFiUser{FirstName: "Alice", LastName: "Smith"}, "Alice", "Smith"},
		{unifi.UniFiUser{Name: "Bob Jones"}, "Bob", "Jones"},
		{unifi.UniFiUser{Name: "Carol"}, "Carol", ""},
		{unifi.UniFiUser{Name: "Carol Ann Jones"}, "Carol", "Jones"},
		{unifi.UniFiUser{}, "", ""},
	}
	for _, tt := range tests {
		first, last := parseUniFiName(tt.user)
		if first != tt.wantFirst || last != tt.wantLast {
			t.Errorf("parseUniFiName(%+v) = (%q, %q), want (%q, %q)",
				tt.user, first, last, tt.wantFirst, tt.wantLast)
		}
	}
}
