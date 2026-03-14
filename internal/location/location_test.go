package location

import (
	"math"
	"testing"

	"github.com/idanyas/speedflare/internal/data"
)

func TestResolveDisplayReferenceUsesCountryAverageWhenGeoIPMissing(t *testing.T) {
	locs := []data.Location{
		{IATA: "PRG", CCA2: "CZ", Lat: 50.0755, Lon: 14.4378},
		{IATA: "BRQ", CCA2: "CZ", Lat: 49.1951, Lon: 16.6068},
		{IATA: "FRA", CCA2: "DE", Lat: 50.1109, Lon: 8.6821},
	}

	lat, lon, ok := resolveDisplayReference(0, 0, "CZ", locs)
	if !ok {
		t.Fatal("expected a country-based fallback reference")
	}

	if math.Abs(lat-49.6353) > 0.0001 {
		t.Fatalf("unexpected latitude: got %.4f want %.4f", lat, 49.6353)
	}
	if math.Abs(lon-15.5223) > 0.0001 {
		t.Fatalf("unexpected longitude: got %.4f want %.4f", lon, 15.5223)
	}
}

func TestBuildServerChoicesUsesCountryEstimateForDisplayedDistance(t *testing.T) {
	locs := []data.Location{
		{IATA: "PRG", City: "Prague", CCA2: "CZ", Lat: 50.0755, Lon: 14.4378},
		{IATA: "FRA", City: "Frankfurt-am-Main", CCA2: "DE", Lat: 50.1109, Lon: 8.6821},
		{IATA: "VIE", City: "Vienna", CCA2: "AT", Lat: 48.2082, Lon: 16.3738},
	}
	uniqueColos := map[string]string{
		"PRG": "162.159.135.177",
		"FRA": "162.159.137.155",
		"VIE": "162.159.128.106",
	}

	servers := buildServerChoices(uniqueColos, 0, 0, "FRA", "CZ", locs)
	if len(servers) != 3 {
		t.Fatalf("unexpected server count: got %d want 3", len(servers))
	}

	if servers[0].IATA != "PRG" {
		t.Fatalf("expected PRG to be the preferred server, got %s", servers[0].IATA)
	}
	if !servers[0].HasDistance {
		t.Fatal("expected displayed distances to use the country estimate")
	}

	var fra data.Server
	for _, server := range servers {
		if server.IATA == "FRA" {
			fra = server
			break
		}
	}

	if !fra.HasDistance {
		t.Fatal("expected Frankfurt to have a displayed distance")
	}
	if fra.Distance < 300 {
		t.Fatalf("expected Frankfurt distance to be based on the CZ estimate, got %.0f km", fra.Distance)
	}

	if servers[1].IATA != "VIE" || servers[2].IATA != "FRA" {
		t.Fatalf("expected distance ordering PRG, VIE, FRA; got %s, %s, %s", servers[0].IATA, servers[1].IATA, servers[2].IATA)
	}
}

func TestBuildServerChoicesKeepsColoFallbackInternalWhenCountryUnknown(t *testing.T) {
	locs := []data.Location{
		{IATA: "FRA", City: "Frankfurt-am-Main", CCA2: "DE", Lat: 50.1109, Lon: 8.6821},
		{IATA: "AMS", City: "Amsterdam", CCA2: "NL", Lat: 52.3676, Lon: 4.9041},
	}
	uniqueColos := map[string]string{
		"FRA": "162.159.137.155",
		"AMS": "162.159.138.12",
	}

	servers := buildServerChoices(uniqueColos, 0, 0, "FRA", "PL", locs)
	if len(servers) != 2 {
		t.Fatalf("unexpected server count: got %d want 2", len(servers))
	}

	if servers[0].IATA != "FRA" {
		t.Fatalf("expected current colo to win the last-resort sort, got %s", servers[0].IATA)
	}
	for _, server := range servers {
		if server.HasDistance {
			t.Fatalf("expected no displayed distance for %s without a country or GeoIP reference", server.IATA)
		}
	}
}
