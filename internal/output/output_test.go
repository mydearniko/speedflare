package output

import (
	"bytes"
	"testing"

	"github.com/mydearniko/speedflare/internal/data"
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

func TestBidirectionalProgressRendererUpdatesBothLinesInPlace(t *testing.T) {
	var output bytes.Buffer
	renderer := bidirectionalProgressRenderer{writer: &output}

	renderer.render("D1", "U1", "100.00 Mbps", "50.00 Mbps")
	renderer.render("D2", "U2", "110.00 Mbps", "55.00 Mbps")
	renderer.clear()

	want := "\r\033[KD1 Download: 100.00 Mbps\n" +
		"\r\033[KU1 Upload:   50.00 Mbps" +
		"\033[1A\r\033[KD2 Download: 110.00 Mbps\n" +
		"\r\033[KU2 Upload:   55.00 Mbps" +
		"\r\033[K\033[1A\r\033[K"
	if got := output.String(); got != want {
		t.Fatalf("rendered output mismatch:\n got: %q\nwant: %q", got, want)
	}
	if renderer.rendered {
		t.Fatal("expected clear to reset the rendered state")
	}
}

func TestBidirectionalProgressRendererClearBeforeFirstRenderIsNoop(t *testing.T) {
	var output bytes.Buffer
	renderer := bidirectionalProgressRenderer{writer: &output}

	renderer.clear()

	if got := output.String(); got != "" {
		t.Fatalf("expected no output, got %q", got)
	}
}
