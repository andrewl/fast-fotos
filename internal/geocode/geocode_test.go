package geocode

import (
	"context"
	"testing"
)

// TestLocationString tests formatting Location struct fields into string representation.
func TestLocationString(t *testing.T) {
	location := Location{Settlement: "Egham", Region: "Surrey", Country: "UK"}
	if actual := location.String(); actual != "Egham, Surrey, UK" {
		t.Errorf("Location.String() = %q, want %q", actual, "Egham, Surrey, UK")
	}
}

// TestLocationStringFallsBackToRegion tests Location formatting when settlement is empty.
func TestLocationStringFallsBackToRegion(t *testing.T) {
	location := Location{Region: "Orkney Islands", Country: "United Kingdom"}
	if actual := location.String(); actual != "Orkney Islands, United Kingdom" {
		t.Errorf("Location.String() = %q, want %q", actual, "Orkney Islands, United Kingdom")
	}
}

// TestPlaceIndexFindsNearestPlace tests reverse geocoding to find the closest place by coordinates.
func TestPlaceIndexFindsNearestPlace(t *testing.T) {
	index, err := NewPlaceIndex([]Place{
		{Settlement: "Egham", AdministrativeArea: "Surrey", Country: "UK", Latitude: 51.4316, Longitude: -0.5524},
		{Settlement: "London", AdministrativeArea: "Greater London", Country: "UK", Latitude: 51.5074, Longitude: -0.1278},
	})
	if err != nil {
		t.Fatalf("NewPlaceIndex() error = %v", err)
	}
	location, err := index.ReverseGeocode(context.Background(), 51.432, -0.553)
	if err != nil {
		t.Fatalf("ReverseGeocode() error = %v", err)
	}
	if location.Settlement != "Egham" {
		t.Errorf("settlement = %q, want %q", location.Settlement, "Egham")
	}
}

// TestPlaceIndexUsesPopulationAsTieBreaker tests population tie-breaking when two places share identical coordinates.
func TestPlaceIndexUsesPopulationAsTieBreaker(t *testing.T) {
	index, err := NewPlaceIndex([]Place{
		{Settlement: "Small", Population: 10, Latitude: 0, Longitude: 0},
		{Settlement: "Large", Population: 100, Latitude: 0, Longitude: 0},
	})
	if err != nil {
		t.Fatalf("NewPlaceIndex() error = %v", err)
	}
	location, err := index.ReverseGeocode(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ReverseGeocode() error = %v", err)
	}
	if location.Settlement != "Large" {
		t.Errorf("settlement = %q, want %q", location.Settlement, "Large")
	}
}

// TestPlaceIndexFallsBackToRegionBeyondSettlementDistance tests omitting settlement when distance exceeds threshold limit.
func TestPlaceIndexFallsBackToRegionBeyondSettlementDistance(t *testing.T) {
	index, err := NewPlaceIndexWithDistance([]Place{
		{Settlement: "Kirkwall", AdministrativeArea: "Orkney Islands", Country: "United Kingdom", Latitude: 58.9848, Longitude: -2.8059},
	}, 5)
	if err != nil {
		t.Fatalf("NewPlaceIndexWithDistance() error = %v", err)
	}
	location, err := index.ReverseGeocode(context.Background(), 59.084788888888895, -2.805986111111111)
	if err != nil {
		t.Fatalf("ReverseGeocode() error = %v", err)
	}
	if location.String() != "Orkney Islands, United Kingdom" {
		t.Errorf("location = %q, want %q", location.String(), "Orkney Islands, United Kingdom")
	}
}

// TestCachedProviderCachesResults verifies that duplicate coordinate queries use cached location results.
func TestCachedProviderCachesResults(t *testing.T) {
	calls := 0
	provider := ProviderFunc(func(context.Context, float64, float64) (Location, error) {
		calls++
		return Location{Settlement: "Egham"}, nil
	})
	cached, err := NewCachedProvider(provider)
	if err != nil {
		t.Fatalf("NewCachedProvider() error = %v", err)
	}
	for range 2 {
		if _, err := cached.ReverseGeocode(context.Background(), 51.43, -0.55); err != nil {
			t.Fatalf("ReverseGeocode() error = %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("provider calls = %d, want 1", calls)
	}
}

// TestReverseGeocodeRejectsInvalidCoordinates verifies coordinate validation for latitude and longitude ranges.
func TestReverseGeocodeRejectsInvalidCoordinates(t *testing.T) {
	index, err := NewPlaceIndex([]Place{{Latitude: 0, Longitude: 0}})
	if err != nil {
		t.Fatalf("NewPlaceIndex() error = %v", err)
	}
	if _, err := index.ReverseGeocode(context.Background(), 91, 0); err == nil {
		t.Fatal("ReverseGeocode() accepted invalid latitude")
	}
	if _, err := index.ReverseGeocode(context.Background(), 0, 181); err == nil {
		t.Fatal("ReverseGeocode() accepted invalid longitude")
	}
	if _, err := index.ReverseGeocode(context.Background(), 0, 0); err != nil {
		t.Fatalf("valid lookup error = %v", err)
	}
}
