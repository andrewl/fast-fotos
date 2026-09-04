package geocode

import (
	"context"
	"fmt"
	"math"
	"sync"
)

const earthRadiusKilometres = 6371.0088
const defaultSettlementDistanceKilometres = 5

// Location is the displayable result of a reverse-geocoding lookup.
type Location struct {
	Settlement string
	Region     string
	Country    string
}

// String formats the location into a comma-separated string (e.g. "Settlement, Region, Country").
func (l Location) String() string {
	parts := make([]string, 0, 3)
	for _, part := range []string{l.Settlement, l.Region, l.Country} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return join(parts, ", ")
}

// join concatenates non-empty string parts with a separator.
func join(parts []string, separator string) string {
	result := ""
	for _, part := range parts {
		if result != "" {
			result += separator
		}
		result += part
	}
	return result
}

// Provider resolves coordinates to a human-readable location.
type Provider interface {
	ReverseGeocode(context.Context, float64, float64) (Location, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(context.Context, float64, float64) (Location, error)

// ReverseGeocode executes the underlying ProviderFunc.
func (f ProviderFunc) ReverseGeocode(ctx context.Context, latitude, longitude float64) (Location, error) {
	return f(ctx, latitude, longitude)
}

// CachedProvider adds concurrency-safe caching to another provider.
type CachedProvider struct {
	provider Provider
	mu       sync.RWMutex
	cache    map[coordinate]Location
}

// coordinate represents latitude and longitude pairs used as cache keys.
type coordinate struct {
	latitude  float64
	longitude float64
}

// NewCachedProvider initializes a new CachedProvider wrapping an existing geocoding Provider.
func NewCachedProvider(provider Provider) (*CachedProvider, error) {
	if provider == nil {
		return nil, fmt.Errorf("geocoder provider is required")
	}
	return &CachedProvider{provider: provider, cache: make(map[coordinate]Location)}, nil
}

// ReverseGeocode resolves coordinates to location details, utilizing cache for repeated queries.
func (p *CachedProvider) ReverseGeocode(ctx context.Context, latitude, longitude float64) (Location, error) {
	if err := validateCoordinates(latitude, longitude); err != nil {
		return Location{}, err
	}
	key := coordinate{latitude: latitude, longitude: longitude}
	p.mu.RLock()
	location, ok := p.cache[key]
	p.mu.RUnlock()
	if ok {
		return location, nil
	}
	location, err := p.provider.ReverseGeocode(ctx, latitude, longitude)
	if err != nil {
		return Location{}, err
	}
	p.mu.Lock()
	p.cache[key] = location
	p.mu.Unlock()
	return location, nil
}

// Place is a provider-neutral populated-place record.
type Place struct {
	Settlement         string
	AdministrativeArea string
	Country            string
	Latitude           float64
	Longitude          float64
	Population         int64
}

// PlaceIndex resolves coordinates to the nearest loaded place.
type PlaceIndex struct {
	places                  []Place
	settlementDistanceLimit float64
}

// NewPlaceIndex initializes a PlaceIndex using default settlement distance limits.
func NewPlaceIndex(places []Place) (*PlaceIndex, error) {
	return NewPlaceIndexWithDistance(places, defaultSettlementDistanceKilometres)
}

// NewPlaceIndexWithDistance initializes a PlaceIndex using a custom settlement distance limit in kilometres.
func NewPlaceIndexWithDistance(places []Place, settlementDistanceLimit float64) (*PlaceIndex, error) {
	if settlementDistanceLimit < 0 || math.IsNaN(settlementDistanceLimit) || math.IsInf(settlementDistanceLimit, 0) {
		return nil, fmt.Errorf("settlement distance limit must be non-negative and finite")
	}
	for _, place := range places {
		if err := validateCoordinates(place.Latitude, place.Longitude); err != nil {
			return nil, fmt.Errorf("place %q: %w", place.Settlement, err)
		}
	}
	copied := append([]Place(nil), places...)
	return &PlaceIndex{places: copied, settlementDistanceLimit: settlementDistanceLimit}, nil
}

// ReverseGeocode locates the nearest place in the index for given latitude and longitude coordinates.
func (i *PlaceIndex) ReverseGeocode(ctx context.Context, latitude, longitude float64) (Location, error) {
	if err := validateCoordinates(latitude, longitude); err != nil {
		return Location{}, err
	}
	if err := ctx.Err(); err != nil {
		return Location{}, err
	}
	if len(i.places) == 0 {
		return Location{}, fmt.Errorf("geocoder has no places")
	}
	nearest := i.places[0]
	nearestDistance := distance(latitude, longitude, nearest.Latitude, nearest.Longitude)
	for _, place := range i.places[1:] {
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		placeDistance := distance(latitude, longitude, place.Latitude, place.Longitude)
		if placeDistance < nearestDistance || placeDistance == nearestDistance && place.Population > nearest.Population {
			nearest = place
			nearestDistance = placeDistance
		}
	}
	location := Location{
		Settlement: nearest.Settlement,
		Region:     nearest.AdministrativeArea,
		Country:    nearest.Country,
	}
	if nearestDistance > i.settlementDistanceLimit {
		location.Settlement = ""
	}
	return location, nil
}

// validateCoordinates ensures latitude and longitude values fall within valid geographical bounds.
func validateCoordinates(latitude, longitude float64) error {
	if math.IsNaN(latitude) || math.IsInf(latitude, 0) || latitude < -90 || latitude > 90 {
		return fmt.Errorf("invalid latitude %v", latitude)
	}
	if math.IsNaN(longitude) || math.IsInf(longitude, 0) || longitude < -180 || longitude > 180 {
		return fmt.Errorf("invalid longitude %v", longitude)
	}
	return nil
}

// distance calculates the Haversine distance in kilometres between two coordinate points.
func distance(latitudeA, longitudeA, latitudeB, longitudeB float64) float64 {
	latitudeDelta := radians(latitudeB - latitudeA)
	longitudeDelta := radians(longitudeB - longitudeA)
	latitudeA = radians(latitudeA)
	latitudeB = radians(latitudeB)
	halfChord := math.Sin(latitudeDelta/2)*math.Sin(latitudeDelta/2) +
		math.Cos(latitudeA)*math.Cos(latitudeB)*math.Sin(longitudeDelta/2)*math.Sin(longitudeDelta/2)
	return earthRadiusKilometres * 2 * math.Atan2(math.Sqrt(halfChord), math.Sqrt(1-halfChord))
}

// radians converts angle degrees to radians.
func radians(degrees float64) float64 {
	return degrees * math.Pi / 180
}
