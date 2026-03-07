package output

import (
	"testing"

	"github.com/idanyas/speedflare/internal/data"
)

func TestFormatServerLine(t *testing.T) {
	tests := []struct {
		name   string
		server data.Server
		want   string
	}{
		{
			name: "without origin ip",
			server: data.Server{
				City:    "Prague",
				Country: "CZ",
				IATA:    "PRG",
				Lat:     50.1008,
				Lon:     14.2600,
			},
			want: "Server: Prague, CZ (PRG) [50.1008, 14.2600]",
		},
		{
			name: "with origin ip",
			server: data.Server{
				City:    "Prague",
				Country: "CZ",
				IATA:    "PRG",
				Lat:     50.1008,
				Lon:     14.2600,
				IP:      "104.16.177.1",
			},
			want: "Server: Prague, CZ (PRG) [50.1008, 14.2600] [104.16.177.1]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatServerLine(tt.server)
			if got != tt.want {
				t.Fatalf("format mismatch: got %q want %q", got, tt.want)
			}
		})
	}
}
