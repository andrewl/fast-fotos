package geocode

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GeoNamesProvider loads the GeoNames cities500, admin1, and country files.
type GeoNamesProvider struct {
	*CachedProvider
}

// LoadGeoNamesProvider loads GeoNames data from a directory. The directory may
// contain cities500.txt or cities500.zip, admin1CodesASCII.txt,
// admin2Codes.txt, and countryInfo.txt.
func LoadGeoNamesProvider(directory string) (*GeoNamesProvider, error) {
	adminAreas, err := loadAdminAreas(directory)
	if err != nil {
		return nil, err
	}
	countries, err := loadCountries(directory)
	if err != nil {
		return nil, err
	}
	admin2Areas, err := loadOptionalAdminAreas(directory, "admin2Codes.txt")
	if err != nil {
		return nil, err
	}
	places, err := loadPlaces(directory, adminAreas, admin2Areas, countries)
	if err != nil {
		return nil, err
	}
	index, err := NewPlaceIndex(places)
	if err != nil {
		return nil, fmt.Errorf("create GeoNames place index: %w", err)
	}
	cached, err := NewCachedProvider(index)
	if err != nil {
		return nil, err
	}
	return &GeoNamesProvider{CachedProvider: cached}, nil
}

// loadOptionalAdminAreas loads optional administrative division mappings (such as admin2Codes.txt).
func loadOptionalAdminAreas(directory, name string) (map[string]string, error) {
	areas := make(map[string]string)
	err := readDatasetFile(directory, name, func(scanner *bufio.Scanner) error {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 {
			return fmt.Errorf("admin area row has %d fields, want at least 2", len(fields))
		}
		areas[fields[0]] = strings.TrimSpace(fields[1])
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load GeoNames administrative areas: %w", err)
	}
	return areas, nil
}

// loadAdminAreas loads primary administrative division mappings from admin1CodesASCII.txt.
func loadAdminAreas(directory string) (map[string]string, error) {
	areas := make(map[string]string)
	err := readDatasetFile(directory, "admin1CodesASCII.txt", func(scanner *bufio.Scanner) error {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 {
			return fmt.Errorf("admin area row has %d fields, want at least 2", len(fields))
		}
		areas[fields[0]] = strings.TrimSpace(fields[1])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load GeoNames administrative areas: %w", err)
	}
	return areas, nil
}

// loadCountries parses country names and codes from countryInfo.txt.
func loadCountries(directory string) (map[string]string, error) {
	countries := make(map[string]string)
	err := readDatasetFile(directory, "countryInfo.txt", func(scanner *bufio.Scanner) error {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			return nil
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			return fmt.Errorf("country row has %d fields, want at least 5", len(fields))
		}
		countries[fields[0]] = strings.TrimSpace(fields[4])
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load GeoNames countries: %w", err)
	}
	return countries, nil
}

// loadPlaces parses populated place records from cities500.txt (or zip archive).
func loadPlaces(directory string, adminAreas, admin2Areas, countries map[string]string) ([]Place, error) {
	var places []Place
	err := readDatasetFile(directory, "cities500.txt", func(scanner *bufio.Scanner) error {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 19 {
			return fmt.Errorf("city row has %d fields, want at least 19", len(fields))
		}
		latitude, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			return fmt.Errorf("parse latitude %q: %w", fields[4], err)
		}
		longitude, err := strconv.ParseFloat(fields[5], 64)
		if err != nil {
			return fmt.Errorf("parse longitude %q: %w", fields[5], err)
		}
		population, err := strconv.ParseInt(fields[14], 10, 64)
		if err != nil {
			return fmt.Errorf("parse population %q: %w", fields[14], err)
		}
		countryCode := fields[8]
		administrativeArea := adminAreas[countryCode+"."+fields[10]]
		if area := admin2Areas[countryCode+"."+fields[10]+"."+fields[11]]; area != "" {
			administrativeArea = area
		}
		places = append(places, Place{
			Settlement:         fields[1],
			AdministrativeArea: administrativeArea,
			Country:            countries[countryCode],
			Latitude:           latitude,
			Longitude:          longitude,
			Population:         population,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load GeoNames places: %w", err)
	}
	if len(places) == 0 {
		return nil, fmt.Errorf("GeoNames places file is empty")
	}
	return places, nil
}

// readDatasetFile opens a plain text dataset file or reads it directly from a zip archive.
func readDatasetFile(directory, name string, visit func(*bufio.Scanner) error) error {
	textPath := filepath.Join(directory, name)
	if file, err := os.Open(textPath); err == nil {
		defer file.Close()
		return scanDataset(file, visit)
	} else if !os.IsNotExist(err) {
		return err
	}
	zipPath := filepath.Join(directory, strings.TrimSuffix(name, ".txt")+".zip")
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open %s or %s: %w", textPath, zipPath, err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if filepath.Base(entry.Name) != name {
			continue
		}
		file, err := entry.Open()
		if err != nil {
			return err
		}
		err = scanDataset(file, visit)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	return fmt.Errorf("%s not found in %s", name, zipPath)
}

// scanDataset reads line-by-line from a dataset reader using an enlarged scanner buffer.
func scanDataset(reader io.Reader, visit func(*bufio.Scanner) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		if err := visit(scanner); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// ReverseGeocode resolves latitude and longitude coordinates using the underlying cached GeoNames index.
func (p *GeoNamesProvider) ReverseGeocode(ctx context.Context, latitude, longitude float64) (Location, error) {
	return p.CachedProvider.ReverseGeocode(ctx, latitude, longitude)
}
