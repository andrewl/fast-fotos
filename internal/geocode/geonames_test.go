package geocode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadGeoNamesProvider tests parsing GeoNames provider files from a local directory.
func TestLoadGeoNamesProvider(t *testing.T) {
	directory := t.TempDir()
	writeDataset(t, directory, "admin1CodesASCII.txt", "GB.N7\tSurrey\tSurrey\t123\n")
	writeDataset(t, directory, "admin2Codes.txt", "GB.N7.001\tElmbridge\tElmbridge\t123\n")
	writeDataset(t, directory, "countryInfo.txt", "GB\tGBR\t826\tUK\tUnited Kingdom\tLondon\n")
	fields := []string{"123", "Egham", "Egham", "", "51.4316", "-0.5524", "P", "PPL", "GB", "", "N7", "001", "", "", "5000", "", "", "", ""}
	writeDataset(t, directory, "cities500.txt", strings.Join(fields, "\t")+"\n")

	provider, err := LoadGeoNamesProvider(directory)
	if err != nil {
		t.Fatalf("LoadGeoNamesProvider() error = %v", err)
	}
	location, err := provider.ReverseGeocode(context.Background(), 51.432, -0.553)
	if err != nil {
		t.Fatalf("ReverseGeocode() error = %v", err)
	}
	if location.String() != "Egham, Elmbridge, United Kingdom" {
		t.Errorf("location = %q, want %q", location.String(), "Egham, Elmbridge, United Kingdom")
	}
}

// writeDataset creates a mock dataset file with given name and text contents.
func writeDataset(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
